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

// Start launches background goroutines:
// - Free tier: 5-min ticker, BTC symbols only
// - Pro tier: 1-min ticker, all subscribed symbols
// Stops cleanly when ctx is cancelled.
func (w *Worker) Start(ctx context.Context) {
	log.Println("worker: starting free cycle (5min)")
	log.Println("worker: starting pro cycle (1min)")

	freeTicker := time.NewTicker(5 * time.Minute)
	proTicker := time.NewTicker(1 * time.Minute)

	// Run both immediately on start
	go w.runCycleFree(ctx)
	go w.runCyclePro(ctx)

	for {
		select {
		case <-freeTicker.C:
			log.Println("worker: free cycle tick")
			go w.runCycleFree(ctx)
		case <-proTicker.C:
			log.Println("worker: pro cycle tick")
			go w.runCyclePro(ctx)
		case <-ctx.Done():
			log.Println("worker: shutting down")
			freeTicker.Stop()
			proTicker.Stop()
			return
		}
	}
}

// isFreeTier returns true if tier is empty or "free".
func isFreeTier(tier string) bool {
	return tier == "" || tier == "free"
}

// isBTCSymbol returns true for BTC-related symbols (free tier only gets these).
func isBTCSymbol(symbol string) bool {
	return strings.EqualFold(symbol, "BTC")
}

// runCycleFree runs for free-tier subscribers, 5-min interval, BTC symbols only.
func (w *Worker) runCycleFree(ctx context.Context) {
	w.runCycle(ctx, true)
}

// runCyclePro runs for pro-tier subscribers, 1-min interval, all symbols.
func (w *Worker) runCyclePro(ctx context.Context) {
	log.Println("worker: pro cycle starting")
	n := w.runCycle(ctx, false)
	log.Printf("worker: pro cycle found %d pro subscribers", n)
}

// runCycle executes one full notification cycle.
// freeOnly: if true, only free-tier subscribers and BTC symbols; if false, only pro-tier and all symbols.
// Returns the count of filtered subscribers for logging.
func (w *Worker) runCycle(ctx context.Context, freeOnly bool) int {
	sentThisCycle := make(map[string]bool)

	subscribers, err := w.db.GetActiveSubscribers(ctx)
	if err != nil {
		log.Printf("worker: GetActiveSubscribers: %v", err)
		return 0
	}

	// Filter by tier.
	var filtered []supabase.Subscriber
	for _, sub := range subscribers {
		if freeOnly && !isFreeTier(sub.Tier) {
			continue
		}
		if !freeOnly && isFreeTier(sub.Tier) {
			continue
		}
		filtered = append(filtered, sub)
	}
	subscribers = filtered
	if len(subscribers) == 0 {
		return 0
	}

	// Collect symbols; for free tier only BTC.
	symbolSet := make(map[string]struct{})
	for _, sub := range subscribers {
		for _, sym := range sub.Symbols {
			if freeOnly && !isBTCSymbol(sym) {
				continue
			}
			symbolSet[sym] = struct{}{}
		}
	}

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
		alerts := w.detector.Analyze(snap)
		snapshots[sym] = symbolAlerts{detected: alerts}
		if !freeOnly {
			log.Printf("worker: pro cycle found %d alerts for %s", len(alerts), sym)
		}
	}

	for _, sub := range subscribers {
		if sub.ChatID == 0 {
			continue
		}

		for _, sym := range sub.Symbols {
			if freeOnly && !isBTCSymbol(sym) {
				continue
			}
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

				if !freeOnly {
					log.Printf("worker: pro cycle sending alert to %s: %s", sub.TelegramUsername, alert.Message)
				}
				sentThisCycle[cycleKey] = true

				if err := w.db.LogAlert(ctx, sub.ID, sym, alert.ID); err != nil {
					log.Printf("worker: LogAlert(sub=%s, alert=%s): %v", sub.ID, alert.ID, err)
				}
			}
		}
	}
	return len(subscribers)
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
