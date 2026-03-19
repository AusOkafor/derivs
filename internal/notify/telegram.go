package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"mime/multipart"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"derivs-backend/internal/alerts"
	"derivs-backend/internal/cards"
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

// SendPhoto sends a photo to a Telegram chat with an optional caption.
func (t *TelegramNotifier) SendPhoto(chatID string, imgBytes []byte, caption string) error {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendPhoto", t.botToken)

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	part, err := writer.CreateFormFile("photo", "alert.png")
	if err != nil {
		return err
	}
	if _, err := part.Write(imgBytes); err != nil {
		return err
	}

	_ = writer.WriteField("chat_id", chatID)
	if caption != "" {
		_ = writer.WriteField("caption", caption)
	}
	if err := writer.Close(); err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := t.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("telegram: sendPhoto unexpected status %d", resp.StatusCode)
	}
	return nil
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

// buildAlertCardData constructs AlertCardData from alert, snap, and signals.
// Uses alert's ClusterPrice, ClusterSize, Distance when populated; falls back to magnet.
func buildAlertCardData(alert models.Alert, snap models.MarketSnapshot, sigs models.MarketSignals) *cards.AlertCardData {
	clusterPrice := alert.ClusterPrice
	clusterSize := alert.ClusterSize
	distance := alert.Distance
	sweepProb := alert.Probability

	if clusterSize == 0 && sigs.LiquidationMagnet != nil {
		magnet := sigs.LiquidationMagnet
		minSize := alerts.GetMinClusterSize(alert.Symbol)
		if magnet.SizeUSD >= minSize && magnet.Distance >= 0.1 {
			clusterPrice = magnet.Price
			clusterSize = magnet.SizeUSD
			distance = magnet.Distance / 100
			sweepProb = magnet.Probability
		}
	}
	if clusterSize == 0 {
		return nil
	}

	sev := "HIGH"
	if alert.Severity == "medium" {
		sev = "MEDIUM"
	}
	return &cards.AlertCardData{
		Symbol:       alert.Symbol,
		Severity:     sev,
		AlertType:    "Liquidity Sweep",
		Price:        snap.LiquidationMap.CurrentPrice,
		ClusterPrice: clusterPrice,
		ClusterSize:  clusterSize,
		Distance:     distance,
		SweepProb:    sweepProb,
		CascadeLevel: sigs.CascadeRisk.Level,
		CascadeScore: sigs.CascadeRisk.Score,
		GravityDir:   sigs.LiquidityGravity.Dominant,
		GravityPct:   math.Max(sigs.LiquidityGravity.UpwardPull, sigs.LiquidityGravity.DownwardPull),
		Funding:      snap.FundingRate.Rate,
		OIChange:     snap.OpenInterest.OIChange1h,
	}
}

// formatPriceForAlert formats price for Telegram message.
func formatPriceForAlert(p float64) string {
	if p >= 1000 {
		return fmt.Sprintf("$%.2f", p)
	} else if p >= 100 {
		return fmt.Sprintf("$%.3f", p)
	} else if p >= 1 {
		return fmt.Sprintf("$%.4f", p)
	} else if p >= 0.1 {
		return fmt.Sprintf("$%.5f", p) // was 4, now 5
	} else if p >= 0.01 {
		return fmt.Sprintf("$%.6f", p) // was 5, now 6
	}
	return fmt.Sprintf("$%.8f", p)
}

// formatUSDForAlert formats USD for Telegram message.
func formatUSDForAlert(v float64) string {
	if v >= 1_000_000 {
		return fmt.Sprintf("$%.2fM", v/1_000_000)
	}
	if v >= 1_000 {
		return fmt.Sprintf("$%.1fK", v/1_000)
	}
	return fmt.Sprintf("$%.0f", v)
}

// buildFormattedAlertText returns the HTML text for a formatted alert card.
func buildFormattedAlertText(data cards.AlertCardData) string {
	sevEmoji := "🔴"
	if data.Severity == "MEDIUM" {
		sevEmoji = "🟡"
	}
	direction := "⬆️ Upward sweep likely"
	if data.ClusterPrice < data.Price {
		direction = "⬇️ Downward sweep likely"
	}
	return fmt.Sprintf(
		"%s <b>%s — Liquidity Sweep</b>\n\n"+
			"💰 <b>Price:</b> %s\n"+
			"🎯 <b>Target:</b> %s\n"+
			"📏 <b>Distance:</b> %.2f%%\n"+
			"💵 <b>Cluster:</b> %s\n\n"+
			"📊 <b>Sweep Probability:</b> %d%%\n"+
			"⚡ <b>Cascade Risk:</b> %s (%d/100)\n"+
			"🌊 <b>Gravity:</b> %.1f%% %s\n"+
			"💸 <b>Funding:</b> %.4f%%\n\n"+
			"%s\n\n"+
			"📈 <a href=\"https://derivlens.io\">Full dashboard</a> │ 🔔 <a href=\"https://t.me/derivlens_signals\">Get alerts</a>",
		sevEmoji, data.Symbol,
		formatPriceForAlert(data.Price),
		formatPriceForAlert(data.ClusterPrice),
		data.Distance*100,
		formatUSDForAlert(data.ClusterSize),
		data.SweepProb,
		data.CascadeLevel,
		data.CascadeScore,
		data.GravityPct,
		data.GravityDir,
		data.Funding*100,
		direction,
	)
}

