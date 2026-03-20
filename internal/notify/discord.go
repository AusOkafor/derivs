package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"derivs-backend/internal/models"
)

// discordColor maps severity to Discord embed color (decimal RGB).
func discordColor(severity string) int {
	switch strings.ToLower(severity) {
	case "high":
		return 0xE74C3C // red
	case "medium":
		return 0xF1C40F // yellow
	default:
		return 0x95A5A6 // grey
	}
}

// SendDiscordAlert posts a formatted alert embed to a Discord webhook URL.
// Discord expects HTTP 204 No Content on success.
func SendDiscordAlert(ctx context.Context, webhookURL string, alert models.Alert, currentPrice float64) error {
	if webhookURL == "" {
		return nil
	}

	sevEmoji := "⚪"
	switch strings.ToLower(alert.Severity) {
	case "high":
		sevEmoji = "🔴"
	case "medium":
		sevEmoji = "🟡"
	}

	priceStr := ""
	if currentPrice > 0 {
		priceStr = fmt.Sprintf("\n💰 **Price:** $%.4g", currentPrice)
	}

	embed := map[string]any{
		"title":       fmt.Sprintf("%s %s — %s ALERT", sevEmoji, alert.Symbol, strings.ToUpper(alert.Severity)),
		"description": alert.Message + priceStr,
		"color":       discordColor(alert.Severity),
		"url":         "https://derivlens.io",
		"footer": map[string]string{
			"text": "DerivLens • derivlens.io",
		},
		"timestamp": alert.Timestamp.UTC().Format(time.RFC3339),
	}

	payload := map[string]any{
		"username": "DerivLens",
		"embeds":   []map[string]any{embed},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("discord: marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("discord: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("discord: send: %w", err)
	}
	defer resp.Body.Close()

	// Discord returns 204 on success; 200 if wait=true is set
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("discord: webhook status %d", resp.StatusCode)
	}
	return nil
}

// SendDiscordMessage posts a plain markdown message to a Discord webhook as a simple embed.
// Used for custom price alerts and other non-Alert-struct notifications.
func SendDiscordMessage(ctx context.Context, webhookURL, content string) error {
	if webhookURL == "" {
		return nil
	}

	embed := map[string]any{
		"description": content,
		"color":       0x3498DB, // blue — neutral, not severity-coded
		"footer": map[string]string{
			"text": "DerivLens • derivlens.io",
		},
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	}

	payload := map[string]any{
		"username": "DerivLens",
		"embeds":   []map[string]any{embed},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("discord: marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("discord: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("discord: send: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("discord: webhook status %d", resp.StatusCode)
	}
	return nil
}

// IsValidDiscordWebhookURL returns true for discord.com and discordapp.com webhook URLs.
func IsValidDiscordWebhookURL(u string) bool {
	return strings.HasPrefix(u, "https://discord.com/api/webhooks/") ||
		strings.HasPrefix(u, "https://discordapp.com/api/webhooks/")
}
