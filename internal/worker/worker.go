package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/getsentry/sentry-go"
	"derivs-backend/internal/aggregator"
	"derivs-backend/internal/alerts"
	"derivs-backend/internal/config"
	"derivs-backend/internal/feargreed"
	"derivs-backend/internal/models"
	"derivs-backend/internal/notify"
	"derivs-backend/internal/signals"
	"derivs-backend/internal/supabase"
)

const (
	maxAlertsPerSymbol = 3
	maxAlertsPerCycle  = 8
	ruleTypeWindow     = 30 * time.Minute
	maxSymbolsPerRule  = 3
)

var severityRank = map[string]int{"high": 0, "medium": 1, "low": 2}
var numericSuffix = regexp.MustCompile(`^\d+$`)

// Worker runs a background ticker that fetches market data, detects anomalies,
// and dispatches Telegram notifications to active subscribers.
type Worker struct {
	aggregator *aggregator.Aggregator
	detector   *alerts.Detector
	notifier   *notify.TelegramNotifier
	db         *supabase.Client
	calc       *feargreed.Calculator
	running    atomic.Bool
	freeRunning atomic.Bool
	proRunning  atomic.Bool
}

func New(
	agg *aggregator.Aggregator,
	det *alerts.Detector,
	not *notify.TelegramNotifier,
	db *supabase.Client,
	calc *feargreed.Calculator,
) *Worker {
	return &Worker{
		aggregator: agg,
		detector:   det,
		notifier:   not,
		db:         db,
		calc:       calc,
	}
}

// scheduleDaily runs fn at target hour:minute UTC, then every 24h.
func scheduleDaily(target time.Time, fn func()) {
	now := time.Now().UTC()
	next := time.Date(now.Year(), now.Month(), now.Day(),
		target.Hour(), target.Minute(), 0, 0, time.UTC)
	if now.After(next) || now.Equal(next) {
		next = next.Add(24 * time.Hour)
	}
	delay := next.Sub(now)
	log.Printf("worker: morning brief scheduled in %v", delay)
	time.AfterFunc(delay, func() {
		fn()
		ticker := time.NewTicker(24 * time.Hour)
		go func() {
			for range ticker.C {
				fn()
			}
		}()
	})
}

// IsRunning returns true if the worker has been started and is still running.
func (w *Worker) IsRunning() bool {
	return w.running.Load()
}

// Start launches background goroutines:
// - Free tier: 5-min ticker, BTC symbols only
// - Pro tier: 1-min ticker, all subscribed symbols
// - Morning brief: 08:00 UTC daily
// Stops cleanly when ctx is cancelled.
func (w *Worker) Start(ctx context.Context) {
	w.running.Store(true)
	defer w.running.Store(false)
	log.Println("worker: starting free cycle (5min)")
	log.Println("worker: starting pro cycle (1min)")

	scheduleDaily(time.Date(0, 1, 1, 8, 0, 0, 0, time.UTC), func() {
		go w.SendMorningBrief(context.Background())
	})

	freeTicker := time.NewTicker(5 * time.Minute)
	proTicker := time.NewTicker(1 * time.Minute)
	topTargetTicker := time.NewTicker(30 * time.Minute)
	defer topTargetTicker.Stop()

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
		case <-topTargetTicker.C:
			go w.broadcastTopTarget(ctx)
		case <-ctx.Done():
			log.Println("worker: shutting down")
			freeTicker.Stop()
			proTicker.Stop()
			return
		}
	}
}

// isProTier returns true if tier is "pro".
func isProTier(tier string) bool {
	return tier == "pro"
}

// allowedSymbols returns the symbols a subscriber is allowed to receive alerts for.
// Free: BTC, ETH, SOL. Basic: up to 5 symbols. Pro: all symbols.
func allowedSymbols(sub supabase.Subscriber) []string {
	freeSymbols := []string{"BTC", "ETH", "SOL"}
	switch sub.Tier {
	case "pro":
		return sub.Symbols
	case "basic":
		if len(sub.Symbols) > 5 {
			return sub.Symbols[:5]
		}
		return sub.Symbols
	default: // free or empty
		allowed := []string{}
		for _, s := range sub.Symbols {
			for _, f := range freeSymbols {
				if strings.EqualFold(s, f) {
					allowed = append(allowed, s)
					break
				}
			}
		}
		return allowed
	}
}