// SendFormattedAlert sends a richly formatted text alert (no image) using HTML.
// Used for public channel posts (no snooze button). For subscriber DMs use SendAlertCardToUser.
func (t *TelegramNotifier) SendFormattedAlert(ctx context.Context, chatID string, data cards.AlertCardData) error {
	message := buildFormattedAlertText(data)
	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", t.botToken)
	payload := map[string]interface{}{
		"chat_id":                  chatID,
		"text":                     message,
		"parse_mode":               "HTML",
		"disable_web_page_preview": true,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("telegram: marshal formatted alert: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("telegram: build formatted alert request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("telegram: send formatted alert: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("telegram: formatted alert status %d", resp.StatusCode)
	}
	return nil
}

// sendMessageWithKeyboard sends an HTML message to chatIDStr with an inline keyboard.
func (t *TelegramNotifier) sendMessageWithKeyboard(ctx context.Context, chatIDStr, text string, keyboard [][]map[string]string) error {
	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", t.botToken)
	payload := map[string]any{
		"chat_id":                  chatIDStr,
		"text":                     text,
		"parse_mode":               "HTML",
		"disable_web_page_preview": true,
		"reply_markup":             map[string]any{"inline_keyboard": keyboard},
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("telegram: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("telegram: sendMessageWithKeyboard: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("telegram: sendMessageWithKeyboard: status %d", resp.StatusCode)
	}
	return nil
}

// AnswerCallbackQuery acknowledges a Telegram inline button press (required within 10s).
func (t *TelegramNotifier) AnswerCallbackQuery(callbackQueryID, text string) error {
	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/answerCallbackQuery", t.botToken)
	payload := map[string]any{
		"callback_query_id": callbackQueryID,
		"text":              text,
		"show_alert":        false,
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, apiURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("telegram: AnswerCallbackQuery: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("telegram: AnswerCallbackQuery: %w", err)
	}
	defer resp.Body.Close()
	return nil
}

// snoozeKeyboard builds the inline keyboard row attached to alert messages.
func snoozeKeyboard(symbol string) [][]map[string]string {
	return [][]map[string]string{
		{
			{"text": "😴 Snooze 1h", "callback_data": fmt.Sprintf("snooze:%s:1h", symbol)},
			{"text": "😴 4h", "callback_data": fmt.Sprintf("snooze:%s:4h", symbol)},
		},
	}
}

// SendAlertCardToUser sends formatted alert text to a specific user's chat with a snooze button.
func (t *TelegramNotifier) SendAlertCardToUser(ctx context.Context, chatID int64, alert models.Alert, snap models.MarketSnapshot, sigs models.MarketSignals) error {
	chatIDStr := strconv.FormatInt(chatID, 10)
	keyboard := snoozeKeyboard(alert.Symbol)
	data := buildAlertCardData(alert, snap, sigs)
	if data != nil {
		msg := buildFormattedAlertText(*data)
		return t.sendMessageWithKeyboard(ctx, chatIDStr, msg, keyboard)
	}
	sev := severityEmoji(alert.Severity)
	msg := fmt.Sprintf("%s %s ALERT — %s\n%s\n\nFull dashboard → derivlens.io", sev, strings.ToUpper(alert.Severity), alert.Symbol, alert.Message)
	return t.sendMessageWithKeyboard(ctx, chatIDStr, msg, keyboard)
}

// PostTopAlert posts a HIGH severity alert to the public channel as formatted text (no image).
func (t *TelegramNotifier) PostTopAlert(alert models.Alert, snap models.MarketSnapshot, sigs models.MarketSignals) error {
	data := buildAlertCardData(alert, snap, sigs)
	if data != nil {
		return t.SendFormattedAlert(context.Background(), "@derivlens_signals", *data)
	}
	firstLine := alert.Message
	if idx := strings.Index(alert.Message, "\n"); idx > 0 {
		firstLine = alert.Message[:idx]
	}
	sevEmoji := "🔴"
	sevLabel := "HIGH"
	if alert.Severity == "medium" {
		sevEmoji = "🟡"
		sevLabel = "MEDIUM"
	} else if alert.Severity == "low" {
		sevEmoji = "🔵"
		sevLabel = "LOW"
	}
	msg := fmt.Sprintf(
		"%s %s ALERT — %s\n%s\n\nFull signal → derivlens.io\nGet alerts → t.me/derivlens_signals",
		sevEmoji, sevLabel, alert.Symbol, firstLine,
	)
	return t.PostToChannel(msg)
}

// SendToAdmin sends a message to the admin's personal Telegram chat.
// Set ADMIN_TELEGRAM_CHAT_ID in env (get it from @userinfobot on Telegram).
func (t *TelegramNotifier) SendToAdmin(message string) error {
	adminChatID := os.Getenv("ADMIN_TELEGRAM_CHAT_ID")
	if adminChatID == "" {
		return nil
	}
	id, err := strconv.ParseInt(adminChatID, 10, 64)
	if err != nil {
		return nil
	}
	return t.SendMessage(context.Background(), id, message)
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
