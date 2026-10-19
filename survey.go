package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"strings"
)

// Survey form receiver (/api/survey)
//
// The thank-you page of the static site (chisei_web) posts its survey form
// straight from the browser to this endpoint, so it has to work as a plain
// HTML form target as well as a fetch() target:
//
//   - application/x-www-form-urlencoded and multipart/form-data are what a
//     <form method="post"> sends. Both are CORS "simple requests", so no
//     preflight is involved when fetch() sends them from another origin.
//   - JSON (application/json or text/plain) is accepted for beacons and tools.
//   - OPTIONS is answered with CORS headers for any client that preflights.
//
// A navigation request (the form submitted without JavaScript) is redirected
// back to the page named by the hidden `_next` field; everything else gets a
// small JSON body.

const (
	surveyMaxBody      = 64 << 10 // request body limit in bytes
	surveyMaxFields    = 20       // answers kept per submission
	surveyMaxValueLen  = 1500     // runes kept per answer
	surveyHoneypot     = "website"
	surveyNextField    = "_next"
	surveyProductField = "_product" // which product the survey is about (shown in the heading)
	surveyPageField    = "_page"    // URL of the page that sent it (shown as a footer)
	surveyMaxMetaLen   = 300
	surveyDiscordMax   = 1900 // Discord message limit is 2000 characters
)

type surveyField struct {
	Name  string
	Value string
}

// surveyMeta holds the reserved fields that describe a submission rather
// than answer a question.
type surveyMeta struct {
	Product string // e.g. "ねおきゴーレム"; empty means the chisei_web survey
	Page    string // http(s) URL of the sending page, or empty
}

// surveyNotify delivers the formatted message. Tests replace it.
var surveyNotify = sendSurveyToDiscord

func handleSurvey(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	w.Header().Set("Access-Control-Max-Age", "86400")
	w.Header().Set("Cache-Control", "no-store")

	switch r.Method {
	case http.MethodOptions:
		w.WriteHeader(http.StatusNoContent)
		return
	case http.MethodPost:
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, surveyMaxBody)
	fields, err := parseSurveyRequest(r)
	if err != nil {
		log.Printf("Rejected survey submission: %v", err)
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	var answers []surveyField
	var meta surveyMeta
	next := ""
	spam := false
	for _, f := range fields {
		switch f.Name {
		case surveyHoneypot:
			spam = spam || f.Value != ""
		case surveyNextField:
			next = f.Value
		case surveyProductField:
			meta.Product = truncateRunes(f.Value, surveyMaxMetaLen)
		case surveyPageField:
			meta.Page = surveyPageURL(f.Value)
		default:
			if f.Value == "" {
				continue
			}
			answers = append(answers, f)
		}
	}

	switch {
	case spam:
		// Bots fill the hidden field; pretend it worked so they don't adapt.
		log.Printf("Ignored survey submission: honeypot filled")
	case len(answers) == 0:
		http.Error(w, "Bad Request: no answers", http.StatusBadRequest)
		return
	default:
		if len(answers) > surveyMaxFields {
			answers = answers[:surveyMaxFields]
		}
		go surveyNotify(buildSurveyMessage(answers, meta))
	}

	if surveyWantsNavigation(r) {
		if target := surveyRedirectTarget(r, next); target != "" {
			http.Redirect(w, r, target, http.StatusSeeOther)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `<!doctype html><meta charset="utf-8"><title>送信しました</title><p>送信しました。ありがとうございます！</p>`)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, `{"ok":true}`)
}

// parseSurveyRequest returns the submitted fields in the order they were sent.
func parseSurveyRequest(r *http.Request) ([]surveyField, error) {
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil && r.Header.Get("Content-Type") != "" {
		return nil, fmt.Errorf("bad content type: %w", err)
	}

	switch mediaType {
	case "multipart/form-data":
		return parseSurveyMultipart(r, params["boundary"])
	case "application/json", "text/plain", "":
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, fmt.Errorf("reading body: %w", err)
		}
		if mediaType == "" && !looksLikeJSON(body) {
			return parseSurveyURLEncoded(string(body))
		}
		return parseSurveyJSON(body)
	default: // application/x-www-form-urlencoded and anything else form-like
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, fmt.Errorf("reading body: %w", err)
		}
		return parseSurveyURLEncoded(string(body))
	}
}

func looksLikeJSON(body []byte) bool {
	trimmed := strings.TrimSpace(string(body))
	return strings.HasPrefix(trimmed, "{")
}

// parseSurveyURLEncoded keeps field order, which url.ParseQuery would lose.
func parseSurveyURLEncoded(body string) ([]surveyField, error) {
	var fields []surveyField
	for _, pair := range strings.Split(body, "&") {
		if pair == "" {
			continue
		}
		name, value, _ := strings.Cut(pair, "=")
		decodedName, err := url.QueryUnescape(name)
		if err != nil {
			return nil, fmt.Errorf("bad field name %q: %w", name, err)
		}
		decodedValue, err := url.QueryUnescape(value)
		if err != nil {
			return nil, fmt.Errorf("bad value for %q: %w", decodedName, err)
		}
		fields = append(fields, newSurveyField(decodedName, decodedValue))
	}
	return fields, nil
}

