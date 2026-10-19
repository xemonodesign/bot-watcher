package main

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// captureSurvey swaps surveyNotify for a channel and restores it afterwards.
func captureSurvey(t *testing.T) chan string {
	t.Helper()
	messages := make(chan string, 1)
	orig := surveyNotify
	surveyNotify = func(msg string) { messages <- msg }
	t.Cleanup(func() { surveyNotify = orig })
	return messages
}

func waitSurvey(t *testing.T, messages chan string) string {
	t.Helper()
	select {
	case msg := <-messages:
		return msg
	case <-time.After(2 * time.Second):
		t.Fatal("expected a survey message to be forwarded")
		return ""
	}
}

func assertNoSurvey(t *testing.T, messages chan string) {
	t.Helper()
	select {
	case msg := <-messages:
		t.Fatalf("expected no message, got %q", msg)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestSurveyPreflightAllowsCrossOrigin(t *testing.T) {
	req := httptest.NewRequest(http.MethodOptions, "/api/survey", nil)
	req.Header.Set("Origin", "https://chisei.xemono.life")
	req.Header.Set("Access-Control-Request-Method", "POST")
	rec := httptest.NewRecorder()

	handleSurvey(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("expected wildcard origin, got %q", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Methods"); !strings.Contains(got, "POST") {
		t.Fatalf("expected POST to be allowed, got %q", got)
	}
}

func TestSurveyRejectsGet(t *testing.T) {
	rec := httptest.NewRecorder()
	handleSurvey(rec, httptest.NewRequest(http.MethodGet, "/api/survey", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rec.Code)
	}
}

func TestSurveyFormPostIsForwardedInOrder(t *testing.T) {
	messages := captureSurvey(t)

	form := url.Values{}
	form.Set("_next", "/thankyou/#survey-sent")
	form.Set("website", "")
	body := "知性をどこで知りましたか？=" + url.QueryEscape("友達のサーバー") +
		"&知性のご飯を買おうと思った理由を教えてください=" + url.QueryEscape("かわいい\nかしこい") +
		"&あなたの知性はどんなサーバーにいますか？=" +
		"&" + form.Encode()
	req := httptest.NewRequest(http.MethodPost, "/api/survey", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=UTF-8")
	req.Header.Set("Origin", "https://chisei.xemono.life")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	rec := httptest.NewRecorder()

	handleSurvey(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("expected CORS header on the response, got %q", got)
	}
	if strings.TrimSpace(rec.Body.String()) != `{"ok":true}` {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}

	msg := waitSurvey(t, messages)
	want := "📨 **知性アンケートが届きました**\n" +
		"**知性をどこで知りましたか？**\n> 友達のサーバー\n" +
		"**知性のご飯を買おうと思った理由を教えてください**\n> かわいい\n> かしこい"
	if msg != want {
		t.Fatalf("unexpected message:\n%s\nwant:\n%s", msg, want)
	}
}

func TestSurveyHoneypotIsIgnoredSilently(t *testing.T) {
	messages := captureSurvey(t)

	body := "q=" + url.QueryEscape("spam") + "&website=http%3A%2F%2Fexample.com"
	req := httptest.NewRequest(http.MethodPost, "/api/survey", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()

	handleSurvey(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 so bots don't adapt, got %d", rec.Code)
	}
	assertNoSurvey(t, messages)
}

func TestSurveyRejectsEmptySubmission(t *testing.T) {
	messages := captureSurvey(t)

	body := "q1=&q2=%20&_next=%2Fthankyou%2F"
	req := httptest.NewRequest(http.MethodPost, "/api/survey", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()

	handleSurvey(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
	assertNoSurvey(t, messages)
}

func TestSurveyJSONPostKeepsKeyOrder(t *testing.T) {
	messages := captureSurvey(t)

	body := `{"b":"second","a":"first","n":3,"empty":"","website":""}`
	req := httptest.NewRequest(http.MethodPost, "/api/survey", strings.NewReader(body))
	req.Header.Set("Content-Type", "text/plain;charset=UTF-8")
	rec := httptest.NewRecorder()

	handleSurvey(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	msg := waitSurvey(t, messages)
	if !strings.Contains(msg, "**b**\n> second\n**a**\n> first\n**n**\n> 3") {
		t.Fatalf("unexpected message:\n%s", msg)
	}
	if strings.Contains(msg, "empty") {
		t.Fatalf("empty answers should be dropped:\n%s", msg)
	}
}

func TestSurveyMultipartPostIsAccepted(t *testing.T) {
	messages := captureSurvey(t)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("知性をどこで知りましたか？", "Discordの掲示板")
	_ = mw.WriteField("website", "")
	_ = mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/survey", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()

	handleSurvey(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	msg := waitSurvey(t, messages)
	if !strings.Contains(msg, "**知性をどこで知りましたか？**\n> Discordの掲示板") {
		t.Fatalf("unexpected message:\n%s", msg)
	}
}

func TestSurveyNavigationRedirectsBackToReferer(t *testing.T) {
	captureSurvey(t)

	body := "q=" + url.QueryEscape("answer") + "&_next=" + url.QueryEscape("/thankyou/#survey-sent")
	req := httptest.NewRequest(http.MethodPost, "/api/survey", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Referer", "https://chisei.xemono.life/")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	rec := httptest.NewRecorder()

	handleSurvey(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "https://chisei.xemono.life/thankyou/#survey-sent" {
		t.Fatalf("unexpected redirect target %q", got)
	}
}

func TestSurveyNavigationRefusesForeignRedirect(t *testing.T) {
	captureSurvey(t)

	body := "q=answer&_next=" + url.QueryEscape("https://evil.example/phish")
	req := httptest.NewRequest(http.MethodPost, "/api/survey", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Referer", "https://chisei.xemono.life/thankyou/")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	rec := httptest.NewRecorder()

	handleSurvey(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "https://chisei.xemono.life/thankyou/" {
		t.Fatalf("expected fallback to the referer, got %q", got)
	}
}

func TestSurveyNavigationWithoutRefererShowsPage(t *testing.T) {
	captureSurvey(t)

	req := httptest.NewRequest(http.MethodPost, "/api/survey", strings.NewReader("q=answer"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()

	handleSurvey(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("expected an HTML page, got %q", ct)
	}
}

func TestBuildSurveyMessageTruncatesLongAnswers(t *testing.T) {
	long := strings.Repeat("あ", surveyMaxValueLen+50)
	msg := buildSurveyMessage([]surveyField{newSurveyField("q", long)})
	if len([]rune(msg)) > surveyDiscordMax+1 {
		t.Fatalf("message exceeds Discord limit: %d runes", len([]rune(msg)))
	}
	if !strings.HasSuffix(msg, "…") {
		t.Fatalf("expected truncation marker, got tail %q", msg[len(msg)-10:])
	}
}
