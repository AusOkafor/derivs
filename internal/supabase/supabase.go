package supabase

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"derivs-backend/internal/models"
)

// Subscriber mirrors the `subscribers` table in Supabase.
type Subscriber struct {
	ID                   string          `json:"id"`
	TelegramUsername     string          `json:"telegram_username"`
	ChatID               int64           `json:"chat_id"`
	Symbols              []string        `json:"symbols"`
	Rules                json.RawMessage `json:"rules"`
	Active               bool            `json:"active"`
	Tier                 string          `json:"tier"`
	StripeCustomerID     string          `json:"stripe_customer_id"`
	StripeSubscriptionID string          `json:"stripe_subscription_id"`
	SubscriptionStatus   string          `json:"subscription_status"`
	ProSince             *time.Time      `json:"pro_since"`
	DiscordWebhookURL    string          `json:"discord_webhook_url"`
}

// subscriberInsert is the insert-only shape — omits id and active so Supabase
// applies its column defaults rather than receiving zero values.
type subscriberInsert struct {
	TelegramUsername string          `json:"telegram_username"`
	ChatID           int64           `json:"chat_id"`
	Symbols          []string        `json:"symbols"`
	Rules            json.RawMessage `json:"rules"`
}

// subscriberUpsert includes active for upsert — on conflict we update and set active=true.
type subscriberUpsert struct {
	TelegramUsername string          `json:"telegram_username"`
	ChatID           int64           `json:"chat_id"`
	Symbols          []string        `json:"symbols"`
	Rules            json.RawMessage `json:"rules"`
	Active           bool            `json:"active"`
}

// alertLogRow mirrors the `alert_log` table in Supabase.
type alertLogRow struct {
	SubscriberID string    `json:"subscriber_id"`
	Symbol       string    `json:"symbol"`
	AlertID      string    `json:"alert_id"`
	SentAt       time.Time `json:"sent_at"`
}

// Client wraps Supabase REST API calls using stdlib net/http only.
type Client struct {
	baseURL    string
	serviceKey string
	httpClient *http.Client
}

