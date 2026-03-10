package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"derivs-backend/internal/aggregator"
	"derivs-backend/internal/alerts"
	"derivs-backend/internal/models"
	"derivs-backend/internal/notify"
	"derivs-backend/internal/supabase"
)

// Worker runs a background ticker that fetches market data, detects anomalies,
// and dispatches Telegram notifications to active subscribers.
type Worker struct {
	aggregator *aggregator.Aggregator
	detector   *alerts.Detector
	notifier   *notify.TelegramNotifier
	db         *supabase.Client
}

func New(
	agg *aggregator.Aggregator,
	det *alerts.Detector,
	not *notify.TelegramNotifier,
	db *supabase.Client,
) *Worker {
	return &Worker{
		aggregator: agg,
		detector:   det,
		notifier:   not,
		db:         db,
	}
}

// Start launches the background goroutine with a 5-minute ticker.
// Runs one cycle immediately on start, then every 5 minutes.
// Stops cleanly when ctx is cancelled.
func (w *Worker) Start(ctx context.Context) {
	log.Println("worker: started")
	w.runCycle(ctx)

	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			w.runCycle(ctx)
		case <-ctx.Done():
			log.Println("worker: stopped")
			return
		}
	}
}

// runCycle executes one full notification cycle.
func (w *Worker) runCycle(ctx context.Context) {
	// Track alerts sent THIS cycle to prevent duplicates within the same run.
	// key: "subscriberID:alertID"
	sentThisCycle := make(map[string]bool)

	subscribers, err := w.db.GetActiveSubscribers(ctx)
	if err != nil {
		log.Printf("worker: GetActiveSubscribers: %v", err)
		return
	}
	if len(subscribers) == 0 {
		return
	}

	// Collect all unique symbols needed across all subscribers.
	symbolSet := make(map[string]struct{})
	for _, sub := range subscribers {
		for _, sym := range sub.Symbols {
			symbolSet[sym] = struct{}{}
		}
	}

	// Fetch snapshots and detect alerts for each unique symbol.
	type symbolAlerts struct {
		detected []models.Alert
	}
	snapshots := make(map[string]symbolAlerts, len(symbolSet))

	for sym := range symbolSet {
		snap, err := w.aggregator.FetchSnapshot(ctx, sym)
		if err != nil {
			log.Printf("worker: FetchSnapshot(%s): %v", sym, err)
			continue
		}
		snapshots[sym] = symbolAlerts{
			detected: w.detector.Analyze(snap),
		}
	}

	// Dispatch alerts to each subscriber.
	for _, sub := range subscribers {
		if sub.ChatID == 0 {
			// No chat ID yet — subscriber hasn't started the bot.
			continue
		}

		for _, sym := range sub.Symbols {
			sa, ok := snapshots[sym]
			if !ok {
				continue
			}

			for _, alert := range sa.detected {
				if !w.shouldSendAlert(sub.Rules, alert) {
					continue
				}

				cycleKey := fmt.Sprintf("%s:%s", sub.ID, alert.ID)
				if sentThisCycle[cycleKey] {
					continue
				}

				alreadySent, err := w.db.WasAlertSent(ctx, sub.ID, alert.ID)
				if err != nil {
					log.Printf("worker: WasAlertSent(sub=%s, alert=%s): %v", sub.ID, alert.ID, err)
					continue
				}
				if alreadySent {
					continue
				}

				msg := w.notifier.FormatAlert(sym, alert)
				if err := w.notifier.SendMessage(ctx, sub.ChatID, msg); err != nil {
					log.Printf("worker: SendMessage(chat=%d): %v", sub.ChatID, err)
					continue
				}

				sentThisCycle[cycleKey] = true

				if err := w.db.LogAlert(ctx, sub.ID, sym, alert.ID); err != nil {
					log.Printf("worker: LogAlert(sub=%s, alert=%s): %v", sub.ID, alert.ID, err)
				}
			}
		}
	}
}

// shouldSendAlert checks whether a subscriber's rules JSONB permits a given alert.
// Rules is expected to be an object whose keys map to bool, e.g.:
//
//	{"funding_spike": true, "oi_divergence": false, "liquidation_cluster": true}
//
// An alert passes if its matching rule key is absent (nil = allow) or true.
func (w *Worker) shouldSendAlert(rules json.RawMessage, alert models.Alert) bool {
	if len(rules) == 0 {
		return true // no rules configured → send everything
	}

	var ruleMap map[string]bool
	if err := json.Unmarshal(rules, &ruleMap); err != nil {
		// Unparseable rules — fail open so the subscriber still receives alerts.
		return true
	}

	ruleKey := alertRuleKey(alert.Message)
	if ruleKey == "" {
		return true // unrecognised alert type → send it
	}

	enabled, exists := ruleMap[ruleKey]
	if !exists {
		return true // rule not configured → allow by default
	}
	return enabled
}

// alertRuleKey maps an alert message to its rule key using keyword matching.
func alertRuleKey(message string) string {
	msg := strings.ToLower(message)
	switch {
	case strings.Contains(msg, "funding rate"):
		return "funding_spike"
	case strings.Contains(msg, "oi") || strings.Contains(msg, "open interest"):
		return "oi_divergence"
	case strings.Contains(msg, "long bias"):
		return "long_bias"
	case strings.Contains(msg, "short bias"):
		return "short_bias"
	case strings.Contains(msg, "liquidation"):
		return "liquidation_cluster"
	case strings.Contains(msg, "negative"):
		return "negative_funding"
	default:
		return ""
	}
}
