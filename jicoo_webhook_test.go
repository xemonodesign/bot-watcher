package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
	"time"
)

func TestVerifyJicooSignature(t *testing.T) {
	secret := "whsec_test_secret"
	body := []byte(`{"event":"guest_booked","createdAt":"2020-11-23T17:51:19.000Z","object":{"uid":"ACDADADASS"}}`)
	timestamp := fmt.Sprintf("%d", time.Now().Unix())

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp + "." + string(body)))
	sig := hex.EncodeToString(mac.Sum(nil))
	header := fmt.Sprintf("t=%s,v1=%s", timestamp, sig)

	if err := verifyJicooSignature(body, header, secret); err != nil {
		t.Fatalf("expected valid signature, got error: %v", err)
	}
}

func TestVerifyJicooSignatureRejectsMismatch(t *testing.T) {
	body := []byte(`{"event":"guest_booked"}`)
	header := fmt.Sprintf("t=%d,v1=deadbeef", time.Now().Unix())

	if err := verifyJicooSignature(body, header, "secret"); err == nil {
		t.Fatal("expected signature mismatch error")
	}
}

func TestVerifyJicooSignatureRejectsOldTimestamp(t *testing.T) {
	secret := "whsec_test_secret"
	body := []byte(`{"event":"guest_booked"}`)
	timestamp := fmt.Sprintf("%d", time.Now().Add(-10*time.Minute).Unix())

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp + "." + string(body)))
	sig := hex.EncodeToString(mac.Sum(nil))
	header := fmt.Sprintf("t=%s,v1=%s", timestamp, sig)

	if err := verifyJicooSignature(body, header, secret); err == nil {
		t.Fatal("expected timestamp tolerance error")
	}
}

func TestJicooEventLabel(t *testing.T) {
	label, color := jicooEventLabel("guest_booked")
	if label != "新規予約" {
		t.Fatalf("unexpected label: %s", label)
	}
	if color == 0 {
		t.Fatal("expected non-zero color")
	}
}

func TestBuildJicooDiscordPayload(t *testing.T) {
	cancelReason := "Another meeting scheduled"
	cancelledBy := "guest"
	event := JicooWebhookEvent{
		Event:     "guest_cancelled",
		CreatedAt: "2020-11-23T17:51:19.000Z",
		Object: JicooObject{
			UID:          "ACDADADASS",
			Status:       "cancel",
			TimeZone:     "Asia/Tokyo",
			StartedAt:    "2022-09-01T17:00:00.000Z",
			EndedAt:      "2022-09-01T18:00:00.000Z",
			CancelReason: &cancelReason,
			CancelledBy:  &cancelledBy,
			Contact: JicooContact{
				Name:  "John Doe",
				Email: "example@example.com",
			},
			Answers: []JicooAnswer{
				{Question: "How did you find out?", Content: []string{"SNS", "DM"}},
			},
		},
	}

	payload := buildJicooDiscordPayload(event)
	if payload.Content == "" {
		t.Fatal("expected content")
	}
	if len(payload.Embeds) != 1 {
		t.Fatalf("expected 1 embed, got %d", len(payload.Embeds))
	}
	if payload.Embeds[0].Title != "ゲストによるキャンセル" {
		t.Fatalf("unexpected title: %s", payload.Embeds[0].Title)
	}
}