func New(baseURL, serviceKey string) *Client {
	return &Client{
		baseURL:    baseURL,
		serviceKey: serviceKey,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// GetActiveSubscribers returns all rows from `subscribers` where active=true.
// GET {baseURL}/rest/v1/subscribers?active=eq.true&select=*
func (c *Client) GetActiveSubscribers(ctx context.Context) ([]Subscriber, error) {
	url := c.baseURL + "/rest/v1/subscribers?active=eq.true&select=id,telegram_username,chat_id,symbols,rules,active,tier,stripe_customer_id,stripe_subscription_id,subscription_status,pro_since,discord_webhook_url"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("supabase: build request: %w", err)
	}
	c.setHeaders(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("supabase: GetActiveSubscribers: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("supabase: GetActiveSubscribers: status %d", resp.StatusCode)
	}

	var subs []Subscriber
	if err := json.NewDecoder(resp.Body).Decode(&subs); err != nil {
		return nil, fmt.Errorf("supabase: GetActiveSubscribers decode: %w", err)
	}
	return subs, nil
}

// GetSubscriberChatID returns the chat_id for a given telegram_username.
// GET {baseURL}/rest/v1/subscribers?telegram_username=eq.{username}&select=chat_id
func (c *Client) GetSubscriberChatID(ctx context.Context, username string) (int64, error) {
	url := fmt.Sprintf("%s/rest/v1/subscribers?telegram_username=ilike.%s&select=chat_id", c.baseURL, url.QueryEscape(strings.ToLower(username)))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, fmt.Errorf("supabase: build request: %w", err)
	}
	c.setHeaders(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("supabase: GetSubscriberChatID: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("supabase: GetSubscriberChatID: status %d", resp.StatusCode)
	}

	var subs []struct {
		ChatID int64 `json:"chat_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&subs); err != nil {
		return 0, fmt.Errorf("supabase: GetSubscriberChatID decode: %w", err)
	}
	if len(subs) == 0 {
		return 0, fmt.Errorf("supabase: no subscriber found for username")
	}
	return subs[0].ChatID, nil
}

// UpdateChatID sets the chat_id for a subscriber identified by telegram_username.
// PATCH {baseURL}/rest/v1/subscribers?telegram_username=eq.{username}
func (c *Client) UpdateChatID(ctx context.Context, username string, chatID int64) error {
	url := fmt.Sprintf("%s/rest/v1/subscribers?telegram_username=eq.%s", c.baseURL, url.QueryEscape(username))
	body, _ := json.Marshal(map[string]any{"chat_id": chatID})

	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("supabase: build request: %w", err)
	}
	c.setHeaders(req)
	req.Header.Set("Prefer", "return=minimal")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("supabase: UpdateChatID: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("supabase: UpdateChatID: status %d", resp.StatusCode)
	}
	return nil
}

// CreateSubscriber upserts a row into the `subscribers` table.
// On conflict (telegram_username), updates chat_id, symbols, rules, and sets active=true.
// POST {baseURL}/rest/v1/subscribers?on_conflict=telegram_username
func (c *Client) CreateSubscriber(ctx context.Context, sub Subscriber) error {
	url := c.baseURL + "/rest/v1/subscribers?on_conflict=telegram_username"
	upsert := subscriberUpsert{
		TelegramUsername: sub.TelegramUsername,
		ChatID:           sub.ChatID,
		Symbols:          sub.Symbols,
		Rules:            sub.Rules,
		Active:           true,
	}
	body, _ := json.Marshal(upsert)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("supabase: build request: %w", err)
	}
	c.setHeaders(req)
	req.Header.Set("Prefer", "return=minimal,resolution=merge-duplicates")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("supabase: CreateSubscriber: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("supabase: CreateSubscriber: status %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// DeleteSubscriber soft-deletes by setting active=false for a subscriber by username.
// PATCH {baseURL}/rest/v1/subscribers?telegram_username=eq.{username}
func (c *Client) DeleteSubscriber(ctx context.Context, username string) error {
	url := fmt.Sprintf("%s/rest/v1/subscribers?telegram_username=eq.%s", c.baseURL, url.QueryEscape(username))
	body, _ := json.Marshal(map[string]any{"active": false})

	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("supabase: build request: %w", err)
	}
	c.setHeaders(req)
	req.Header.Set("Prefer", "return=minimal")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("supabase: DeleteSubscriber: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("supabase: DeleteSubscriber: status %d", resp.StatusCode)
	}
	return nil
}

// UpdateSubscriberTier updates tier and Stripe fields for a subscriber by telegram_username.
func (c *Client) UpdateSubscriberTier(ctx context.Context, telegramUsername, tier, customerID, subscriptionID, status string) error {
	url := fmt.Sprintf("%s/rest/v1/subscribers?telegram_username=ilike.%s", c.baseURL, url.QueryEscape(strings.ToLower(telegramUsername)))
	body := map[string]any{"subscription_status": status}
	if tier != "" {
		body["tier"] = tier
	}
	if customerID != "" {
		body["stripe_customer_id"] = customerID
	}
	if subscriptionID != "" {
		body["stripe_subscription_id"] = subscriptionID
	}
	if tier == "pro" && status == "active" {
		body["pro_since"] = time.Now().UTC().Format(time.RFC3339)
	}
	bodyBytes, _ := json.Marshal(body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return fmt.Errorf("supabase: build request: %w", err)
	}
	c.setHeaders(req)
	req.Header.Set("Prefer", "return=minimal")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("supabase: UpdateSubscriberTier: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("supabase: UpdateSubscriberTier: status %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// UpdateSubscriberTierByStripeID updates tier/status for a subscriber found by stripe_customer_id or stripe_subscription_id.
func (c *Client) UpdateSubscriberTierByStripeID(ctx context.Context, customerID, subscriptionID, tier, status string) error {
	var reqURL string
	if customerID != "" {
		reqURL = fmt.Sprintf("%s/rest/v1/subscribers?stripe_customer_id=eq.%s&select=telegram_username", c.baseURL, url.QueryEscape(customerID))
	} else if subscriptionID != "" {
		reqURL = fmt.Sprintf("%s/rest/v1/subscribers?stripe_subscription_id=eq.%s&select=telegram_username", c.baseURL, url.QueryEscape(subscriptionID))
	} else {
		return fmt.Errorf("supabase: need customer_id or subscription_id")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return fmt.Errorf("supabase: build request: %w", err)
	}
	c.setHeaders(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("supabase: GetByStripeID: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("supabase: GetByStripeID: status %d", resp.StatusCode)
	}

	var subs []struct {
		TelegramUsername string `json:"telegram_username"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&subs); err != nil || len(subs) == 0 {
		return fmt.Errorf("supabase: no subscriber found for stripe id")
	}

	return c.UpdateSubscriberTier(ctx, subs[0].TelegramUsername, tier, customerID, subscriptionID, status)
}

// GetSubscriberStripeCustomerID returns the stripe_customer_id for a username, or empty if none.
func (c *Client) GetSubscriberStripeCustomerID(ctx context.Context, telegramUsername string) (string, error) {
	url := fmt.Sprintf("%s/rest/v1/subscribers?telegram_username=eq.%s&select=stripe_customer_id", c.baseURL, url.QueryEscape(telegramUsername))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("supabase: build request: %w", err)
	}
	c.setHeaders(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("supabase: GetSubscriberStripeCustomerID: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("supabase: GetSubscriberStripeCustomerID: status %d", resp.StatusCode)
	}

	var subs []struct {
		StripeCustomerID string `json:"stripe_customer_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&subs); err != nil {
		return "", fmt.Errorf("supabase: GetSubscriberStripeCustomerID decode: %w", err)
	}
	if len(subs) == 0 || subs[0].StripeCustomerID == "" {
		return "", nil
	}
	return subs[0].StripeCustomerID, nil
}

// GetSubscriberTier returns the tier and status for a username.
func (c *Client) GetSubscriberTier(ctx context.Context, telegramUsername string) (tier, status string, err error) {
	url := fmt.Sprintf("%s/rest/v1/subscribers?telegram_username=ilike.%s&select=tier,subscription_status", c.baseURL, url.QueryEscape(strings.ToLower(telegramUsername)))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", "", fmt.Errorf("supabase: build request: %w", err)
	}
	c.setHeaders(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("supabase: GetSubscriberTier: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("supabase: GetSubscriberTier: status %d", resp.StatusCode)
	}

	var subs []struct {
		Tier               string `json:"tier"`
		SubscriptionStatus string `json:"subscription_status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&subs); err != nil {
		return "", "", fmt.Errorf("supabase: GetSubscriberTier decode: %w", err)
	}
	if len(subs) == 0 {
		return "free", "inactive", nil
	}
	tier = subs[0].Tier
	if tier == "" {
		tier = "free"
	}
	status = subs[0].SubscriptionStatus
	if status == "" {
		status = "inactive"
	}
	return tier, status, nil
}

// isClusterAlertID reports whether alertID belongs to a price-specific cluster alert (zone/magnet/burst).
// Cluster IDs end with a numeric price component (e.g. BTC-zone-84500, BTC-liq-magnet-84000).
// Regime IDs are fixed strings (e.g. BTC-long-bias, BTC-oi-divergence-24h).
func isClusterAlertID(alertID string) bool {
	idx := strings.LastIndex(alertID, "-")
	if idx < 0 {
		return false
	}
	suffix := alertID[idx+1:]
	if len(suffix) == 0 {
		return false
	}
	for _, c := range suffix {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// WasAlertSent returns true if alertID was already logged for subscriberID within the cooldown window.
// Cluster alerts (zones, magnets — contain a price in the ID) use a 30-min window.
// Regime alerts (OI, funding, long/short bias — fixed IDs) use a 4-hour window to prevent spam.
func (c *Client) WasAlertSent(ctx context.Context, subscriberID, alertID string) (bool, error) {
	window := 4 * time.Hour
	// Cluster alerts have price-based IDs (e.g. BTC-zone-84500, BTC-liq-magnet-84000).
	// Use shorter window so a new nearby cluster can still fire after 30 min.
	if isClusterAlertID(alertID) {
		window = 30 * time.Minute
	}
	cutoff := time.Now().UTC().Add(-window).Format(time.RFC3339)
	url := fmt.Sprintf(
		"%s/rest/v1/alert_log?subscriber_id=eq.%s&alert_id=eq.%s&sent_at=gte.%s&select=id",
		c.baseURL, url.QueryEscape(subscriberID), url.QueryEscape(alertID), url.QueryEscape(cutoff),
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, fmt.Errorf("supabase: build request: %w", err)
	}
	c.setHeaders(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("supabase: WasAlertSent: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("supabase: WasAlertSent: status %d", resp.StatusCode)
	}

	// Supabase returns an array; if it's non-empty the alert was already sent.
	var rows []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		return false, fmt.Errorf("supabase: WasAlertSent decode: %w", err)
	}
	return len(rows) > 0, nil
}

// RecentAlertLogEntry holds alert_id and symbol for dedup by rule type.
type RecentAlertLogEntry struct {
	AlertID string `json:"alert_id"`
	Symbol  string `json:"symbol"`
}

// GetRecentAlertLogs returns alert_log rows for subscriberID within the given window.
// Used to suppress same-rule-type alerts when 3+ symbols already sent.
func (c *Client) GetRecentAlertLogs(ctx context.Context, subscriberID string, since time.Time) ([]RecentAlertLogEntry, error) {
	sinceStr := since.Format(time.RFC3339)
	url := fmt.Sprintf(
		"%s/rest/v1/alert_log?subscriber_id=eq.%s&sent_at=gte.%s&select=alert_id,symbol",
		c.baseURL, url.QueryEscape(subscriberID), url.QueryEscape(sinceStr),
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("supabase: build request: %w", err)
	}
	c.setHeaders(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("supabase: GetRecentAlertLogs: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("supabase: GetRecentAlertLogs: status %d", resp.StatusCode)
	}
	var rows []RecentAlertLogEntry
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		return nil, fmt.Errorf("supabase: GetRecentAlertLogs decode: %w", err)
	}
	return rows, nil
}

// AlertOutcomeEntry holds the minimal fields needed for performance aggregation.
type AlertOutcomeEntry struct {
	Severity      string   `json:"severity"`
	Message       string   `json:"message"`
	OutcomePct15m *float64 `json:"outcome_pct_15m"`
	OutcomePct1h  *float64 `json:"outcome_pct_1h"`
}

// GetAlertOutcomes returns alert_history rows that have 1h outcomes, within the given window.
// Used to compute signal performance stats for the landing page.
func (c *Client) GetAlertOutcomes(ctx context.Context, since time.Time) ([]AlertOutcomeEntry, error) {
	sinceStr := since.Format(time.RFC3339)
	u := fmt.Sprintf(
		"%s/rest/v1/alert_history?outcome_pct_1h=not.is.null&triggered_at=gte.%s&select=severity,message,outcome_pct_15m,outcome_pct_1h&limit=2000&order=triggered_at.desc",
		c.baseURL, url.QueryEscape(sinceStr),
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("supabase: build request: %w", err)
	}
	c.setHeaders(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("supabase: GetAlertOutcomes: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("supabase: GetAlertOutcomes: status %d", resp.StatusCode)
	}
	var rows []AlertOutcomeEntry
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		return nil, fmt.Errorf("supabase: GetAlertOutcomes decode: %w", err)
	}
	return rows, nil
}

// LogAlertHistory logs every alert that fires (regardless of subscriber dedup).
// POST {baseURL}/rest/v1/alert_history
func (c *Client) LogAlertHistory(ctx context.Context, symbol, alertID, message, severity string, priceAtAlert float64) error {
	url := c.baseURL + "/rest/v1/alert_history"
	row := map[string]any{
		"symbol":    symbol,
		"alert_id":  alertID,
		"message":   message,
		"severity":  severity,
	}
	if priceAtAlert > 0 {
		row["price_at_alert"] = priceAtAlert
	}
	body, _ := json.Marshal(row)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("supabase: build request: %w", err)
	}
	c.setHeaders(req)
	req.Header.Set("Prefer", "return=minimal")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("supabase: LogAlertHistory: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("supabase: LogAlertHistory: status %d", resp.StatusCode)
	}
	return nil
}

// GetAlertHistory returns the last N alerts for a symbol (or all symbols if symbol is empty).
// GET {baseURL}/rest/v1/alert_history?order=triggered_at.desc&limit=N
func (c *Client) GetAlertHistory(ctx context.Context, symbol string, limit int) ([]models.AlertHistoryEntry, error) {
	reqURL := fmt.Sprintf("%s/rest/v1/alert_history?order=triggered_at.desc&limit=%d&select=id,symbol,alert_id,message,severity,triggered_at,price_at_alert,price_15m,price_1h,outcome_pct_15m,outcome_pct_1h", c.baseURL, limit)
	if symbol != "" {
		reqURL += "&symbol=eq." + url.QueryEscape(symbol)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("supabase: build request: %w", err)
	}
	c.setHeaders(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("supabase: GetAlertHistory: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("supabase: GetAlertHistory: status %d", resp.StatusCode)
	}
	var entries []models.AlertHistoryEntry
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		return nil, fmt.Errorf("supabase: GetAlertHistory decode: %w", err)
	}
	return entries, nil
}

// GetAlertsPendingOutcome returns alert_history rows where the given outcome column is NULL,
// triggered between minAge and maxAge ago, and price_at_alert is non-null.
// outcomeCol must be "price_15m" or "price_1h".
func (c *Client) GetAlertsPendingOutcome(ctx context.Context, outcomeCol string, minAge, maxAge time.Duration) ([]models.AlertHistoryEntry, error) {
	now := time.Now().UTC()
	lt := now.Add(-minAge).Format(time.RFC3339)
	gt := now.Add(-maxAge).Format(time.RFC3339)
	reqURL := fmt.Sprintf(
		"%s/rest/v1/alert_history?%s=is.null&price_at_alert=not.is.null&triggered_at=lt.%s&triggered_at=gt.%s&select=id,symbol,price_at_alert&limit=100",
		c.baseURL, url.QueryEscape(outcomeCol), url.QueryEscape(lt), url.QueryEscape(gt),
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("supabase: build request: %w", err)
	}
	c.setHeaders(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("supabase: GetAlertsPendingOutcome: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("supabase: GetAlertsPendingOutcome: status %d", resp.StatusCode)
	}
	var entries []models.AlertHistoryEntry
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		return nil, fmt.Errorf("supabase: GetAlertsPendingOutcome decode: %w", err)
	}
	return entries, nil
}

// UpdateAlertOutcome patches the outcome price and pct columns for a single alert_history row.
func (c *Client) UpdateAlertOutcome(ctx context.Context, id, priceCol, pctCol string, currentPrice, pctChange float64) error {
	reqURL := fmt.Sprintf("%s/rest/v1/alert_history?id=eq.%s", c.baseURL, url.QueryEscape(id))
	body, _ := json.Marshal(map[string]any{
		priceCol: currentPrice,
		pctCol:   pctChange,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, reqURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("supabase: build request: %w", err)
	}
	c.setHeaders(req)
	req.Header.Set("Prefer", "return=minimal")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("supabase: UpdateAlertOutcome: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("supabase: UpdateAlertOutcome: status %d", resp.StatusCode)
	}
	return nil
}

// LogAlert inserts a row into the `alert_log` table.
// POST {baseURL}/rest/v1/alert_log
func (c *Client) LogAlert(ctx context.Context, subscriberID, symbol, alertID string) error {
	url := c.baseURL + "/rest/v1/alert_log"
	body, _ := json.Marshal(alertLogRow{
		SubscriberID: subscriberID,
		Symbol:       symbol,
		AlertID:      alertID,
		SentAt:       time.Now().UTC(),
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("supabase: build request: %w", err)
	}
	c.setHeaders(req)
	req.Header.Set("Prefer", "return=minimal")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("supabase: LogAlert: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("supabase: LogAlert: status %d", resp.StatusCode)
	}
	return nil
}

// UserSettings holds user preferences stored in user_settings table.
type UserSettings struct {
	Username        string `json:"username"`
	AnthropicAPIKey string `json:"anthropic_api_key"`
	PreferredModel  string `json:"preferred_model"`
}

// GetUserSettings returns user settings for the given username.
func (c *Client) GetUserSettings(ctx context.Context, username string) (*UserSettings, error) {
	reqURL := fmt.Sprintf("%s/rest/v1/user_settings?username=eq.%s&select=username,anthropic_api_key,preferred_model", c.baseURL, url.QueryEscape(username))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("supabase: build request: %w", err)
	}
	c.setHeaders(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("supabase: GetUserSettings: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("supabase: GetUserSettings: status %d", resp.StatusCode)
	}
	var rows []UserSettings
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		return nil, fmt.Errorf("supabase: GetUserSettings decode: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

// SaveUserSettings upserts user settings.
func (c *Client) SaveUserSettings(ctx context.Context, settings UserSettings) error {
	url := c.baseURL + "/rest/v1/user_settings?on_conflict=username"
	body, err := json.Marshal(settings)
	if err != nil {
		return fmt.Errorf("supabase: marshal settings: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("supabase: build request: %w", err)
	}
	c.setHeaders(req)
	req.Header.Set("Prefer", "return=minimal,resolution=merge-duplicates")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("supabase: SaveUserSettings: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("supabase: SaveUserSettings: status %d body=%s", resp.StatusCode, string(bodyBytes))
	}
	return nil
}

// Ping performs a lightweight query to Supabase. Returns "ok" or "error: <message>".
func (c *Client) Ping() string {
	url := c.baseURL + "/rest/v1/subscribers?limit=1&select=id"
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return "error: " + err.Error()
	}
	c.setHeaders(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "error: " + err.Error()
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "error: status " + fmt.Sprint(resp.StatusCode)
	}
	return "ok"
}

// AddToWaitlist inserts a row into the waitlist table.
// Returns error for duplicate email (409) or other failures.
func (c *Client) AddToWaitlist(ctx context.Context, email, tier, username string) error {
	payload := map[string]interface{}{
		"email":    email,
		"tier":     tier,
		"username": username,
	}
	body, _ := json.Marshal(payload)
	url := c.baseURL + "/rest/v1/waitlist"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("supabase: build request: %w", err)
	}
	c.setHeaders(req)
	req.Header.Set("Prefer", "return=minimal")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == 409 {
		return fmt.Errorf("duplicate email")
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("supabase error: %d", resp.StatusCode)
	}
	return nil
}

// UpdateDiscordWebhook sets (or clears) the discord_webhook_url for a subscriber by username.
// Pass an empty string to remove the webhook.
func (c *Client) UpdateDiscordWebhook(ctx context.Context, username, webhookURL string) error {
	reqURL := fmt.Sprintf("%s/rest/v1/subscribers?telegram_username=eq.%s", c.baseURL, url.QueryEscape(username))
	body, _ := json.Marshal(map[string]any{"discord_webhook_url": webhookURL})
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, reqURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("supabase: build request: %w", err)
	}
	c.setHeaders(req)
	req.Header.Set("Prefer", "return=minimal,count=exact")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("supabase: UpdateDiscordWebhook: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("supabase: UpdateDiscordWebhook: status %d", resp.StatusCode)
	}
	// Content-Range: */0 means no subscriber row was found — user hasn't subscribed yet.
	if cr := resp.Header.Get("Content-Range"); cr == "*/0" {
		return fmt.Errorf("subscriber not found: user must subscribe before saving Discord settings")
	}
	return nil
}

// GetDiscordWebhook returns the current discord_webhook_url for a subscriber.
func (c *Client) GetDiscordWebhook(ctx context.Context, username string) (string, error) {
	reqURL := fmt.Sprintf("%s/rest/v1/subscribers?telegram_username=eq.%s&select=discord_webhook_url&limit=1", c.baseURL, url.QueryEscape(username))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return "", fmt.Errorf("supabase: build request: %w", err)
	}
	c.setHeaders(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("supabase: GetDiscordWebhook: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("supabase: GetDiscordWebhook: status %d", resp.StatusCode)
	}
	var rows []struct {
		DiscordWebhookURL string `json:"discord_webhook_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil || len(rows) == 0 {
		return "", nil
	}
	return rows[0].DiscordWebhookURL, nil
}

// GetSubscriberIDByChatID returns the subscriber UUID for the given Telegram chat ID.
// Used to look up a subscriber when only their chat ID is known (e.g. inline button callbacks).
func (c *Client) GetSubscriberIDByChatID(ctx context.Context, chatID int64) (string, error) {
	reqURL := fmt.Sprintf("%s/rest/v1/subscribers?chat_id=eq.%d&active=eq.true&select=id&limit=1", c.baseURL, chatID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return "", fmt.Errorf("supabase: build request: %w", err)
	}
	c.setHeaders(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("supabase: GetSubscriberIDByChatID: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("supabase: GetSubscriberIDByChatID: status %d", resp.StatusCode)
	}
	var rows []struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil || len(rows) == 0 {
		return "", fmt.Errorf("supabase: no subscriber found for chat_id %d", chatID)
	}
	return rows[0].ID, nil
}

// GetSubscriberIDByUsername returns the subscriber UUID for the given Telegram username.
func (c *Client) GetSubscriberIDByUsername(ctx context.Context, username string) (string, string, error) {
	reqURL := fmt.Sprintf("%s/rest/v1/subscribers?telegram_username=eq.%s&select=id,tier&active=eq.true", c.baseURL, url.QueryEscape(username))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return "", "", fmt.Errorf("supabase: build request: %w", err)
	}
	c.setHeaders(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("supabase: GetSubscriberIDByUsername: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("supabase: GetSubscriberIDByUsername: status %d", resp.StatusCode)
	}
	var rows []struct {
		ID   string `json:"id"`
		Tier string `json:"tier"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil || len(rows) == 0 {
		return "", "", fmt.Errorf("supabase: subscriber not found")
	}
	return rows[0].ID, rows[0].Tier, nil
}

// CreateCustomPriceAlert inserts a new custom price alert for a subscriber.
func (c *Client) CreateCustomPriceAlert(ctx context.Context, subscriberID, symbol, direction, note string, targetPrice float64) error {
	reqURL := c.baseURL + "/rest/v1/custom_price_alerts"
	body, _ := json.Marshal(map[string]any{
		"subscriber_id": subscriberID,
		"symbol":        symbol,
		"target_price":  targetPrice,
		"direction":     direction,
		"note":          note,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("supabase: build request: %w", err)
	}
	c.setHeaders(req)
	req.Header.Set("Prefer", "return=minimal")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("supabase: CreateCustomPriceAlert: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("supabase: CreateCustomPriceAlert: status %d", resp.StatusCode)
	}
	return nil
}

// GetCustomPriceAlerts returns active (non-triggered) custom price alerts for a subscriber.
func (c *Client) GetCustomPriceAlerts(ctx context.Context, subscriberID string) ([]models.CustomPriceAlert, error) {
	reqURL := fmt.Sprintf("%s/rest/v1/custom_price_alerts?subscriber_id=eq.%s&triggered=eq.false&order=created_at.desc&select=id,subscriber_id,symbol,target_price,direction,note,triggered,triggered_at,created_at",
		c.baseURL, url.QueryEscape(subscriberID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("supabase: build request: %w", err)
	}
	c.setHeaders(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("supabase: GetCustomPriceAlerts: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("supabase: GetCustomPriceAlerts: status %d", resp.StatusCode)
	}
	var alerts []models.CustomPriceAlert
	if err := json.NewDecoder(resp.Body).Decode(&alerts); err != nil {
		return nil, fmt.Errorf("supabase: GetCustomPriceAlerts decode: %w", err)
	}
	return alerts, nil
}

// DeleteCustomPriceAlert deletes a custom price alert owned by the subscriber.
func (c *Client) DeleteCustomPriceAlert(ctx context.Context, id, subscriberID string) error {
	reqURL := fmt.Sprintf("%s/rest/v1/custom_price_alerts?id=eq.%s&subscriber_id=eq.%s",
		c.baseURL, url.QueryEscape(id), url.QueryEscape(subscriberID))
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, reqURL, nil)
	if err != nil {
		return fmt.Errorf("supabase: build request: %w", err)
	}
	c.setHeaders(req)
	req.Header.Set("Prefer", "return=minimal")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("supabase: DeleteCustomPriceAlert: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("supabase: DeleteCustomPriceAlert: status %d", resp.StatusCode)
	}
	return nil
}

// GetPendingCustomPriceAlerts returns all non-triggered custom price alerts across all subscribers.
func (c *Client) GetPendingCustomPriceAlerts(ctx context.Context) ([]models.CustomPriceAlert, error) {
	reqURL := fmt.Sprintf("%s/rest/v1/custom_price_alerts?triggered=eq.false&select=id,subscriber_id,symbol,target_price,direction,note&limit=500", c.baseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("supabase: build request: %w", err)
	}
	c.setHeaders(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("supabase: GetPendingCustomPriceAlerts: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("supabase: GetPendingCustomPriceAlerts: status %d", resp.StatusCode)
	}
	var alerts []models.CustomPriceAlert
	if err := json.NewDecoder(resp.Body).Decode(&alerts); err != nil {
		return nil, fmt.Errorf("supabase: GetPendingCustomPriceAlerts decode: %w", err)
	}
	return alerts, nil
}

// MarkCustomPriceAlertTriggered marks a custom price alert as triggered.
func (c *Client) MarkCustomPriceAlertTriggered(ctx context.Context, id string) error {
	reqURL := fmt.Sprintf("%s/rest/v1/custom_price_alerts?id=eq.%s", c.baseURL, url.QueryEscape(id))
	body, _ := json.Marshal(map[string]any{
		"triggered":    true,
		"triggered_at": time.Now().UTC().Format(time.RFC3339),
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, reqURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("supabase: build request: %w", err)
	}
	c.setHeaders(req)
	req.Header.Set("Prefer", "return=minimal")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("supabase: MarkCustomPriceAlertTriggered: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("supabase: MarkCustomPriceAlertTriggered: status %d", resp.StatusCode)
	}
	return nil
}

// CountCustomPriceAlerts returns the number of active custom price alerts for a subscriber.
func (c *Client) CountCustomPriceAlerts(ctx context.Context, subscriberID string) (int, error) {
	reqURL := fmt.Sprintf("%s/rest/v1/custom_price_alerts?subscriber_id=eq.%s&triggered=eq.false&select=id", c.baseURL, url.QueryEscape(subscriberID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return 0, fmt.Errorf("supabase: build request: %w", err)
	}
	c.setHeaders(req)
	req.Header.Set("Prefer", "count=exact")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("supabase: CountCustomPriceAlerts: %w", err)
	}
	defer resp.Body.Close()
	// PostgREST returns Content-Range: 0-4/5 — parse the total
	cr := resp.Header.Get("Content-Range")
	if cr == "" {
		var rows []map[string]any
		json.NewDecoder(resp.Body).Decode(&rows) //nolint:errcheck
		return len(rows), nil
	}
	// Format: "0-N/total" or "*/total"
	parts := strings.Split(cr, "/")
	if len(parts) == 2 {
		n, _ := strconv.Atoi(parts[1])
		return n, nil
	}
	return 0, nil
}

// GetSubscriberRules returns the current rules JSONB for a subscriber by username.
func (c *Client) GetSubscriberRules(ctx context.Context, username string) (json.RawMessage, error) {
	reqURL := fmt.Sprintf("%s/rest/v1/subscribers?telegram_username=eq.%s&select=rules&limit=1", c.baseURL, url.QueryEscape(username))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("supabase: build request: %w", err)
	}
	c.setHeaders(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("supabase: GetSubscriberRules: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("supabase: GetSubscriberRules: status %d", resp.StatusCode)
	}
	var rows []struct {
		Rules json.RawMessage `json:"rules"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil || len(rows) == 0 {
		return nil, nil
	}
	return rows[0].Rules, nil
}

// UpdateSubscriberRules patches the rules JSONB for a subscriber by username.
func (c *Client) UpdateSubscriberRules(ctx context.Context, username string, rules json.RawMessage) error {
	reqURL := fmt.Sprintf("%s/rest/v1/subscribers?telegram_username=eq.%s", c.baseURL, url.QueryEscape(username))
	body, _ := json.Marshal(map[string]any{"rules": rules})
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, reqURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("supabase: build request: %w", err)
	}
	c.setHeaders(req)
	req.Header.Set("Prefer", "return=minimal,count=exact")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("supabase: UpdateSubscriberRules: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("supabase: UpdateSubscriberRules: status %d", resp.StatusCode)
	}
	// Content-Range: */0 means no subscriber row was found — user hasn't subscribed yet.
	if cr := resp.Header.Get("Content-Range"); cr == "*/0" {
		return fmt.Errorf("subscriber not found: user must subscribe before saving alert thresholds")
	}
	return nil
}

// PlaybookCooldownRow mirrors the `playbook_cooldowns` table.
type PlaybookCooldownRow struct {
	Key     string    `json:"key"`
	FiredAt time.Time `json:"fired_at"`
	Score   int       `json:"score"`
}

// UpsertPlaybookCooldown saves or updates a playbook cooldown entry.
// Uses ON CONFLICT (key) DO UPDATE so restarts restore the exact last-fired time.
// Requires table: playbook_cooldowns (key TEXT PRIMARY KEY, fired_at TIMESTAMPTZ, score INT)
func (c *Client) UpsertPlaybookCooldown(ctx context.Context, key string, firedAt time.Time, score int) error {
	apiURL := c.baseURL + "/rest/v1/playbook_cooldowns"
	body, _ := json.Marshal(PlaybookCooldownRow{Key: key, FiredAt: firedAt, Score: score})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("supabase: UpsertPlaybookCooldown build: %w", err)
	}
	c.setHeaders(req)
	req.Header.Set("Prefer", "return=minimal,resolution=merge-duplicates")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("supabase: UpsertPlaybookCooldown: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("supabase: UpsertPlaybookCooldown: status %d: %s", resp.StatusCode, b)
	}
	return nil
}

// LoadPlaybookCooldowns returns all cooldown rows from the last `window` duration.
// Called on startup to restore in-memory cooldown state after a restart.
func (c *Client) LoadPlaybookCooldowns(ctx context.Context, window time.Duration) ([]PlaybookCooldownRow, error) {
	since := time.Now().Add(-window).UTC().Format(time.RFC3339)
	apiURL := fmt.Sprintf("%s/rest/v1/playbook_cooldowns?fired_at=gte.%s&select=key,fired_at,score",
		c.baseURL, url.QueryEscape(since))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("supabase: LoadPlaybookCooldowns build: %w", err)
	}
	c.setHeaders(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("supabase: LoadPlaybookCooldowns: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("supabase: LoadPlaybookCooldowns: status %d", resp.StatusCode)
	}
	var rows []PlaybookCooldownRow
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		return nil, fmt.Errorf("supabase: LoadPlaybookCooldowns decode: %w", err)
	}
	return rows, nil
}

// ─── Simulator ───────────────────────────────────────────────────────────────

// SimulatorScoreRow mirrors the simulator_scores table.
type SimulatorScoreRow struct {
	Username     string  `json:"username"`
	DisplayName  string  `json:"display_name"`
	RoundsPlayed int     `json:"rounds_played"`
	Correct      int     `json:"correct"`
	Score        int     `json:"score"`
	BestStreak   int     `json:"best_streak"`
	Accuracy     float64 `json:"accuracy"`
}

// UpsertSimulatorScore inserts or updates a user's simulator score.
func (c *Client) UpsertSimulatorScore(ctx context.Context, row SimulatorScoreRow) error {
	accuracy := 0.0
	if row.RoundsPlayed > 0 {
		accuracy = float64(row.Correct) / float64(row.RoundsPlayed) * 100
	}
	row.Accuracy = accuracy

	body, err := json.Marshal(row)
	if err != nil {
		return fmt.Errorf("supabase: UpsertSimulatorScore marshal: %w", err)
	}
	reqURL := c.baseURL + "/rest/v1/simulator_scores"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("supabase: build request: %w", err)
	}
	c.setHeaders(req)
	req.Header.Set("Prefer", "resolution=merge-duplicates")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("supabase: UpsertSimulatorScore: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("supabase: UpsertSimulatorScore: status %d: %s", resp.StatusCode, b)
	}
	return nil
}

// GetSimulatorLeaderboard returns the top N scores ordered by score descending.
func (c *Client) GetSimulatorLeaderboard(ctx context.Context, limit int) ([]SimulatorScoreRow, error) {
	reqURL := fmt.Sprintf(
		"%s/rest/v1/simulator_scores?select=username,display_name,rounds_played,correct,score,best_streak,accuracy&order=score.desc&limit=%d",
		c.baseURL, limit,
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("supabase: build request: %w", err)
	}
	c.setHeaders(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("supabase: GetSimulatorLeaderboard: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("supabase: GetSimulatorLeaderboard: status %d", resp.StatusCode)
	}
	var rows []SimulatorScoreRow
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		return nil, fmt.Errorf("supabase: GetSimulatorLeaderboard decode: %w", err)
	}
	return rows, nil
}

// ─── Simulator Scenarios ─────────────────────────────────────────────────────

// SimulatorScenarioRow mirrors the simulator_scenarios table.
type SimulatorScenarioRow struct {
	ID          string    `json:"id,omitempty"`
	Symbol      string    `json:"symbol"`
	CapturedAt  time.Time `json:"captured_at"`
	Price       float64   `json:"price"`
	Funding     float64   `json:"funding"`
	OIChange1h  float64   `json:"oi_change_1h"`
	Regime      string    `json:"regime"`
	OITrend     string    `json:"oi_trend"`
	LPIScore    int       `json:"lpi_score"`
	Bias        string    `json:"bias"`
	ClusterSide  *string  `json:"cluster_side,omitempty"`
	ClusterPrice *float64 `json:"cluster_price,omitempty"`
	ClusterSize  *float64 `json:"cluster_size,omitempty"`
	ClusterDist  *float64 `json:"cluster_dist,omitempty"`
	Difficulty  string    `json:"difficulty"`
	SetupType   string    `json:"setup_type"`
	KeySignal   string    `json:"key_signal"`
	Outcome     *string   `json:"outcome,omitempty"`
	OutcomePrice *float64 `json:"outcome_price,omitempty"`
	MovePct     *float64  `json:"move_pct,omitempty"`
}

// SaveSimulatorScenario inserts a new scenario snapshot.
func (c *Client) SaveSimulatorScenario(ctx context.Context, row SimulatorScenarioRow) error {
	body, err := json.Marshal(row)
	if err != nil {
		return fmt.Errorf("supabase: SaveSimulatorScenario marshal: %w", err)
	}
	reqURL := c.baseURL + "/rest/v1/simulator_scenarios"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("supabase: build request: %w", err)
	}
	c.setHeaders(req)
	req.Header.Set("Prefer", "return=minimal")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("supabase: SaveSimulatorScenario: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("supabase: SaveSimulatorScenario: status %d: %s", resp.StatusCode, b)
	}
	return nil
}

// GetUnresolvedScenarios returns scenarios between minAge and maxAge with no outcome.
func (c *Client) GetUnresolvedScenarios(ctx context.Context, minAge, maxAge time.Duration) ([]SimulatorScenarioRow, error) {
	now := time.Now().UTC()
	minTime := now.Add(-maxAge).Format(time.RFC3339)
	maxTime := now.Add(-minAge).Format(time.RFC3339)
	reqURL := fmt.Sprintf(
		"%s/rest/v1/simulator_scenarios?select=*&outcome=is.null&captured_at=gte.%s&captured_at=lte.%s",
		c.baseURL,
		url.QueryEscape(minTime),
		url.QueryEscape(maxTime),
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("supabase: build request: %w", err)
	}
	c.setHeaders(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("supabase: GetUnresolvedScenarios: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("supabase: GetUnresolvedScenarios: status %d: %s", resp.StatusCode, b)
	}
	var rows []SimulatorScenarioRow
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		return nil, fmt.Errorf("supabase: GetUnresolvedScenarios decode: %w", err)
	}
	return rows, nil
}

// ResolveSimulatorScenario sets the outcome on a captured scenario.
func (c *Client) ResolveSimulatorScenario(ctx context.Context, id, outcome string, outcomePrice, movePct float64) error {
	payload := map[string]interface{}{
		"outcome":       outcome,
		"outcome_price": outcomePrice,
		"move_pct":      movePct,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("supabase: ResolveSimulatorScenario marshal: %w", err)
	}
	reqURL := fmt.Sprintf("%s/rest/v1/simulator_scenarios?id=eq.%s", c.baseURL, url.QueryEscape(id))
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, reqURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("supabase: build request: %w", err)
	}
	c.setHeaders(req)
	req.Header.Set("Prefer", "return=minimal")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("supabase: ResolveSimulatorScenario: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("supabase: ResolveSimulatorScenario: status %d: %s", resp.StatusCode, b)
	}
	return nil
}

// GetSimulatorScenario returns recent resolved scenarios for the game.
func (c *Client) GetSimulatorScenario(ctx context.Context, limit int) ([]SimulatorScenarioRow, error) {
	reqURL := fmt.Sprintf(
		"%s/rest/v1/simulator_scenarios?select=*&outcome=not.is.null&order=captured_at.desc&limit=%d",
		c.baseURL, limit,
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("supabase: build request: %w", err)
	}
	c.setHeaders(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("supabase: GetSimulatorScenario: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("supabase: GetSimulatorScenario: status %d: %s", resp.StatusCode, b)
	}
	var rows []SimulatorScenarioRow
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		return nil, fmt.Errorf("supabase: GetSimulatorScenario decode: %w", err)
	}
	return rows, nil
}

// setHeaders applies the standard Supabase auth headers to every request.
func (c *Client) setHeaders(req *http.Request) {
	req.Header.Set("apikey", c.serviceKey)
	req.Header.Set("Authorization", "Bearer "+c.serviceKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
}
