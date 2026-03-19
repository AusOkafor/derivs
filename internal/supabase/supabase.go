package supabase

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
	url := c.baseURL + "/rest/v1/subscribers?active=eq.true&select=id,telegram_username,chat_id,symbols,rules,active,tier,stripe_customer_id,stripe_subscription_id,subscription_status,pro_since"
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
	url := fmt.Sprintf("%s/rest/v1/subscribers?telegram_username=eq.%s&select=chat_id", c.baseURL, url.QueryEscape(username))
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
	url := fmt.Sprintf("%s/rest/v1/subscribers?telegram_username=eq.%s", c.baseURL, url.QueryEscape(telegramUsername))
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
	url := fmt.Sprintf("%s/rest/v1/subscribers?telegram_username=eq.%s&select=tier,subscription_status", c.baseURL, url.QueryEscape(telegramUsername))
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

// WasAlertSent returns true if alertID was already logged for subscriberID within the last 30 minutes.
// Only checks recent window to avoid permanent dedup (silent alert death).
// GET {baseURL}/rest/v1/alert_log?subscriber_id=eq.{id}&alert_id=eq.{alertID}&sent_at=gte.{30minAgo}
func (c *Client) WasAlertSent(ctx context.Context, subscriberID, alertID string) (bool, error) {
	thirtyMinAgo := time.Now().UTC().Add(-30 * time.Minute).Format(time.RFC3339)
	url := fmt.Sprintf(
		"%s/rest/v1/alert_log?subscriber_id=eq.%s&alert_id=eq.%s&sent_at=gte.%s&select=id",
		c.baseURL, url.QueryEscape(subscriberID), url.QueryEscape(alertID), url.QueryEscape(thirtyMinAgo),
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
		return fmt.Errorf("supabase: SaveUserSettings: status %d", resp.StatusCode)
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

// setHeaders applies the standard Supabase auth headers to every request.
func (c *Client) setHeaders(req *http.Request) {
	req.Header.Set("apikey", c.serviceKey)
	req.Header.Set("Authorization", "Bearer "+c.serviceKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
}
