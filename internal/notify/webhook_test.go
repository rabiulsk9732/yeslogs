package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWebhook_Slack(t *testing.T) {
	var receivedBody map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &receivedBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer ts.Close()

	wh := WebhookConfig{
		Enabled: true,
		Type:    WebhookSlack,
		URL:     ts.URL,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := wh.Send(ctx, "Device Silence Alert", "Device #12 is down")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	text, ok := receivedBody["text"].(string)
	if !ok || !strings.Contains(text, "Device Silence Alert") || !strings.Contains(text, "Device #12 is down") {
		t.Fatalf("unexpected Slack payload: %v", receivedBody)
	}
}

func TestWebhook_Discord(t *testing.T) {
	var receivedBody map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &receivedBody)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ts.Close()

	wh := WebhookConfig{
		Enabled: true,
		Type:    WebhookDiscord,
		URL:     ts.URL,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := wh.Send(ctx, "Device Restored", "Device #12 is back online")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content, ok := receivedBody["content"].(string)
	if !ok || !strings.Contains(content, "Device Restored") || !strings.Contains(content, "back online") {
		t.Fatalf("unexpected Discord payload: %v", receivedBody)
	}
}

func TestWebhook_Telegram(t *testing.T) {
	var receivedBody map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &receivedBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer ts.Close()

	wh := WebhookConfig{
		Enabled:        true,
		Type:           WebhookTelegram,
		URL:            ts.URL + "/sendMessage",
		TelegramChatID: "-100123456789",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := wh.Send(ctx, "NTP Skew Warning", "Collector clock skew is 800ms")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	chatID, _ := receivedBody["chat_id"].(string)
	if chatID != "-100123456789" {
		t.Fatalf("expected chat_id -100123456789, got %v", chatID)
	}
	text, _ := receivedBody["text"].(string)
	if !strings.Contains(text, "NTP Skew Warning") {
		t.Fatalf("expected text to contain NTP Skew Warning, got %v", text)
	}
}

func TestWebhook_Generic(t *testing.T) {
	var receivedBody map[string]any
	var customHeader string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		customHeader = r.Header.Get("X-Custom-Auth")
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &receivedBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"received"}`))
	}))
	defer ts.Close()

	wh := WebhookConfig{
		Enabled: true,
		Type:    WebhookGeneric,
		URL:     ts.URL,
		Headers: map[string]string{"X-Custom-Auth": "secret-token-123"},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := wh.Send(ctx, "Disk Pressure Alert", "ClickHouse disk at 88%")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if customHeader != "secret-token-123" {
		t.Fatalf("expected custom header secret-token-123, got %s", customHeader)
	}
	if receivedBody["event"] != "alert" || receivedBody["subject"] != "Disk Pressure Alert" {
		t.Fatalf("unexpected generic payload: %v", receivedBody)
	}
}

func TestWebhook_AutoDetect(t *testing.T) {
	if detectWebhookType("https://hooks.slack.com/services/T00/B00/X00") != WebhookSlack {
		t.Errorf("failed to detect slack")
	}
	if detectWebhookType("https://discord.com/api/webhooks/123/abc") != WebhookDiscord {
		t.Errorf("failed to detect discord")
	}
	if detectWebhookType("https://api.telegram.org/bot123456:ABC-DEF/sendMessage") != WebhookTelegram {
		t.Errorf("failed to detect telegram")
	}
	if detectWebhookType("https://ops.example.com/alerts/webhook") != WebhookGeneric {
		t.Errorf("failed to detect generic")
	}
}

func TestWebhook_HTTPError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal server crash", http.StatusInternalServerError)
	}))
	defer ts.Close()

	wh := WebhookConfig{
		Enabled: true,
		Type:    WebhookGeneric,
		URL:     ts.URL,
	}

	err := wh.Send(context.Background(), "Sub", "Body")
	if err == nil {
		t.Fatal("expected error on 500, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Fatalf("expected error to mention 500, got: %v", err)
	}
}
