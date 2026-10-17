package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	_ "time/tzdata" // embed zoneinfo so TIMEZONE works on minimal containers

	"github.com/bwmarrin/discordgo"
	"github.com/joho/godotenv"
	"github.com/robfig/cron/v3"
)

type Config struct {
	DiscordToken       string
	ChannelID          string
	TargetBotIDs       []string          // Multiple bot IDs
	TopGGToken         string            // Optional: for top.gg API
	NotificationTime   string            // Cron format or time like "09:00"
	CustomWebhooks     map[string]string // Bot ID -> Webhook URL for custom stats endpoints
	BotTokens          map[string]string // Bot ID -> Bot Token for direct API access
	Port               string
	DiscordWebhookURL  string // Discord Incoming Webhook URL for Jicoo notifications
	JicooWebhookSecret string // Jicoo webhook signature secret
	TimeZone           string // IANA name for schedules, e.g. Asia/Tokyo (default)
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
	config     Config
	session    *discordgo.Session
	apiStats   map[string]int // counts for the current calendar day
	statsMutex sync.Mutex

	// Closed at 00:00 and reported at 09:00, so the morning report always
	// covers the whole previous day.
	yesterdayStats map[string]int
	yesterdayDate  string

	location = time.Local
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
		DiscordToken:       os.Getenv("DISCORD_TOKEN"),
		ChannelID:          os.Getenv("CHANNEL_ID"),
		TargetBotIDs:       botIDs,
		TopGGToken:         os.Getenv("TOPGG_TOKEN"),
		NotificationTime:   os.Getenv("NOTIFICATION_TIME"),
		CustomWebhooks:     customWebhooks,
		BotTokens:          botTokens,
		Port:               os.Getenv("PORT"),
		DiscordWebhookURL:  os.Getenv("DISCORD_WEBHOOK_URL"),
		JicooWebhookSecret: os.Getenv("JICOO_WEBHOOK_SECRET"),
		TimeZone:           os.Getenv("TIMEZONE"),
	}

	location = loadLocation(config.TimeZone)

	if config.Port == "" {
		config.Port = "8080"
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

	// Initialize stats
	apiStats = make(map[string]int)

	// Start HTTP server
	go startHTTPServer()

	// Setup cron job for daily notifications and stats
	setupDailyNotification()

	// Setup memory cleanup routine
	//setupMemoryCleanup()

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

// loadLocation resolves the schedule timezone. Defaults to Asia/Tokyo so the
// 00:00 rollover and 09:00 report happen in Japan time even on a UTC host.
func loadLocation(name string) *time.Location {
	if name == "" {
		name = "Asia/Tokyo"
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		log.Printf("Invalid TIMEZONE %q, falling back to server local time: %v", name, err)
		return time.Local
	}
	return loc
}

func setupDailyNotification() {
	c := cron.New(cron.WithLocation(location))
	log.Printf("Schedules run in timezone %s", location)

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
	log.Printf("Daily server count notification scheduled at: %s", config.NotificationTime)

	// Close the day's counter at 00:00 (kept as "yesterday" until reported)
	_, err = c.AddFunc("0 0 * * *", rolloverApiStats)
	if err != nil {
		log.Printf("Error scheduling stats rollover: %v", err)
	}

	// Schedule stats reporting at 09:00
	_, err = c.AddFunc("0 9 * * *", reportApiStats)
	if err != nil {
		log.Printf("Error scheduling stats reporting: %v", err)
	}
}

func setupMemoryCleanup() {
	// Run memory cleanup every 30 minutes
	ticker := time.NewTicker(180 * time.Minute)

	go func() {
		for range ticker.C {
			performMemoryCleanup()
		}
	}()

	log.Println("Memory cleanup routine started (runs every 30 minutes)")
}

func performMemoryCleanup() {
	// Get memory stats before cleanup
	var m1 runtime.MemStats
	runtime.ReadMemStats(&m1)

	// Force garbage collection
	runtime.GC()

	// Return memory to OS
	debug.FreeOSMemory()

	// Get memory stats after cleanup
	var m2 runtime.MemStats
	runtime.ReadMemStats(&m2)

	log.Printf("Memory cleanup completed - Before: %d KB, After: %d KB, Freed: %d KB",
		m1.Alloc/1024,
		m2.Alloc/1024,
		(m1.Alloc-m2.Alloc)/1024,
	)
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

	// Clean up memory after processing
	runtime.GC()
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
	defer func() {
		resp.Body.Close()
		// Explicitly nil the response to help GC
		resp = nil
	}()

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

	// Method 1: Try to get bot info first to check if it's sharded
	botUser, err := botSession.User("@me")
	if err != nil {
		return 0, fmt.Errorf("failed to get bot user info: %v", err)
	}

	log.Printf("Bot user: %s (ID: %s)", botUser.Username, botUser.ID)

	// Method 2: Get recommended shard count from Discord
	gateway, err := botSession.GatewayBot()
	if err != nil {
		log.Printf("Failed to get gateway info, using REST API only: %v", err)
	} else {
		log.Printf("Recommended shards: %d", gateway.Shards)

		// If sharding is required, try with proper shard configuration
		if gateway.Shards > 1 {
			return getServerCountWithSharding(botSession, gateway.Shards)
		}
	}

	// Method 3: Try Gateway connection for non-sharded or fallback
	err = botSession.Open()
	if err != nil {
		// If sharding is required, try with minimal sharding
		if strings.Contains(err.Error(), "4011") || strings.Contains(err.Error(), "Sharding required") {
			log.Printf("Sharding required, attempting with shard configuration")
			return getServerCountWithSharding(botSession, 1)
		}
		log.Printf("Failed to open websocket connection: %v, falling back to REST API", err)
	} else {
		defer botSession.Close()

		// Wait a moment for the ready event and guild information to populate
		time.Sleep(3 * time.Second)

		// Get guild count from the session state
		guildCount := len(botSession.State.Guilds)
		log.Printf("Guild count from session state: %d", guildCount)

		if guildCount > 0 {
			return guildCount, nil
		}
	}

	// Method 4: Fallback to REST API
	log.Printf("Falling back to REST API")

	totalGuilds := 0
	after := ""

	for {
		// Use the REST API method
		guilds, err := botSession.UserGuilds(100, "", after)
		if err != nil {
			return 0, fmt.Errorf("failed to fetch guilds via REST API: %v", err)
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

	log.Printf("REST API returned %d guilds", totalGuilds)
	return totalGuilds, nil
}

func getServerCountWithSharding(botSession *discordgo.Session, recommendedShards int) (int, error) {
	log.Printf("Attempting sharded connection with %d shards", recommendedShards)

	// Set shard information
	botSession.ShardID = 0
	botSession.ShardCount = recommendedShards

	err := botSession.Open()
	if err != nil {
		return 0, fmt.Errorf("failed to open sharded connection: %v", err)
	}
	defer botSession.Close()

	// Wait for ready event and guild population
	time.Sleep(5 * time.Second)

	guildCount := len(botSession.State.Guilds)
	log.Printf("Shard 0 guild count: %d", guildCount)

	// For sharded bots, we can only get the count from shard 0
	// The real total would be across all shards, but we can't connect to all shards with one token
	// So we'll estimate or use REST API instead
	if guildCount > 0 {
		log.Printf("Got %d guilds from shard 0, but this is not the total count for sharded bot", guildCount)
	}

	// Use REST API for accurate total count with larger limit
	totalGuilds := 0
	after := ""
	maxIterations := 50 // Safety limit to prevent infinite loops
	iteration := 0

	for iteration < maxIterations {
		// Try to get more guilds per request (max is 200)
		guilds, err := botSession.UserGuilds(200, "", after)
		if err != nil {
			// If 200 fails, try with 100
			guilds, err = botSession.UserGuilds(100, "", after)
			if err != nil {
				return 0, fmt.Errorf("failed to fetch guilds via REST API in sharded mode: %v", err)
			}
		}

		if len(guilds) == 0 {
			break
		}

		totalGuilds += len(guilds)
		log.Printf("REST API iteration %d: got %d guilds, total: %d", iteration+1, len(guilds), totalGuilds)

		// If we got less than the requested amount, we're done
		if len(guilds) < 200 && len(guilds) < 100 {
			break
		}

		after = guilds[len(guilds)-1].ID
		iteration++

		// Small delay to avoid rate limiting
		time.Sleep(100 * time.Millisecond)
	}

	if iteration >= maxIterations {
		log.Printf("Warning: Reached maximum iterations (%d), there might be more guilds", maxIterations)
	}

	log.Printf("REST API in sharded mode returned %d guilds after %d iterations", totalGuilds, iteration)

	// If we still don't have the expected count, try a different approach
	if totalGuilds < 2500 { // If it seems incomplete for a large bot
		log.Printf("Guild count seems low for a sharded bot, this might be due to API limitations")
		log.Printf("Consider using a custom webhook endpoint for more accurate counts")
	}

	return totalGuilds, nil
}

func sendServerCountNotification(allStats []BotStats) {
	var message string

	message = "⏰" + time.Now().Format("2006-01-02 15:04:05")

	for _, stats := range allStats {
		var fieldValue string
		if stats.Error != nil {
			fieldValue = fmt.Sprintf("エラー: %v", stats.Error)
		} else {
			fieldValue = fmt.Sprintf("**%d**", stats.ServerCount)
		}

		botDisplay := stats.BotName
		if botDisplay == "Unknown" || botDisplay == "" {
			botDisplay = stats.BotID
		}

		message += "\n" + botDisplay + " : " + fieldValue
	}

	// messageの内容をDiscordに送信
	_, err := session.ChannelMessageSend(config.ChannelID, message)
	if err != nil {
		log.Printf("Error sending message: %v", err)
	} else {
		log.Printf("Successfully sent server count notification for %d bots", len(allStats))
	}
}

// HTTP Server and Handlers

func startHTTPServer() {
	http.HandleFunc("/api/count", handleCount)
	http.HandleFunc("/api/framer", handleFramerWebhook)
	http.HandleFunc("/api/jicoo", handleJicooWebhook)

	log.Printf("Starting HTTP server on port %s", config.Port)
	if err := http.ListenAndServe(":"+config.Port, nil); err != nil {
		log.Fatal("HTTP server error:", err)
	}
}

func handleCount(w http.ResponseWriter, r *http.Request) {
	// Allow browser beacons from any site (GET fetch, sendBeacon POST, <img>)
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	label := r.URL.Query().Get("label")
	if label == "" {
		label = "default"
	}
	if len(label) > 64 {
		label = label[:64]
	}

	statsMutex.Lock()
	apiStats[label]++
	count := apiStats[label]
	statsMutex.Unlock()

	log.Printf("API count incremented for label '%s': %d", label, count)
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "Counted: %d", count)
}

func handleFramerWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Framer requires a 2xx response immediately.
	// We read the body first to ensure we have it, then respond to Framer.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("Error reading framer webhook body: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	defer r.Body.Close()

	// Respond to Framer immediately
	w.WriteHeader(http.StatusOK)

	// Process forwarding asynchronously or synchronously
	// Since we need to forward it and reliability matters, we'll do it synchronously here
	// but after writing the header? No, `w.WriteHeader` sends the header.
	// Actually, `w.WriteHeader` is implicit if we write to `w`.
	// But to be safe and ensure Framer gets the 200, we should just forward it.
	// If forwarding fails, we should still have returned 200 to Framer?
	// The requirement says "Your webhook must return a 2xx HTTP status code to indicate successful receipt."
	// So we should return 200 even if forwarding fails, as long as WE received it.

	go forwardFramerWebhook(body)
}

func forwardFramerWebhook(body []byte) {
	// Pretty print JSON if possible
	var prettyJSON bytes.Buffer
	var messageContent string

	if err := json.Indent(&prettyJSON, body, "", "  "); err == nil {
		messageContent = fmt.Sprintf("📨 **アンケートが届きました**\n```json\n%s\n```", prettyJSON.String())
	} else {
		messageContent = fmt.Sprintf("📨 **アンケートが届きました**\n```\n%s\n```", string(body))
	}

	_, err := session.ChannelMessageSend(config.ChannelID, messageContent)
	if err != nil {
		log.Printf("Error forwarding webhook to Discord: %v", err)
	} else {
		log.Println("Successfully forwarded Framer webhook to Discord channel")
	}
}

// Jicoo webhook types: https://developer.jicoo.com/doc-355758

type JicooWebhookEvent struct {
	Event     string      `json:"event"`
	CreatedAt string      `json:"createdAt"`
	Object    JicooObject `json:"object"`
}

type JicooObject struct {
	UID          string         `json:"uid"`
	EventTypeUID string         `json:"eventTypeUid"`
	Status       string         `json:"status"`
	TimeZone     string         `json:"timeZone"`
	CreatedAt    string         `json:"createdAt"`
	UpdatedAt    string         `json:"updatedAt"`
	StartedAt    string         `json:"startedAt"`
	EndedAt      string         `json:"endedAt"`
	CancelReason *string        `json:"cancelReason"`
	CancelledAt  *string        `json:"cancelledAt"`
	CancelledBy  *string        `json:"cancelledBy"`
	Contact      JicooContact   `json:"contact"`
	Answers      []JicooAnswer  `json:"answers"`
	Tracking     JicooTracking  `json:"tracking"`
	DownloadLink *string        `json:"downloadLink"`
}

type JicooContact struct {
	Email string `json:"email"`
	Name  string `json:"name"`
}

type JicooAnswer struct {
	Question string   `json:"question"`
	Content  []string `json:"content"`
}

type JicooTracking struct {
	UTMCampaign *string `json:"utm_campaign"`
	UTMSource   *string `json:"utm_source"`
	UTMMedium   *string `json:"utm_medium"`
	UTMContent  *string `json:"utm_content"`
	UTMTerm     *string `json:"utm_term"`
}

type DiscordWebhookPayload struct {
	Content string         `json:"content,omitempty"`
	Embeds  []DiscordEmbed `json:"embeds,omitempty"`
}

type DiscordEmbed struct {
	Title       string              `json:"title,omitempty"`
	Description string              `json:"description,omitempty"`
	Color       int                 `json:"color,omitempty"`
	Fields      []DiscordEmbedField `json:"fields,omitempty"`
	Timestamp   string              `json:"timestamp,omitempty"`
	Footer      *DiscordEmbedFooter `json:"footer,omitempty"`
}

type DiscordEmbedField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline,omitempty"`
}