func parseSurveyMultipart(r *http.Request, boundary string) ([]surveyField, error) {
	if boundary == "" {
		return nil, fmt.Errorf("multipart body without boundary")
	}
	reader, err := r.MultipartReader()
	if err != nil {
		return nil, fmt.Errorf("multipart: %w", err)
	}
	var fields []surveyField
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			return fields, nil
		}
		if err != nil {
			return nil, fmt.Errorf("multipart part: %w", err)
		}
		if part.FileName() != "" || part.FormName() == "" {
			part.Close()
			continue // files are not part of the survey
		}
		value, err := io.ReadAll(io.LimitReader(part, surveyMaxBody))
		part.Close()
		if err != nil {
			return nil, fmt.Errorf("multipart value: %w", err)
		}
		fields = append(fields, newSurveyField(part.FormName(), string(value)))
	}
}

// parseSurveyJSON accepts a flat JSON object and keeps key order.
func parseSurveyJSON(body []byte) ([]surveyField, error) {
	dec := json.NewDecoder(strings.NewReader(string(body)))
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("json: %w", err)
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return nil, fmt.Errorf("json: expected an object")
	}
	var fields []surveyField
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("json key: %w", err)
		}
		key, _ := keyTok.(string)
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, fmt.Errorf("json value for %q: %w", key, err)
		}
		var value string
		var asString string
		if err := json.Unmarshal(raw, &asString); err == nil {
			value = asString
		} else {
			value = string(raw) // numbers, booleans, nested values as-is
			if value == "null" {
				value = ""
			}
		}
		fields = append(fields, newSurveyField(key, value))
	}
	return fields, nil
}

func newSurveyField(name, value string) surveyField {
	value = strings.TrimSpace(value)
	if runes := []rune(value); len(runes) > surveyMaxValueLen {
		value = string(runes[:surveyMaxValueLen]) + "…"
	}
	return surveyField{Name: strings.TrimSpace(name), Value: value}
}

// surveyPageURL keeps only an http(s) URL so the footer can never carry
// javascript: links or stray text.
func surveyPageURL(value string) string {
	value = truncateRunes(strings.TrimSpace(value), surveyMaxMetaLen)
	if strings.HasPrefix(value, "https://") || strings.HasPrefix(value, "http://") {
		return value
	}
	return ""
}

func truncateRunes(s string, max int) string {
	if runes := []rune(s); len(runes) > max {
		return string(runes[:max])
	}
	return s
}

func buildSurveyMessage(answers []surveyField, meta surveyMeta) string {
	var b strings.Builder
	if meta.Product != "" {
		b.WriteString("📨 **" + meta.Product + "のアンケートが届きました**\n")
	} else {
		b.WriteString("📨 **知性アンケートが届きました**\n")
	}
	for _, a := range answers {
		b.WriteString("**")
		b.WriteString(a.Name)
		b.WriteString("**\n> ")
		b.WriteString(strings.ReplaceAll(a.Value, "\n", "\n> "))
		b.WriteString("\n")
	}
	if meta.Page != "" {
		b.WriteString("-# " + meta.Page + "\n")
	}
	msg := strings.TrimRight(b.String(), "\n")
	if runes := []rune(msg); len(runes) > surveyDiscordMax {
		msg = string(runes[:surveyDiscordMax]) + "…"
	}
	return msg
}

func sendSurveyToDiscord(message string) {
	if session == nil {
		log.Printf("Discord session not ready; survey not forwarded:\n%s", message)
		return
	}
	if _, err := session.ChannelMessageSend(config.ChannelID, message); err != nil {
		log.Printf("Error forwarding survey to Discord: %v", err)
		return
	}
	log.Println("Successfully forwarded survey to Discord channel")
}

// surveyWantsNavigation is true for a plain <form> submission (no JavaScript),
// which expects a page back rather than JSON.
func surveyWantsNavigation(r *http.Request) bool {
	if mode := r.Header.Get("Sec-Fetch-Mode"); mode != "" {
		return mode == "navigate"
	}
	accept := r.Header.Get("Accept")
	return strings.Contains(accept, "text/html") && !strings.Contains(accept, "application/json")
}

// surveyRedirectTarget resolves `_next` against the Referer and only allows
// a destination on the same host as the page that sent the form, so the
// endpoint cannot be used as an open redirect.
func surveyRedirectTarget(r *http.Request, next string) string {
	referer, err := url.Parse(r.Header.Get("Referer"))
	if err != nil || referer.Host == "" || (referer.Scheme != "http" && referer.Scheme != "https") {
		return ""
	}
	if next == "" {
		return referer.String()
	}
	target, err := url.Parse(next)
	if err != nil {
		return referer.String()
	}
	resolved := referer.ResolveReference(target)
	if resolved.Host != referer.Host || resolved.Scheme != referer.Scheme {
		return referer.String()
	}
	return resolved.String()
}
