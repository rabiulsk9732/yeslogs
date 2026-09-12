package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type WebhookType string

const (
	WebhookGeneric  WebhookType = "generic"
	WebhookSlack    WebhookType = "slack"
	WebhookDiscord  WebhookType = "discord"
	WebhookTelegram WebhookType = "telegram"
)

type WebhookConfig struct {
	Enabled        bool              `json:"enabled"`
	Type           WebhookType       `json:"type"`           // generic | slack | discord | telegram
	URL            string            `json:"url"`            // target webhook endpoint
	TelegramChatID string            `json:"telegramChatId"` // required for Telegram bot API
	Headers        map[string]string `json:"headers,omitempty"`
	Timeout        time.Duration     `json:"-"`
}

func detectWebhookType(rawURL string) WebhookType {
	lower := strings.ToLower(rawURL)
	switch {
	case strings.Contains(lower, "hooks.slack.com"):
		return WebhookSlack
	case strings.Contains(lower, "discord.com") || strings.Contains(lower, "discordapp.com"):
		return WebhookDiscord
	case strings.Contains(lower, "telegram.org"):
		return WebhookTelegram
	default:
		return WebhookGeneric
	}
}

func (w WebhookConfig) Send(ctx context.Context, subject, body string) error {
	if !w.Enabled {
		return nil
	}
	targetURL := strings.TrimSpace(w.URL)
	if targetURL == "" {
		return fmt.Errorf("webhook URL not configured")
	}
	parsedURL, err := url.Parse(targetURL)
	if err != nil || (parsedURL.Scheme != "http" && parsedURL.Scheme != "https") {
		return fmt.Errorf("invalid webhook URL (must be http/https): %s", targetURL)
	}

	wType := w.Type
	if wType == "" {
		wType = detectWebhookType(targetURL)
	}

	var payload any
	switch wType {
	case WebhookSlack:
		payload = map[string]any{
			"text": fmt.Sprintf(":rotating_light: *%s*\n```%s```", subject, body),
		}
	case WebhookDiscord:
		payload = map[string]any{
			"content": fmt.Sprintf("🚨 **%s**\n```\n%s\n```", subject, body),
		}
	case WebhookTelegram:
		if !strings.HasSuffix(targetURL, "/sendMessage") {
			targetURL = strings.TrimRight(targetURL, "/") + "/sendMessage"
		}
		payload = map[string]any{
			"chat_id":    w.TelegramChatID,
			"text":       fmt.Sprintf("*%s*\n\n%s", subject, body),
			"parse_mode": "Markdown",
		}
	case WebhookGeneric:
		fallthrough
	default:
		payload = map[string]any{
			"event":     "alert",
			"subject":   subject,
			"body":      body,
			"timestamp": time.Now().UTC().Format(time.RFC3339),
		}
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal webhook payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("create webhook request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "YesLogs-Notifier/2.0")
	for k, v := range w.Headers {
		req.Header.Set(k, v)
	}

	timeout := w.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	client := &http.Client{Timeout: timeout}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("dispatch webhook %s: %w", targetURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("webhook %s failed with status %d: %s", targetURL, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	return nil
}