type DiscordEmbedFooter struct {
	Text string `json:"text"`
}

const jicooSignatureTolerance = 5 * time.Minute

func handleJicooWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("Error reading Jicoo webhook body: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	defer r.Body.Close()

	if config.JicooWebhookSecret != "" {
		signatureHeader := r.Header.Get("Jicoo-Webhook-Signature")
		if err := verifyJicooSignature(body, signatureHeader, config.JicooWebhookSecret); err != nil {
			log.Printf("Jicoo signature verification failed: %v", err)
			http.Error(w, "Invalid signature", http.StatusUnauthorized)
			return
		}
	}

	// Jicoo requires HTTP 200 on successful receipt; retry on non-2xx.
	w.WriteHeader(http.StatusOK)

	go forwardJicooWebhook(body)
}

func verifyJicooSignature(body []byte, header, secret string) error {
	if header == "" {
		return fmt.Errorf("missing Jicoo-Webhook-Signature header")
	}

	var timestamp string
	var signatures []string
	for _, part := range strings.Split(header, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "t":
			timestamp = kv[1]
		case "v1":
			signatures = append(signatures, kv[1])
		}
	}

	if timestamp == "" || len(signatures) == 0 {
		return fmt.Errorf("invalid signature header format")
	}

	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid timestamp: %w", err)
	}

	if diff := time.Since(time.Unix(ts, 0)); diff > jicooSignatureTolerance || diff < -jicooSignatureTolerance {
		return fmt.Errorf("timestamp outside tolerance: %s", timestamp)
	}

	signedPayload := timestamp + "." + string(body)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signedPayload))
	expected := hex.EncodeToString(mac.Sum(nil))

	for _, sig := range signatures {
		if subtle.ConstantTimeCompare([]byte(expected), []byte(sig)) == 1 {
			return nil
		}
	}

	return fmt.Errorf("signature mismatch")
}