// runCycleFree runs for free and basic tier subscribers, 5-min interval.
// Free: BTC, ETH, SOL. Basic: up to 5 symbols.
func (w *Worker) runCycleFree(ctx context.Context) {
	if !w.freeRunning.CompareAndSwap(false, true) {
		log.Println("[worker] free cycle already running, skipping")
		return
	}
	defer w.freeRunning.Store(false)
	w.runCycle(ctx, false) // freeCycle = not pro
}

// runCyclePro runs for pro-tier subscribers only, 1-min interval, all symbols.
func (w *Worker) runCyclePro(ctx context.Context) {
	log.Println("worker: pro cycle starting")
	n := w.runCycle(ctx, true) // proCycle
	log.Printf("worker: pro cycle found %d pro subscribers", n)
}

// runCycle executes one full notification cycle.
// proOnly: if true, only pro-tier subscribers; if false, free and basic tiers.
// Returns the count of filtered subscribers for logging.
func (w *Worker) runCycle(ctx context.Context, proOnly bool) int {
	defer func() {
		if r := recover(); r != nil {
			sentry.CurrentHub().Recover(r)
			sentry.Flush(2 * time.Second)
			log.Printf("worker panic recovered: %v", r)
		}
	}()
	sentThisCycle := make(map[string]bool)

	subscribers, err := w.db.GetActiveSubscribers(ctx)
	if err != nil {
		log.Printf("worker: GetActiveSubscribers: %v", err)
		return 0
	}

	// Filter by tier.
	var filtered []supabase.Subscriber
	for _, sub := range subscribers {
		tier := sub.Tier
		if tier == "" {
			tier = "free"
		}
		if proOnly && !isProTier(tier) {
			continue
		}
		if !proOnly && isProTier(tier) {
			continue
		}
		filtered = append(filtered, sub)
	}
	subscribers = filtered
	if len(subscribers) == 0 {
		return 0
	}

	// Collect symbols using allowedSymbols per subscriber.
	symbolSet := make(map[string]struct{})
	for _, sub := range subscribers {
		for _, sym := range allowedSymbols(sub) {
			symbolSet[sym] = struct{}{}
		}
	}

	type symbolAlerts struct {
		detected []models.Alert
	}
	snapshots := make(map[string]symbolAlerts, len(symbolSet))

	engine := signals.New()
	for sym := range symbolSet {
		snap, err := w.aggregator.FetchSnapshot(ctx, sym)
		if err != nil {
			log.Printf("worker: FetchSnapshot(%s): %v", sym, err)
			continue
		}
		sigs := engine.Analyze(snap, 0)
		alerts := w.detector.Analyze(snap, sigs)
		snapshots[sym] = symbolAlerts{detected: alerts}
		if proOnly {
			log.Printf("worker: pro cycle found %d alerts for %s", len(alerts), sym)
		}
		// Log every alert that fires to alert_history (regardless of subscriber dedup)
		for _, alert := range alerts {
			if err := w.db.LogAlertHistory(ctx, sym, alert.ID, alert.Message, alert.Severity); err != nil {
				log.Printf("worker: LogAlertHistory(%s): %v", sym, err)
			}
		}
	}

	for _, sub := range subscribers {
		if sub.ChatID == 0 {
			continue
		}

		// Collect all (symbol, alert) pairs that pass rules filter
		type symAlert struct {
			sym   string
			alert models.Alert
		}
		var candidates []symAlert
		for _, sym := range allowedSymbols(sub) {
			sa, ok := snapshots[sym]
			if !ok {
				continue
			}
			for _, alert := range sa.detected {
				if !w.shouldSendAlert(sub.Rules, alert) {
					continue
				}
				candidates = append(candidates, symAlert{sym: sym, alert: alert})
			}
		}

		// Sort by severity (high first, then medium, then low)
		sort.Slice(candidates, func(i, j int) bool {
			ri := severityRank[candidates[i].alert.Severity]
			rj := severityRank[candidates[j].alert.Severity]
			if ri != rj {
				return ri < rj
			}
			return candidates[i].alert.ID < candidates[j].alert.ID
		})

		// Build rule-type symbol count from recent alert_log (last 30 min)
		ruleTypeSymbols := make(map[string]map[string]struct{})
		recentLogs, err := w.db.GetRecentAlertLogs(ctx, sub.ID, time.Now().UTC().Add(-ruleTypeWindow))
		if err != nil {
			log.Printf("worker: GetRecentAlertLogs(sub=%s): %v", sub.ID, err)
		} else {
			for _, e := range recentLogs {
				rt := ruleTypeFromAlertID(e.AlertID)
				if ruleTypeSymbols[rt] == nil {
					ruleTypeSymbols[rt] = make(map[string]struct{})
				}
				ruleTypeSymbols[rt][e.Symbol] = struct{}{}
			}
		}

		sentPerSymbol := make(map[string]int)
		sentTotal := 0

		for _, ca := range candidates {
			if sentTotal >= maxAlertsPerCycle {
				break
			}
			if sentPerSymbol[ca.sym] >= maxAlertsPerSymbol {
				continue
			}

			rt := ruleTypeFromAlertID(ca.alert.ID)
			symbolsForRule := len(ruleTypeSymbols[rt])
			if symbolsForRule >= maxSymbolsPerRule {
				if proOnly {
					log.Printf("worker: pro cycle skipping %s (rule type %s already sent for %d symbols)", ca.alert.ID, rt, symbolsForRule)
				}
				continue
			}

			cycleKey := fmt.Sprintf("%s:%s", sub.ID, ca.alert.ID)
			if sentThisCycle[cycleKey] {
				continue
			}

			alreadySent, err := w.db.WasAlertSent(ctx, sub.ID, ca.alert.ID)
			if err != nil {
				log.Printf("worker: WasAlertSent(sub=%s, alert=%s): %v", sub.ID, ca.alert.ID, err)
				continue
			}
			if alreadySent {
				continue
			}

			msg := w.notifier.FormatAlert(ca.sym, ca.alert)
			if err := w.notifier.SendMessage(ctx, sub.ChatID, msg); err != nil {
				log.Printf("worker: SendMessage(chat=%d): %v", sub.ChatID, err)
				continue
			}

			if proOnly {
				log.Printf("worker: pro cycle sending alert to %s: %s", sub.TelegramUsername, ca.alert.Message)
			}
			sentThisCycle[cycleKey] = true
			sentPerSymbol[ca.sym]++
			sentTotal++

			if ruleTypeSymbols[rt] == nil {
				ruleTypeSymbols[rt] = make(map[string]struct{})
			}
			ruleTypeSymbols[rt][ca.sym] = struct{}{}

			if err := w.db.LogAlert(ctx, sub.ID, ca.sym, ca.alert.ID); err != nil {
				log.Printf("worker: LogAlert(sub=%s, alert=%s): %v", sub.ID, ca.alert.ID, err)
			}
		}
	}
	return len(subscribers)
}

