package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/joho/godotenv"
	"github.com/robfig/cron/v3"
)

type Config struct {
	DiscordToken     string
	ChannelID        string
	TargetBotIDs     []string          // Multiple bot IDs
	TopGGToken       string            // Optional: for top.gg API
	NotificationTime string            // Cron format or time like "09:00"
	CustomWebhooks   map[string]string // Bot ID -> Webhook URL for custom stats endpoints
	BotTokens        map[string]string // Bot ID -> Bot Token for direct API access
}

type TopGGStats struct {
	ServerCount int `json:"server_count"`
	ShardCount  int `json:"shard_count"`
}

type BotStats struct {
	BotID       string
	BotName     string
	ServerCount int
	Error       error
}

var (
	config  Config
	session *discordgo.Session
)

func main() {
	// Load environment variables
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found, using environment variables")
	}

	// Load configuration
	targetBotIDs := os.Getenv("TARGET_BOT_IDS")
	if targetBotIDs == "" {
		// Fallback to single bot ID for backward compatibility
		targetBotIDs = os.Getenv("TARGET_BOT_ID")
	}

	var botIDs []string
	if targetBotIDs != "" {
		// Split by comma and trim spaces
		for _, id := range strings.Split(targetBotIDs, ",") {
			trimmedID := strings.TrimSpace(id)
			if trimmedID != "" {
				botIDs = append(botIDs, trimmedID)
			}
		}
	}

	// Parse custom webhooks (format: BOT_ID:WEBHOOK_URL,BOT_ID:WEBHOOK_URL)
	customWebhooks := make(map[string]string)
	if webhooks := os.Getenv("CUSTOM_WEBHOOKS"); webhooks != "" {
		for _, webhook := range strings.Split(webhooks, ",") {
			parts := strings.Split(strings.TrimSpace(webhook), ":")
			if len(parts) == 2 {
				customWebhooks[parts[0]] = parts[1]
			}
		}
	}

	// Parse bot tokens (format: BOT_ID:TOKEN,BOT_ID:TOKEN)
	botTokens := make(map[string]string)
	if tokens := os.Getenv("BOT_TOKENS"); tokens != "" {
		log.Printf("Parsing BOT_TOKENS (length: %d)", len(tokens))
		
		// Split by comma first
		tokenPairs := strings.Split(tokens, ",")
		for i, tokenPair := range tokenPairs {
			tokenPair = strings.TrimSpace(tokenPair)
			log.Printf("Processing token pair %d: %s", i+1, tokenPair)
			
			// Find the first colon to split ID and token
			colonIndex := strings.Index(tokenPair, ":")
			if colonIndex > 0 && colonIndex < len(tokenPair)-1 {
				botID := strings.TrimSpace(tokenPair[:colonIndex])
				botToken := strings.TrimSpace(tokenPair[colonIndex+1:])
				
				if botID != "" && botToken != "" {
					botTokens[botID] = botToken
					log.Printf("Added bot token for ID: %s (token length: %d)", botID, len(botToken))
				} else {
					log.Printf("Invalid token pair: empty ID or token")
				}
			} else {
				log.Printf("Invalid token pair format: %s", tokenPair)
			}
		}
	}

	log.Printf("Configured %d bot tokens", len(botTokens))
	log.Printf("Configured %d custom webhooks", len(customWebhooks))

	config = Config{
		DiscordToken:     os.Getenv("DISCORD_TOKEN"),
		ChannelID:        os.Getenv("CHANNEL_ID"),
		TargetBotIDs:     botIDs,
		TopGGToken:       os.Getenv("TOPGG_TOKEN"),
		NotificationTime: os.Getenv("NOTIFICATION_TIME"),
		CustomWebhooks:   customWebhooks,
		BotTokens:        botTokens,
	}

	if config.DiscordToken == "" || config.ChannelID == "" || len(config.TargetBotIDs) == 0 {
		log.Fatal("Missing required environment variables: DISCORD_TOKEN, CHANNEL_ID, or TARGET_BOT_IDS")
	}

	if config.NotificationTime == "" {
		config.NotificationTime = "09:00" // Default to 9 AM
	}

	// Create Discord session
	var err error
	session, err = discordgo.New("Bot " + config.DiscordToken)
	if err != nil {
		log.Fatal("Error creating Discord session:", err)
	}

	// Register ready handler
	session.AddHandler(ready)

	// Open connection to Discord
	err = session.Open()
	if err != nil {
		log.Fatal("Error opening Discord connection:", err)
	}
	defer session.Close()

	// Setup cron job for daily notifications
	setupDailyNotification()

	// Wait for interrupt signal
	fmt.Println("Bot is running. Press CTRL+C to exit.")
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt)
	<-sc
}

