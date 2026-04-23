package billing

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
)

const lemonSqueezyAPI = "https://api.lemonsqueezy.com/v1"

// LemonSqueezyClient wraps Lemon Squeezy API operations.
type LemonSqueezyClient struct {
	apiKey         string
	webhookSecret  string
	variantBasic   string
	variantPro     string
	storeID        string
	redirectURL    string
	httpClient     *http.Client
}

// NewLemonSqueezyClient creates a Lemon Squeezy client.
func NewLemonSqueezyClient(apiKey, webhookSecret, variantBasic, variantPro, storeID, redirectURL string) *LemonSqueezyClient {
	return &LemonSqueezyClient{
		apiKey:        apiKey,
		webhookSecret: webhookSecret,
		variantBasic:  variantBasic,
		variantPro:    variantPro,
		storeID:      storeID,
		redirectURL:   redirectURL,
		httpClient:    &http.Client{},
	}
}

// CreateCheckout creates a Lemon Squeezy checkout and returns the checkout URL.
// tier is "basic" or "pro". telegramUsername is passed as custom data for webhook.
func (c *LemonSqueezyClient) CreateCheckout(telegramUsername, tier string) (string, error) {
	if c.apiKey == "" || c.storeID == "" {
		return "", fmt.Errorf("billing: Lemon Squeezy not configured")
	}
	variantID := c.variantPro
	if tier == "basic" {
		variantID = c.variantBasic
	}
	if variantID == "" {
		return "", fmt.Errorf("billing: variant not configured for tier %s", tier)
	}

	// Lemon Squeezy requires telegram_username to be a non-empty string in custom data.
	// Webhook uses strings.TrimSpace; whitespace-only becomes empty and resolves from buyer email.
	customTelegram := telegramUsername
	if strings.TrimSpace(customTelegram) == "" {
		customTelegram = " "
	}

	body := map[string]interface{}{
		"data": map[string]interface{}{
			"type": "checkouts",
			"attributes": map[string]interface{}{
				"product_options": map[string]interface{}{
					"redirect_url": c.redirectURL + "/dashboard?upgraded=true",
				},
				"checkout_data": map[string]interface{}{
					"custom": map[string]interface{}{
						"telegram_username": customTelegram,
						"tier":               tier,
					},
				},
			},
			"relationships": map[string]interface{}{
				"store": map[string]interface{}{
					"data": map[string]interface{}{
						"type": "stores",
						"id":   c.storeID,
					},
				},
				"variant": map[string]interface{}{
					"data": map[string]interface{}{
						"type": "variants",
						"id":   variantID,
					},
				},
			},
		},
	}
	jsonBody, _ := json.Marshal(body)

	req, err := http.NewRequest(http.MethodPost, lemonSqueezyAPI+"/checkouts", bytes.NewReader(jsonBody))
	if err != nil {
		return "", fmt.Errorf("billing: CreateCheckout request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.api+json")
	req.Header.Set("Content-Type", "application/vnd.api+json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("billing: CreateCheckout: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("billing: Lemon Squeezy API %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Data struct {
			Attributes struct {
				URL string `json:"url"`
			} `json:"attributes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("billing: CreateCheckout decode: %w", err)
	}
	if result.Data.Attributes.URL == "" {
		return "", fmt.Errorf("billing: no URL in checkout response")
	}
	return result.Data.Attributes.URL, nil
}

// lemonSqueezyWebhookPayload wraps the webhook payload structure.
// meta.custom_data contains our checkout custom data (telegram_username, tier).
// data varies by event: orders have first_order_item, subscriptions have attributes directly.
type lemonSqueezyWebhookPayload struct {
	Meta struct {
		EventName  string `json:"event_name"`
		CustomData struct {
			TelegramUsername string `json:"telegram_username"`
			Tier             string `json:"tier"`
		} `json:"custom_data"`
	} `json:"meta"`
	Data struct {
		Type       string `json:"type"`
		ID         string `json:"id"`
		Attributes struct {
			Status     string `json:"status"`
			Identifier string `json:"identifier"`
			UserEmail  string `json:"user_email"`
		} `json:"attributes"`
		Relationships struct {
			Subscriptions struct {
				Data []struct {
					ID string `json:"id"`
				} `json:"data"`
			} `json:"subscriptions"`
		} `json:"relationships"`
	} `json:"data"`
}

// HandleWebhook processes Lemon Squeezy webhook events.
// Verifies X-Signature using HMAC-SHA256.
func (c *LemonSqueezyClient) HandleWebhook(payload []byte, sigHeader string, updateFn func(WebhookUpdate)) error {
	if c.webhookSecret == "" {
		return fmt.Errorf("billing: webhook secret not configured")
	}

	mac := hmac.New(sha256.New, []byte(c.webhookSecret))
	mac.Write(payload)
	expected := hex.EncodeToString(mac.Sum(nil))
	if sigHeader != expected {
		return fmt.Errorf("billing: webhook signature invalid")
	}

	var p lemonSqueezyWebhookPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return fmt.Errorf("billing: webhook unmarshal: %w", err)
	}

	// Extract custom data from meta.custom_data
	username := p.Meta.CustomData.TelegramUsername
	tier := p.Meta.CustomData.Tier
	if tier == "" {
		tier = "pro"
	}

	// For order events: data.relationships.subscriptions has subscription id
	// For subscription events: data.id is the subscription id, data.attributes has status
	subID := p.Data.ID
	if p.Data.Type == "orders" && len(p.Data.Relationships.Subscriptions.Data) > 0 {
		subID = p.Data.Relationships.Subscriptions.Data[0].ID
	}

	resolveUsername := func() string {
		u := strings.TrimSpace(username)
		if u != "" {
			return u
		}
		email := strings.TrimSpace(p.Data.Attributes.UserEmail)
		if email != "" {
			return EmailToPlaceholderTelegramUsername(email)
		}
		return ""
	}

	switch p.Meta.EventName {
	case "order_created":
		u := resolveUsername()
		if p.Data.Attributes.Status == "paid" && u != "" {
			updateFn(WebhookUpdate{
				EventType:        "order_created",
				TelegramUsername: u,
				Tier:             tier,
				CustomerID:       p.Data.Attributes.Identifier,
				SubscriptionID:   subID,
				Status:           "active",
			})
		}

	case "subscription_created":
		u := resolveUsername()
		if u != "" {
			updateFn(WebhookUpdate{
				EventType:        "subscription_created",
				TelegramUsername: u,
				Tier:             tier,
				CustomerID:       p.Data.Attributes.Identifier,
				SubscriptionID:   subID,
				Status:           "active",
			})
		}

	case "subscription_updated":
		updateFn(WebhookUpdate{
			EventType:      "subscription_updated",
			CustomerID:     p.Data.Attributes.Identifier,
			SubscriptionID: subID,
			Status:         p.Data.Attributes.Status,
		})

	case "subscription_expired", "subscription_cancelled":
		updateFn(WebhookUpdate{
			EventType:      "subscription_expired",
			Tier:           "free",
			CustomerID:     p.Data.Attributes.Identifier,
			SubscriptionID: subID,
			Status:         "inactive",
		})

	default:
		log.Printf("billing: Lemon Squeezy unhandled event: %s", p.Meta.EventName)
	}

	return nil
}