func (w *Worker) broadcastTopTarget(ctx context.Context) {
	type TargetCandidate struct {
		Symbol  string
		Score   float64
		Snap    models.MarketSnapshot
		Signals models.MarketSignals
		Alert   *models.Alert
	}

	var candidates []TargetCandidate
	var mu sync.Mutex

	engine := signals.New()
	for _, symbol := range config.DefaultSymbols {
		snap, err := w.aggregator.FetchSnapshot(ctx, symbol)
		if err != nil {
			continue
		}

		sigs := engine.Analyze(snap, 0)
		alerts := w.detector.Analyze(snap, sigs)

		magnet := sigs.LiquidationMagnet
		if magnet == nil {
			continue
		}

		distance := magnet.Distance
		if distance < 0.0001 {
			distance = 0.0001
		}

		gravityDom := sigs.LiquidityGravity.UpwardPull
		if sigs.LiquidityGravity.DownwardPull > gravityDom {
			gravityDom = sigs.LiquidityGravity.DownwardPull
		}

		score := (magnet.SizeUSD / distance) *
			(float64(magnet.Probability) / 100) *
			(gravityDom / 100)

		if magnet.Probability < 70 {
			continue
		}

		var bestAlert *models.Alert
		for i := range alerts {
			if alerts[i].Severity == "high" {
				bestAlert = &alerts[i]
				break
			}
		}

		mu.Lock()
		candidates = append(candidates, TargetCandidate{
			Symbol:  symbol,
			Score:   score,
			Snap:    snap,
			Signals: sigs,
			Alert:   bestAlert,
		})
		mu.Unlock()
	}

	if len(candidates) == 0 {
		return
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Score > candidates[j].Score
	})

	top := candidates[0]
	magnet := top.Signals.LiquidationMagnet

	direction := "upward sweep"
	clusterType := "Short liquidations"
	if magnet.Side == "long" {
		direction = "downward sweep"
		clusterType = "Long liquidations"
	}

	message := fmt.Sprintf(
		`🔥 <b>TOP LIQUIDITY TARGET</b>

<b>%s</b> — %s

Price: %s
Target: %s
Distance: %.2f%%

%s building
Sweep Probability: <b>%d%%</b>
Cascade Risk: %s (%d/100)
Gravity: %.1f%% %s

Expected move: <b>%s</b>

📊 Full dashboard → derivlens.io
🔔 Get alerts → t.me/derivlens_signals`,
		top.Symbol,
		clusterType,
		formatPriceStr(top.Snap.LiquidationMap.CurrentPrice),
		formatPriceStr(magnet.Price),
		magnet.Distance,
		clusterType,
		magnet.Probability,
		top.Signals.CascadeRisk.Level,
		top.Signals.CascadeRisk.Score,
		math.Max(top.Signals.LiquidityGravity.UpwardPull, top.Signals.LiquidityGravity.DownwardPull),
		top.Signals.LiquidityGravity.Dominant,
		direction,
	)

	if magnet.Probability >= 80 && top.Alert != nil {
		w.notifier.PostTopAlert(*top.Alert, top.Snap, top.Signals)
	} else {
		w.notifier.PostToChannel(message)
	}

	log.Printf("[worker] top target broadcast: %s score=%.0f prob=%d%%",
		top.Symbol, top.Score, magnet.Probability)
}