func ready(s *discordgo.Session, event *discordgo.Ready) {
	log.Printf("Logged in as: %v#%v", s.State.User.Username, s.State.User.Discriminator)

	// Send initial notification
	go checkAndNotifyServerCount()
}

func setupDailyNotification() {
	c := cron.New()

	// Convert time to cron expression if it's in HH:MM format
	cronExpr := config.NotificationTime
	if len(config.NotificationTime) == 5 && config.NotificationTime[2] == ':' {
		// Convert HH:MM to cron format
		hour := config.NotificationTime[:2]
		minute := config.NotificationTime[3:]
		cronExpr = fmt.Sprintf("%s %s * * *", minute, hour)
	}

	_, err := c.AddFunc(cronExpr, checkAndNotifyServerCount)
	if err != nil {
		log.Fatal("Error setting up cron job:", err)
	}

	c.Start()
	log.Printf("Daily notification scheduled at: %s", config.NotificationTime)
}

func checkAndNotifyServerCount() {
	var allStats []BotStats

	// Fetch stats for all configured bots
	for _, botID := range config.TargetBotIDs {
		stats := BotStats{
			BotID: botID,
		}

		// Try to get bot name
		user, err := session.User(botID)
		if err == nil {
			stats.BotName = user.Username
		} else {
			stats.BotName = "Unknown"
		}

		// Get server count
		count, err := getServerCount(botID)
		if err != nil {
			stats.Error = err
			log.Printf("Error fetching server count for bot %s: %v", botID, err)
		} else {
			stats.ServerCount = count
		}

		allStats = append(allStats, stats)
	}

	sendServerCountNotification(allStats)
}

func getServerCount(botID string) (int, error) {
	log.Printf("Fetching server count for bot %s", botID)

	// Method 1: Try custom webhook if configured
	if webhookURL, exists := config.CustomWebhooks[botID]; exists {
		log.Printf("Trying custom webhook for bot %s: %s", botID, webhookURL)
		count, err := getServerCountFromCustomWebhook(botID, webhookURL)
		if err == nil {
			log.Printf("Successfully got count from custom webhook for bot %s: %d", botID, count)
			return count, nil
		}
		log.Printf("Failed to get count from custom webhook for bot %s: %v", botID, err)
	}

	// Method 2: Try direct Discord API if bot token is available
	if token, exists := config.BotTokens[botID]; exists {
		log.Printf("Trying Discord API for bot %s", botID)
		count, err := getServerCountFromDiscordAPI(botID, token)
		if err == nil {
			log.Printf("Successfully got count from Discord API for bot %s: %d", botID, count)
			return count, nil
		}
		log.Printf("Failed to get count from Discord API for bot %s: %v", botID, err)
	} else {
		log.Printf("No bot token configured for bot %s", botID)
	}

	// Method 3: Try top.gg API if token is available
	if config.TopGGToken != "" {
		log.Printf("Trying top.gg API for bot %s", botID)
		count, err := getServerCountFromTopGG(botID)
		if err == nil {
			log.Printf("Successfully got count from top.gg for bot %s: %d", botID, count)
			return count, nil
		}
		log.Printf("Failed to get count from top.gg for bot %s: %v", botID, err)
	}

	// Method 4: Try Discord Bot List API (doesn't require authentication)
	log.Printf("Trying Discord Bot List API for bot %s", botID)
	count, err := getServerCountFromDBL(botID)
	if err == nil {
		log.Printf("Successfully got count from DBL for bot %s: %d", botID, count)
		return count, nil
	}
	log.Printf("Failed to get count from DBL for bot %s: %v", botID, err)

	// Method 5: If the bot is in the same server, try to get it directly
	// This only works if this monitoring bot is in the same servers
	log.Printf("Trying direct method for bot %s", botID)
	count, err = getServerCountDirectly(botID)
	if err == nil {
		log.Printf("Successfully got count from direct method for bot %s: %d", botID, count)
		return count, nil
	}
	log.Printf("Failed to get count from direct method for bot %s: %v", botID, err)

	return 0, fmt.Errorf("could not fetch server count from any source")
}

func getServerCountFromTopGG(botID string) (int, error) {
	url := fmt.Sprintf("https://top.gg/api/bots/%s/stats", botID)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return 0, err
	}

	req.Header.Set("Authorization", config.TopGGToken)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("top.gg API returned status %d: %s", resp.StatusCode, string(body))
	}

	var stats TopGGStats
	if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
		return 0, err
	}

	return stats.ServerCount, nil
}

