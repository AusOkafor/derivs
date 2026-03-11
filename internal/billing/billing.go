package billing

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"

	"github.com/stripe/stripe-go/v76"
	"github.com/stripe/stripe-go/v76/checkout/session"
	"github.com/stripe/stripe-go/v76/webhook"
)

// StripeClient wraps Stripe operations.
type StripeClient struct {
	secretKey     string
	webhookSecret string
}

// New creates a StripeClient.
func New(secretKey, webhookSecret string) *StripeClient {
	return &StripeClient{
		secretKey:     secretKey,
		webhookSecret: webhookSecret,
	}
}

// CreateCheckoutSession creates a Stripe checkout session for subscription.
// priceID is the Stripe price ID (basic or pro). plan is stored in metadata for webhook tier resolution.
func (s *StripeClient) CreateCheckoutSession(telegramUsername, priceID, plan string) (string, error) {
	if s.secretKey == "" || priceID == "" {
		return "", fmt.Errorf("billing: Stripe not configured")
	}
	stripe.Key = s.secretKey

	params := &stripe.CheckoutSessionParams{
		Mode: stripe.String(string(stripe.CheckoutSessionModeSubscription)),
		LineItems: []*stripe.CheckoutSessionLineItemParams{
			{
				Price:    stripe.String(priceID),
				Quantity: stripe.Int64(1),
			},
		},
		SuccessURL: stripe.String("https://derivlens-pro.vercel.app/dashboard?upgraded=true"),
		CancelURL:  stripe.String("https://derivlens-pro.vercel.app/dashboard"),
		Metadata: map[string]string{
			"telegram_username": telegramUsername,
			"plan":              plan,
		},
	}

	sess, err := session.New(params)
	if err != nil {
		return "", fmt.Errorf("billing: CreateCheckoutSession: %w", err)
	}
	if sess.URL == "" {
		return "", fmt.Errorf("billing: no URL in checkout session")
	}
	return sess.URL, nil
}

// OnCheckoutCompleted is called when checkout.session.completed fires.
// Returns telegram_username, tier, customerID, subscriptionID for the handler to update Supabase.
// Tier is "basic" or "pro" from session metadata (plan).
func (s *StripeClient) OnCheckoutCompleted(sess *stripe.CheckoutSession) (telegramUsername, tier, customerID, subscriptionID string) {
	if sess.Metadata != nil {
		telegramUsername = sess.Metadata["telegram_username"]
		tier = sess.Metadata["plan"]
		if tier == "" {
			tier = "pro"
		}
	} else {
		tier = "pro"
	}
	if sess.Customer != nil {
		customerID = sess.Customer.ID
	}
	if sess.Subscription != nil {
		subscriptionID = sess.Subscription.ID
	}
	return telegramUsername, tier, customerID, subscriptionID
}

// WebhookUpdate holds data to update a subscriber from a webhook event.
type WebhookUpdate struct {
	EventType        string
	TelegramUsername string
	Tier             string
	CustomerID       string
	SubscriptionID   string
	Status           string
}

// HandleWebhook processes Stripe webhook events.
// Handles: checkout.session.completed, customer.subscription.deleted, customer.subscription.updated
// Calls updateFn with the update data; the handler uses it to update Supabase.
func (s *StripeClient) HandleWebhook(payload []byte, sigHeader string, updateFn func(WebhookUpdate)) error {
	if s.webhookSecret == "" {
		return fmt.Errorf("billing: webhook secret not configured")
	}

	evt, err := webhook.ConstructEventWithOptions(payload, sigHeader, s.webhookSecret,
		webhook.ConstructEventOptions{
			IgnoreAPIVersionMismatch: true,
		})
	if err != nil {
		return fmt.Errorf("billing: webhook signature invalid: %w", err)
	}

	switch evt.Type {
	case stripe.EventTypeCheckoutSessionCompleted:
		var sess stripe.CheckoutSession
		if err := json.Unmarshal(evt.Data.Raw, &sess); err != nil {
			log.Printf("billing: checkout.session.completed unmarshal: %v", err)
			return nil
		}
		username, tier, custID, subID := s.OnCheckoutCompleted(&sess)
		if username != "" {
			updateFn(WebhookUpdate{
				EventType:        "checkout.session.completed",
				TelegramUsername: username,
				Tier:             tier,
				CustomerID:       custID,
				SubscriptionID:   subID,
				Status:           "active",
			})
		}

	case stripe.EventTypeCustomerSubscriptionDeleted:
		var sub stripe.Subscription
		if err := json.Unmarshal(evt.Data.Raw, &sub); err != nil {
			log.Printf("billing: customer.subscription.deleted unmarshal: %v", err)
			return nil
		}
		custID := ""
		if sub.Customer != nil {
			custID = sub.Customer.ID
		}
		updateFn(WebhookUpdate{
			EventType:      "customer.subscription.deleted",
			Tier:           "free",
			CustomerID:     custID,
			SubscriptionID: sub.ID,
			Status:         "inactive",
		})

	case stripe.EventTypeCustomerSubscriptionUpdated:
		var sub stripe.Subscription
		if err := json.Unmarshal(evt.Data.Raw, &sub); err != nil {
			log.Printf("billing: customer.subscription.updated unmarshal: %v", err)
			return nil
		}
		custID := ""
		if sub.Customer != nil {
			custID = sub.Customer.ID
		}
		updateFn(WebhookUpdate{
			EventType:      "customer.subscription.updated",
			CustomerID:     custID,
			SubscriptionID: sub.ID,
			Status:         string(sub.Status),
		})
	}

	return nil
}

// ReadWebhookPayload reads the raw body from an HTTP request.
func ReadWebhookPayload(r *http.Request) ([]byte, error) {
	return io.ReadAll(r.Body)
}