func formatPriceStr(p float64) string {
	if p >= 1000 {
		return fmt.Sprintf("$%.2f", p)
	} else if p >= 1 {
		return fmt.Sprintf("$%.3f", p)
	}
	return fmt.Sprintf("$%.4f", p)
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

// ruleTypeFromAlertID extracts the rule type from the full alert ID for dedup.
// Format: "SYMBOL-rule-rest" e.g. "BTC-funding-elevated", "BTC-liq-cluster-69800".
// Strips trailing numeric suffix so "liq-cluster-69800" and "liq-cluster-69700" map to "liq-cluster".
func ruleTypeFromAlertID(alertID string) string {
	parts := strings.SplitN(alertID, "-", 2)
	if len(parts) < 2 {
		return alertID
	}
	rest := parts[1]
	// Strip trailing -NUMBER (e.g. liq-cluster-69800 -> liq-cluster, whale-long-150 -> whale-long)
	if idx := strings.LastIndex(rest, "-"); idx >= 0 {
		suffix := rest[idx+1:]
		if numericSuffix.MatchString(suffix) {
			rest = rest[:idx]
		}
	}
	return rest
}

// alertRuleKey maps an alert message to its rule key using keyword matching.
func alertRuleKey(message string) string {
	msg := strings.ToLower(message)
	switch {
	case strings.Contains(msg, "funding rate"):
		return "funding_spike"
	case strings.Contains(msg, "oi") || strings.Contains(msg, "open interest"):
		return "oi_divergence"
	case strings.Contains(msg, "long bias") || strings.Contains(msg, "traders are long"):
		return "long_bias"
	case strings.Contains(msg, "short bias") || strings.Contains(msg, "traders are short"):
		return "short_bias"
	case strings.Contains(msg, "liquidation"):
		return "liquidation_cluster"
	case strings.Contains(msg, "negative"):
		return "negative_funding"
	default:
		return ""
	}
}