func getServerCountFromDBL(botID string) (int, error) {
	// Discord Bot List API (discordbotlist.com)
	url := fmt.Sprintf("https://discordbotlist.com/api/v1/bots/%s/stats", botID)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("DBL API returned status %d", resp.StatusCode)
	}

	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, err
	}

	if guilds, ok := result["guilds"].(float64); ok {
		return int(guilds), nil
	}

	return 0, fmt.Errorf("could not parse guild count from DBL response")
}

func getServerCountDirectly(botID string) (int, error) {
	// This method only works if the monitoring bot can see the target bot
	// It's limited and won't give accurate results

	guilds := session.State.Guilds
	count := 0

	for _, guild := range guilds {
		for _, member := range guild.Members {
			if member.User.ID == botID {
				count++
				break
			}
		}
	}

	if count == 0 {
		return 0, fmt.Errorf("target bot not found in any mutual servers")
	}

	// This is just the count of mutual servers, not total
	return count, fmt.Errorf("only mutual servers counted (not total)")
}

func getServerCountFromCustomWebhook(_, webhookURL string) (int, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(webhookURL)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("webhook returned status %d", resp.StatusCode)
	}

	// Try to parse different response formats
	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, err
	}

	// Common field names for server count
	possibleFields := []string{"server_count", "serverCount", "guilds", "guild_count", "guildCount", "servers"}
	for _, field := range possibleFields {
		if val, ok := result[field]; ok {
			switch v := val.(type) {
			case float64:
				return int(v), nil
			case int:
				return v, nil
			case string:
				var count int
				if _, err := fmt.Sscanf(v, "%d", &count); err == nil {
					return count, nil
				}
			}
		}
	}

	return 0, fmt.Errorf("could not find server count in webhook response")
}

func getServerCountFromDiscordAPI(_, token string) (int, error) {
	// Create a temporary session for the bot
	botSession, err := discordgo.New("Bot " + token)
	if err != nil {
		return 0, fmt.Errorf("failed to create Discord session: %v", err)
	}

	// We don't need to open a websocket connection, just use REST API
	// Get the bot's guilds using the correct endpoint
	totalGuilds := 0
	after := ""

	for {
		// Use the correct method to get guilds
		guilds, err := botSession.UserGuilds(100, "", after)
		if err != nil {
			return 0, fmt.Errorf("failed to fetch guilds: %v", err)
		}

		if len(guilds) == 0 {
			break
		}

		totalGuilds += len(guilds)

		// If we got less than 100 guilds, we're done
		if len(guilds) < 100 {
			break
		}

		// Set the after parameter for the next request
		after = guilds[len(guilds)-1].ID
	}

	return totalGuilds, nil
}

func sendServerCountNotification(allStats []BotStats) {
	// Create fields for each bot
	var fields []*discordgo.MessageEmbedField
	var totalServers int
	var hasErrors bool

	for _, stats := range allStats {
		var fieldValue string
		if stats.Error != nil {
			fieldValue = fmt.Sprintf("❌ Error: %v", stats.Error)
			hasErrors = true
		} else {
			fieldValue = fmt.Sprintf("**%d** servers", stats.ServerCount)
			totalServers += stats.ServerCount
		}

		botDisplay := stats.BotName
		if botDisplay == "Unknown" || botDisplay == "" {
			botDisplay = stats.BotID
		}

		fields = append(fields, &discordgo.MessageEmbedField{
			Name:   botDisplay,
			Value:  fieldValue,
			Inline: true,
		})
	}

	// Add timestamp field
	fields = append(fields, &discordgo.MessageEmbedField{
		Name:   "⏰ Timestamp",
		Value:  time.Now().Format("2006-01-02 15:04:05"),
		Inline: false,
	})

	// Add total if monitoring multiple bots
	if len(allStats) > 1 {
		fields = append(fields, &discordgo.MessageEmbedField{
			Name:   "📊 Total Servers",
			Value:  fmt.Sprintf("**%d** servers across all bots", totalServers),
			Inline: false,
		})
	}

	// Determine embed color based on whether there were errors
	embedColor := 0x00ff00 // Green
	if hasErrors {
		embedColor = 0xffa500 // Orange for partial success
	}

	embed := &discordgo.MessageEmbed{
		Title:       "📊 Daily Server Count Report",
		Description: fmt.Sprintf("Monitoring %d bot(s)", len(allStats)),
		Color:       embedColor,
		Fields:      fields,
		Footer: &discordgo.MessageEmbedFooter{
			Text: "Daily Server Statistics",
		},
		Timestamp: time.Now().Format(time.RFC3339),
	}

	_, err := session.ChannelMessageSendEmbed(config.ChannelID, embed)
	if err != nil {
		log.Printf("Error sending notification: %v", err)
	} else {
		log.Printf("Successfully sent server count notification for %d bots", len(allStats))
	}
}
