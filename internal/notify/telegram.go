package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"derivs-backend/internal/models"
)

type TelegramNotifier struct {
	botToken   string
	httpClient *http.Client
}

func NewTelegram(botToken string) *TelegramNotifier {
	return &TelegramNotifier{
		botToken:   botToken,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// SendMessage sends a plain text (HTML-formatted) message to a Telegram chat ID.
// POST https://api.telegram.org/bot{token}/sendMessage
func (t *TelegramNotifier) SendMessage(ctx context.Context, chatID int64, text string) error {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", t.botToken)

	body, err := json.Marshal(map[string]any{
		"chat_id":    chatID,
		"text":       text,
		"parse_mode": "HTML",
	})
	if err != nil {
		return fmt.Errorf("telegram: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("telegram: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("telegram: send: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("telegram: unexpected status %d", resp.StatusCode)
	}
	return nil
}

// FormatAlert formats a models.Alert into a Telegram HTML message.
//
// Example output:
//
//	🔴 <b>BTC — HIGH ALERT</b>
//	OI spike -3.42% in 1h without price confirmation — potential distribution
//	<i>DerivLens • 22:18 UTC</i>
func (t *TelegramNotifier) FormatAlert(symbol string, alert models.Alert) string {
	emoji := severityEmoji(alert.Severity)
	severity := strings.ToUpper(alert.Severity)
	ts := alert.Timestamp.UTC().Format("15:04") + " UTC"

	return fmt.Sprintf(
		"%s <b>%s — %s ALERT</b>\n%s\n<i>DerivLens • %s</i>",
		emoji, symbol, severity, alert.Message, ts,
	)
}

func severityEmoji(severity string) string {
	switch strings.ToLower(severity) {
	case "high":
		return "🔴"
	case "medium":
		return "🟡"
	default:
		return "⚪"
	}
}

// PostToChannel posts a message to the public channel @derivlens_signals.
func (t *TelegramNotifier) PostToChannel(message string) error {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", t.botToken)
	payload := map[string]interface{}{
		"chat_id":    "@derivlens_signals",
		"text":       message,
		"parse_mode": "HTML",
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("telegram: marshal channel request: %w", err)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("telegram: build channel request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("telegram: post to channel: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("telegram: channel unexpected status %d", resp.StatusCode)
	}
	return nil
}

// PostTopAlert posts a HIGH severity alert to the public channel as a free preview.
func (t *TelegramNotifier) PostTopAlert(alert models.Alert) error {
	firstLine := alert.Message
	if idx := strings.Index(alert.Message, "\n"); idx > 0 {
		firstLine = alert.Message[:idx]
	}
	msg := fmt.Sprintf(
		"🚨 HIGH ALERT — %s\n%s\n\nFull signal → derivlens-pro.vercel.app\nGet alerts → t.me/derivlens_signals",
		alert.Symbol,
		firstLine,
	)
	return t.PostToChannel(msg)
}

// VerifyAuth validates Telegram Login Widget auth data using HMAC-SHA256.
// See https://core.telegram.org/widgets/login#checking-authorization
func (t *TelegramNotifier) VerifyAuth(data map[string]string) bool {
	hash, ok := data["hash"]
	if !ok || hash == "" {
		return false
	}
	delete(data, "hash")

	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(data[k])
	}
	checkString := b.String()

	secretKey := sha256.Sum256([]byte(t.botToken))
	mac := hmac.New(sha256.New, secretKey[:])
	mac.Write([]byte(checkString))
	expected := hex.EncodeToString(mac.Sum(nil))

	return hmac.Equal([]byte(expected), []byte(hash))
}
