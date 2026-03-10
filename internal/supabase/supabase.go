package supabase

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
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

// UpdateChatID sets the chat_id for a subscriber identified by telegram_username.
// PATCH {baseURL}/rest/v1/subscribers?telegram_username=eq.{username}
func (c *Client) UpdateChatID(ctx context.Context, username string, chatID int64) error {
	url := fmt.Sprintf("%s/rest/v1/subscribers?telegram_username=eq.%s", c.baseURL, username)
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
	url := fmt.Sprintf("%s/rest/v1/subscribers?telegram_username=eq.%s", c.baseURL, username)
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
	url := fmt.Sprintf("%s/rest/v1/subscribers?telegram_username=eq.%s", c.baseURL, telegramUsername)
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
	var url string
	if customerID != "" {
		url = fmt.Sprintf("%s/rest/v1/subscribers?stripe_customer_id=eq.%s&select=telegram_username", c.baseURL, customerID)
	} else if subscriptionID != "" {
		url = fmt.Sprintf("%s/rest/v1/subscribers?stripe_subscription_id=eq.%s&select=telegram_username", c.baseURL, subscriptionID)
	} else {
		return fmt.Errorf("supabase: need customer_id or subscription_id")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
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

// GetSubscriberTier returns the tier and status for a username.
func (c *Client) GetSubscriberTier(ctx context.Context, telegramUsername string) (tier, status string, err error) {
	url := fmt.Sprintf("%s/rest/v1/subscribers?telegram_username=eq.%s&select=tier,subscription_status", c.baseURL, telegramUsername)
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

// WasAlertSent returns true if alertID was already logged for subscriberID within the last hour.
// GET {baseURL}/rest/v1/alert_log?subscriber_id=eq.{id}&alert_id=eq.{alertID}&sent_at=gte.{1hrAgo}
func (c *Client) WasAlertSent(ctx context.Context, subscriberID, alertID string) (bool, error) {
	oneHourAgo := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	url := fmt.Sprintf(
		"%s/rest/v1/alert_log?subscriber_id=eq.%s&alert_id=eq.%s&sent_at=gte.%s&select=id",
		c.baseURL, subscriberID, alertID, oneHourAgo,
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

// setHeaders applies the standard Supabase auth headers to every request.
func (c *Client) setHeaders(req *http.Request) {
	req.Header.Set("apikey", c.serviceKey)
	req.Header.Set("Authorization", "Bearer "+c.serviceKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
}