func forwardJicooWebhook(body []byte) {
	var event JicooWebhookEvent
	if err := json.Unmarshal(body, &event); err != nil {
		log.Printf("Error parsing Jicoo webhook JSON: %v", err)
		return
	}

	payload := buildJicooDiscordPayload(event)
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		log.Printf("Error marshaling Discord webhook payload: %v", err)
		return
	}

	if config.DiscordWebhookURL != "" {
		if err := sendDiscordWebhook(config.DiscordWebhookURL, payloadBytes); err != nil {
			log.Printf("Error sending Jicoo notification via Discord webhook: %v", err)
			return
		}
		log.Printf("Successfully forwarded Jicoo event %s to Discord webhook", event.Event)
		return
	}

	// Fallback: send via bot to CHANNEL_ID when Discord webhook URL is not configured
	message := formatJicooPlainMessage(event)
	if _, err := session.ChannelMessageSend(config.ChannelID, message); err != nil {
		log.Printf("Error forwarding Jicoo webhook to Discord channel: %v", err)
		return
	}
	log.Printf("Successfully forwarded Jicoo event %s to Discord channel", event.Event)
}

func sendDiscordWebhook(webhookURL string, payload []byte) error {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(webhookURL, "application/json", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("discord webhook returned status %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

func jicooEventLabel(event string) (string, int) {
	switch event {
	case "guest_booked":
		return "新規予約", 0x57F287 // green
	case "guest_rescheduled":
		return "ゲストによる日程変更", 0xFEE75C // yellow
	case "host_rescheduled":
		return "ホストによる日程変更", 0xFEE75C
	case "guest_cancelled":
		return "ゲストによるキャンセル", 0xED4245 // red
	case "host_cancelled":
		return "ホストによるキャンセル", 0xED4245
	case "guest_booking_request_created":
		return "予約リクエスト作成", 0x5865F2 // blurple
	case "host_booking_request_accepted":
		return "予約リクエスト承認", 0x57F287
	case "host_booking_request_declined":
		return "予約リクエスト拒否", 0xED4245
	case "recording_ready":
		return "録画準備完了", 0xEB459E // fuchsia
	default:
		return event, 0x99AAB5
	}
}

func formatJicooTime(value, timeZone string) string {
	if value == "" {
		return "-"
	}
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		t, err = time.Parse(time.RFC3339, value)
		if err != nil {
			return value
		}
	}
	loc, err := time.LoadLocation(timeZone)
	if err != nil || timeZone == "" {
		return t.Format("2006-01-02 15:04 MST")
	}
	return t.In(loc).Format("2006-01-02 15:04 MST")
}

func buildJicooDiscordPayload(event JicooWebhookEvent) DiscordWebhookPayload {
	label, color := jicooEventLabel(event.Event)
	obj := event.Object

	fields := []DiscordEmbedField{
		{Name: "ゲスト", Value: nonEmpty(obj.Contact.Name, "-"), Inline: true},
		{Name: "メール", Value: nonEmpty(obj.Contact.Email, "-"), Inline: true},
		{Name: "ステータス", Value: nonEmpty(obj.Status, "-"), Inline: true},
		{Name: "開始", Value: formatJicooTime(obj.StartedAt, obj.TimeZone), Inline: true},
		{Name: "終了", Value: formatJicooTime(obj.EndedAt, obj.TimeZone), Inline: true},
		{Name: "タイムゾーン", Value: nonEmpty(obj.TimeZone, "-"), Inline: true},
		{Name: "予約UID", Value: nonEmpty(obj.UID, "-"), Inline: false},
	}

	if obj.CancelReason != nil && *obj.CancelReason != "" {
		fields = append(fields, DiscordEmbedField{
			Name: "キャンセル理由", Value: *obj.CancelReason, Inline: false,
		})
	}
	if obj.CancelledBy != nil && *obj.CancelledBy != "" {
		fields = append(fields, DiscordEmbedField{
			Name: "キャンセル者", Value: *obj.CancelledBy, Inline: true,
		})
	}
	if obj.DownloadLink != nil && *obj.DownloadLink != "" {
		fields = append(fields, DiscordEmbedField{
			Name: "録画リンク", Value: *obj.DownloadLink, Inline: false,
		})
	}
	if len(obj.Answers) > 0 {
		var answers strings.Builder
		for i, a := range obj.Answers {
			if i > 0 {
				answers.WriteString("\n")
			}
			answers.WriteString(fmt.Sprintf("**%s**: %s", a.Question, strings.Join(a.Content, ", ")))
		}
		value := answers.String()
		if len(value) > 1024 {
			value = value[:1021] + "..."
		}
		fields = append(fields, DiscordEmbedField{
			Name: "回答", Value: value, Inline: false,
		})
	}

	return DiscordWebhookPayload{
		Content: fmt.Sprintf("📅 **Jicoo: %s**", label),
		Embeds: []DiscordEmbed{{
			Title:     label,
			Color:     color,
			Fields:    fields,
			Timestamp: event.CreatedAt,
			Footer:    &DiscordEmbedFooter{Text: "Jicoo Webhook"},
		}},
	}
}

func formatJicooPlainMessage(event JicooWebhookEvent) string {
	label, _ := jicooEventLabel(event.Event)
	obj := event.Object
	var b strings.Builder
	b.WriteString(fmt.Sprintf("📅 **Jicoo: %s**\n", label))
	b.WriteString(fmt.Sprintf("ゲスト: %s (%s)\n", nonEmpty(obj.Contact.Name, "-"), nonEmpty(obj.Contact.Email, "-")))
	b.WriteString(fmt.Sprintf("開始: %s\n", formatJicooTime(obj.StartedAt, obj.TimeZone)))
	b.WriteString(fmt.Sprintf("終了: %s\n", formatJicooTime(obj.EndedAt, obj.TimeZone)))
	b.WriteString(fmt.Sprintf("UID: %s", nonEmpty(obj.UID, "-")))
	if obj.CancelReason != nil && *obj.CancelReason != "" {
		b.WriteString(fmt.Sprintf("\nキャンセル理由: %s", *obj.CancelReason))
	}
	if obj.DownloadLink != nil && *obj.DownloadLink != "" {
		b.WriteString(fmt.Sprintf("\n録画: %s", *obj.DownloadLink))
	}
	return b.String()
}

func nonEmpty(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

// rolloverApiStats runs at 00:00: the finished day's counts are kept as
// "yesterday" for the 09:00 report, and a fresh counter starts for today.
func rolloverApiStats() {
	statsMutex.Lock()
	defer statsMutex.Unlock()

	yesterdayStats = apiStats
	yesterdayDate = time.Now().In(location).AddDate(0, 0, -1).Format("2006-01-02")
	apiStats = make(map[string]int)
	log.Printf("API stats rolled over: %d labels closed for %s", len(yesterdayStats), yesterdayDate)
}

// buildApiStatsReport formats the daily report with labels in a stable order.
func buildApiStatsReport(date string, stats map[string]int) string {
	var b strings.Builder
	b.WriteString("📊 カウンター")
	if date != "" {
		b.WriteString(" (" + date + ")")
	}
	b.WriteString("\n")

	labels := make([]string, 0, len(stats))
	for label, count := range stats {
		if count > 0 {
			labels = append(labels, label)
		}
	}
	if len(labels) == 0 {
		b.WriteString("記録なし\n")
		return b.String()
	}
	sort.Strings(labels)
	for _, label := range labels {
		b.WriteString(fmt.Sprintf("%s : %d\n", label, stats[label]))
	}
	return b.String()
}

// reportApiStats runs at 09:00 and posts the previous day's counts.
func reportApiStats() {
	statsMutex.Lock()
	date := yesterdayDate
	stats := make(map[string]int, len(yesterdayStats))
	for k, v := range yesterdayStats {
		stats[k] = v
	}
	yesterdayStats = nil
	yesterdayDate = ""
	statsMutex.Unlock()

	if date == "" {
		// No rollover happened since startup (e.g. deployed after midnight)
		log.Println("No closed day to report yet; skipping API stats report")
		return
	}

	_, err := session.ChannelMessageSend(config.ChannelID, buildApiStatsReport(date, stats))
	if err != nil {
		log.Printf("Error sending stats report: %v", err)
	} else {
		log.Printf("Successfully reported API stats for %s to Discord channel", date)
	}
}
