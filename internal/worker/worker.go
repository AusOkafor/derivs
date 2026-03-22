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
	"derivs-backend/internal/snooze"
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

type symbolAlerts struct {
	detected []models.Alert
	snap     models.MarketSnapshot
	sigs     models.MarketSignals
}

// Worker runs a background ticker that fetches market data, detects anomalies,
// and dispatches Telegram notifications to active subscribers.
type Worker struct {
	aggregator       *aggregator.Aggregator
	detector         *alerts.Detector
	alertEngine      *alerts.Engine
	notifier         *notify.TelegramNotifier
	db               *supabase.Client
	calc             *feargreed.Calculator
	running          atomic.Bool
	freeRunning      atomic.Bool
	proRunning       atomic.Bool
	lastSnapshots    map[string]symbolAlerts
	lastSnapMu       sync.Mutex
	lastAlertTime    time.Time
	lastAlertMu      sync.Mutex
	playbookCooldown *playbookCooldowns
}

func New(
	agg *aggregator.Aggregator,
	det *alerts.Detector,
	not *notify.TelegramNotifier,
	db *supabase.Client,
	calc *feargreed.Calculator,
) *Worker {
	return &Worker{
		aggregator:       agg,
		detector:         det,
		alertEngine:      alerts.NewEngine(),
		notifier:         not,
		db:               db,
		calc:             calc,
		playbookCooldown: newPlaybookCooldowns(),
	}
}

// ProcessAlerts runs alerts through the engine (validate, cooldown, downgrade).
// Used by OnHighAlert callback and broadcastTopTarget to ensure engine is applied.
func (w *Worker) ProcessAlerts(alerts []models.Alert) []models.Alert {
	return w.alertEngine.Process(alerts)
}

// GetLastAlertTime returns the time the most recent alert was sent to a subscriber.
func (w *Worker) GetLastAlertTime() time.Time {
	w.lastAlertMu.Lock()
	defer w.lastAlertMu.Unlock()
	return w.lastAlertTime
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
// - Morning brief: 13:00 UTC daily (8am Jamaica time)
// Stops cleanly when ctx is cancelled.
func (w *Worker) Start(ctx context.Context) {
	w.running.Store(true)
	defer w.running.Store(false)
	log.Println("worker: starting free cycle (5min)")
	log.Println("worker: starting pro cycle (1min)")

	scheduleDaily(time.Date(0, 1, 1, 13, 0, 0, 0, time.UTC), func() {
		go w.SendMorningBrief(context.Background())
	})
	w.schedulePoster()

	freeTicker := time.NewTicker(5 * time.Minute)
	proTicker := time.NewTicker(1 * time.Minute)
	topTargetTicker := time.NewTicker(30 * time.Minute)
	outcomeTicker := time.NewTicker(5 * time.Minute)
	defer topTargetTicker.Stop()
	defer outcomeTicker.Stop()

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
		case <-outcomeTicker.C:
			go w.backfillOutcomes(ctx)
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
// Free: no alerts. Basic: up to 5 symbols. Pro: all 16 symbols.
func allowedSymbols(sub supabase.Subscriber) []string {
	switch sub.Tier {
	case "pro":
		return sub.Symbols
	case "basic":
		if len(sub.Symbols) > 5 {
			return sub.Symbols[:5]
		}
		return sub.Symbols
	default: // free or empty — no alerts
		return nil
	}
}

// runCycleFree runs for Basic tier only (5-min interval). Free tier gets NO alerts.
func (w *Worker) runCycleFree(ctx context.Context) {
	if !w.freeRunning.CompareAndSwap(false, true) {
		log.Println("[worker] basic cycle already running, skipping")
		return
	}
	defer w.freeRunning.Store(false)

	log.Printf("[worker] basic cycle starting (Basic tier only — Free gets no alerts)")
	runCtx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	n := w.runCycle(runCtx, false) // basicCycle = Basic tier only
	log.Printf("[worker] basic cycle done, processed %d subscribers", n)
}

// runCyclePro runs for pro-tier subscribers only, 1-min interval, all symbols.
func (w *Worker) runCyclePro(ctx context.Context) {
	log.Println("worker: pro cycle starting")
	n := w.runCycle(ctx, true) // proCycle
	log.Printf("worker: pro cycle found %d pro subscribers", n)

	// Check for rejection candles forming at cluster levels (playbook triggers).
	w.lastSnapMu.Lock()
	snapshots := make(map[string]symbolAlerts, len(w.lastSnapshots))
	for k, v := range w.lastSnapshots {
		snapshots[k] = v
	}
	w.lastSnapMu.Unlock()
	if len(snapshots) > 0 {
		go w.checkPlaybookTriggers(ctx, snapshots)
	}
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

	// Filter by tier: proOnly = Pro only; !proOnly = Basic only (Free gets no alerts)
	var filtered []supabase.Subscriber
	for _, sub := range subscribers {
		tier := sub.Tier
		if tier == "" {
			tier = "free"
		}
		if tier == "free" {
			continue // free users get no alerts
		}
		if proOnly && !isProTier(tier) {
			continue
		}
		if !proOnly && isProTier(tier) {
			continue // basic cycle: Pro excluded (handled by pro cycle)
		}
		filtered = append(filtered, sub)
	}
	subscribers = filtered
	if len(subscribers) == 0 {
		return 0
	}

	// Collect symbols using allowedSymbols per subscriber, plus DefaultSymbols for heat feed.
	symbolSet := make(map[string]struct{})
	for _, sym := range config.DefaultSymbols {
		symbolSet[sym] = struct{}{}
	}
	for _, sub := range subscribers {
		for _, sym := range allowedSymbols(sub) {
			symbolSet[sym] = struct{}{}
		}
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
		rawAlerts := w.detector.Analyze(snap, sigs)
		processedAlerts := w.alertEngine.Process(rawAlerts)
		snapshots[sym] = symbolAlerts{detected: processedAlerts, snap: snap, sigs: sigs}
		// Fire OnHighAlert for HIGH, MEDIUM, and LOW alerts that passed the engine (only after engine approval).
		// LOW/MEDIUM regime alerts post to public channel; cluster alerts require $500K+ (enforced in main.go).
		// Only from pro cycle — free cycle must not post to public channel (avoids DOGE etc. from basic tier).
		if proOnly {
			for _, alert := range processedAlerts {
				if (alert.Severity == "high" || alert.Severity == "medium" || alert.Severity == "low") && alerts.OnHighAlert != nil {
					alerts.OnHighAlert(alert, snap, sigs)
				}
			}
		}
		if proOnly {
			log.Printf("worker: pro cycle found %d alerts for %s (raw: %d)", len(processedAlerts), sym, len(rawAlerts))
		} else {
			log.Printf("[worker] free cycle found %d alerts for %s (raw: %d)", len(processedAlerts), sym, len(rawAlerts))
		}
		// Log alerts to alert_history for outcome tracking.
		// Long/short ratio bias alerts are excluded — they observe a condition but have
		// no price target or timing signal, so their outcomes are noise.
		// All other alert types (cluster sweeps, OI events, funding, liquidation events)
		// have actionable signals and are worth tracking.
		currentPrice := snap.LiquidationMap.CurrentPrice
		for _, alert := range processedAlerts {
			if alert.RuleKey == "long_bias" || alert.RuleKey == "short_bias" {
				continue
			}
			if err := w.db.LogAlertHistory(ctx, sym, alert.ID, alert.Message, alert.Severity, currentPrice); err != nil {
				log.Printf("worker: LogAlertHistory(%s): %v", sym, err)
			}
		}
	}

	// Check custom price alerts against current prices (pro cycle only)
	if proOnly {
		go w.checkCustomAlerts(ctx, snapshots)
	}

	// Store for heat feed (broadcastTopTarget uses engine-processed data only)
	w.lastSnapMu.Lock()
	w.lastSnapshots = make(map[string]symbolAlerts, len(snapshots))
	for k, v := range snapshots {
		w.lastSnapshots[k] = v
	}
	w.lastSnapMu.Unlock()

	for _, sub := range subscribers {
		if sub.ChatID == 0 {
			if !proOnly {
				log.Printf("[worker] free cycle skipping @%s: ChatID=0 (user must send /start to bot)", sub.TelegramUsername)
			}
			continue
		}

		// Collect all (symbol, alert, snap, sigs) that pass rules filter
		type symAlert struct {
			sym   string
			alert models.Alert
			snap  models.MarketSnapshot
			sigs  models.MarketSignals
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
				candidates = append(candidates, symAlert{sym: sym, alert: alert, snap: sa.snap, sigs: sa.sigs})
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

			if !alerts.IsSafeToSend(ca.alert) {
				log.Printf("[guard] BLOCKED before send: %s cluster=%.0f dist=%.4f", ca.alert.Symbol, ca.alert.ClusterSize, ca.alert.Distance)
				continue
			}

			if snooze.Global.IsSnoozed(sub.ID, ca.sym) {
				log.Printf("[snooze] skipping %s %s for @%s — snoozed", ca.sym, ca.alert.ID, sub.TelegramUsername)
				continue
			}

			// Use formatted alert for all alerts (Pro and Free)
			if err := w.notifier.SendAlertCardToUser(ctx, sub.ChatID, ca.alert, ca.snap, ca.sigs); err != nil {
				log.Printf("worker: SendAlertCardToUser(chat=%d): %v", sub.ChatID, err)
				// Fallback to formatted text
				msg := formatAlertMessage(ca.sym, ca.alert, ca.snap, ca.sigs)
				if err := w.notifier.SendMessage(ctx, sub.ChatID, msg); err != nil {
					log.Printf("worker: SendMessage(chat=%d): %v", sub.ChatID, err)
					continue
				}
			}
			w.lastAlertMu.Lock()
			w.lastAlertTime = time.Now().UTC()
			w.lastAlertMu.Unlock()

			// Discord webhook — fire alongside Telegram if configured
			if sub.DiscordWebhookURL != "" {
				currentPrice := ca.snap.LiquidationMap.CurrentPrice
				go func(hookURL string, alert models.Alert, price float64) {
					if err := notify.SendDiscordAlert(context.Background(), hookURL, alert, price); err != nil {
						log.Printf("[discord] SendDiscordAlert(@%s): %v", sub.TelegramUsername, err)
					}
				}(sub.DiscordWebhookURL, ca.alert, currentPrice)
			}

			if proOnly {
				log.Printf("worker: pro cycle sending alert to %s: %s", sub.TelegramUsername, ca.alert.Message)
			} else {
				log.Printf("[worker] free cycle sent alert to @%s: %s — %s", sub.TelegramUsername, ca.alert.ID, ca.sym)
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

	w.lastSnapMu.Lock()
	processed := w.lastSnapshots
	w.lastSnapMu.Unlock()

	if len(processed) == 0 {
		log.Printf("[worker] heat feed: no processed snapshots yet, skipping")
		return
	}

	var candidates []TargetCandidate
	for sym, sa := range processed {
		var bestScore float64
		var bestTC *TargetCandidate
		for _, alert := range sa.detected {
			if !alerts.IsSafeToSend(alert) {
				continue
			}
			effectiveDistance := alert.Distance
			if effectiveDistance < 0.001 {
				effectiveDistance = 0.001 // clamp — prevents score explosion
			}
			score := alert.ClusterSize / effectiveDistance
			if score > bestScore {
				bestScore = score
				alertCopy := alert
				bestTC = &TargetCandidate{
					Symbol:  sym,
					Score:  score,
					Snap:   sa.snap,
					Signals: sa.sigs,
					Alert:  &alertCopy,
				}
			}
		}
		if bestTC != nil {
			candidates = append(candidates, *bestTC)
		}
	}

	if len(candidates) == 0 {
		return
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Score > candidates[j].Score
	})

	// Show top 3 as ranked heat feed
	topN := 3
	if len(candidates) < topN {
		topN = len(candidates)
	}

	var sb strings.Builder
	sb.WriteString("🔥 <b>LIQUIDITY HEAT FEED</b>\n\n")

	for i := 0; i < topN; i++ {
		tc := candidates[i]
		magnet := tc.Signals.LiquidationMagnet
		snap := tc.Snap
		signals := tc.Signals
		side := "Short"
		if magnet != nil && magnet.Side == "long" {
			side = "Long"
		}
		clusterStr := formatUSDWorker(0)
		priceStr := formatPriceWorker(0)
		targetStr := formatPriceWorker(0)
		distPct := 0.0
		prob := 0
		cascade := "—"
		dirLabel := directionLabel("")
		if tc.Alert != nil {
			clusterStr = formatUSDWorker(tc.Alert.ClusterSize)
			targetStr = formatPriceWorker(tc.Alert.ClusterPrice)
			distPct = tc.Alert.Distance * 100
		}
		if snap.LiquidationMap.CurrentPrice > 0 {
			priceStr = formatPriceWorker(snap.LiquidationMap.CurrentPrice)
		}
		if magnet != nil {
			prob = magnet.Probability
		}
		if signals.CascadeRisk.Level != "" {
			cascade = signals.CascadeRisk.Level
		}
		if signals.LiquidityGravity.Dominant != "" {
			dirLabel = directionLabel(signals.LiquidityGravity.Dominant)
		}
		entry := fmt.Sprintf(
			"%s <b>%s</b> — %s cluster %s\n   Price: %s | Target: %s | Distance: %.2f%% | Prob: %d%% | CASCADE: %s | %s\n\n",
			rankEmoji(i+1),
			tc.Symbol,
			side,
			clusterStr,
			priceStr,
			targetStr,
			distPct,
			prob,
			cascade,
			dirLabel,
		)
		sb.WriteString(entry)
	}

	sb.WriteString("📊 Full dashboard → derivlens.io\n")
	sb.WriteString("🔔 Get alerts → t.me/derivlens_signals")

	message := sb.String()

	top := candidates[0]
	heatFeedKey := fmt.Sprintf("heatfeed:%s", top.Symbol)
	if !w.alertEngine.Cooldown().Allow(heatFeedKey) {
		log.Printf("[worker] heat feed: skipping %s (cooldown)", top.Symbol)
		return
	}

	magnet := top.Signals.LiquidationMagnet
	if magnet.Probability >= 80 && top.Alert != nil && alerts.IsSafeToSend(*top.Alert) {
		processed := w.ProcessAlerts([]models.Alert{*top.Alert})
		if len(processed) > 0 {
			w.notifier.PostTopAlert(*top.Alert, top.Snap, top.Signals)
		} else {
			w.notifier.PostToChannel(message)
		}
	} else {
		if top.Alert != nil && !alerts.IsSafeToSend(*top.Alert) {
			log.Printf("[guard] BLOCKED heat feed PostTopAlert: %s cluster=%.0f dist=%.4f", top.Alert.Symbol, top.Alert.ClusterSize, top.Alert.Distance)
		}
		w.notifier.PostToChannel(message)
	}

	// Send heat feed to Pro and Basic subscribers
	subscribers, err := w.db.GetActiveSubscribers(ctx)
	if err == nil {
		for _, sub := range subscribers {
			if sub.ChatID == 0 {
				continue
			}
			tier := sub.Tier
			if tier == "" {
				tier = "free"
			}
			if tier != "pro" && tier != "basic" {
				continue
			}
			if err := w.notifier.SendMessage(ctx, sub.ChatID, message); err != nil {
				log.Printf("[worker] heat feed send to @%s: %v", sub.TelegramUsername, err)
			}
		}
	}

	log.Printf("[worker] top target broadcast: top %d — %s score=%.0f prob=%d%%",
		topN, top.Symbol, top.Score, magnet.Probability)
}

// backfillOutcomes fetches price outcomes for alerts fired 15m and 1h ago.
// It groups pending alerts by symbol, fetches the current price once per symbol,
// then patches price_15m/price_1h and outcome_pct columns in alert_history.
func (w *Worker) backfillOutcomes(ctx context.Context) {
	type pending struct {
		id           string
		priceAtAlert float64
		priceCol     string
		pctCol       string
	}

	collect := func(outcomeCol, priceCol, pctCol string, minAge, maxAge time.Duration) map[string][]pending {
		entries, err := w.db.GetAlertsPendingOutcome(ctx, outcomeCol, minAge, maxAge)
		if err != nil {
			log.Printf("[outcomes] GetAlertsPendingOutcome(%s): %v", outcomeCol, err)
			return nil
		}
		bySymbol := make(map[string][]pending)
		for _, e := range entries {
			if e.PriceAtAlert == nil || *e.PriceAtAlert == 0 {
				continue
			}
			bySymbol[e.Symbol] = append(bySymbol[e.Symbol], pending{
				id:           e.ID,
				priceAtAlert: *e.PriceAtAlert,
				priceCol:     priceCol,
				pctCol:       pctCol,
			})
		}
		return bySymbol
	}

	pending15m := collect("price_15m", "price_15m", "outcome_pct_15m", 15*time.Minute, 4*time.Hour)
	pending1h := collect("price_1h", "price_1h", "outcome_pct_1h", 1*time.Hour, 8*time.Hour)

	// Merge symbol sets
	symbolSet := make(map[string]struct{})
	for sym := range pending15m {
		symbolSet[sym] = struct{}{}
	}
	for sym := range pending1h {
		symbolSet[sym] = struct{}{}
	}
	if len(symbolSet) == 0 {
		return
	}

	// Fetch current price per symbol (one call each)
	prices := make(map[string]float64)
	for sym := range symbolSet {
		price, _, err := w.aggregator.FetchTicker(ctx, sym)
		if err != nil {
			log.Printf("[outcomes] FetchTicker(%s): %v", sym, err)
			continue
		}
		prices[sym] = price
	}

	update := func(bySymbol map[string][]pending) {
		for sym, items := range bySymbol {
			currentPrice, ok := prices[sym]
			if !ok || currentPrice == 0 {
				continue
			}
			for _, p := range items {
				pct := (currentPrice - p.priceAtAlert) / p.priceAtAlert * 100
				if err := w.db.UpdateAlertOutcome(ctx, p.id, p.priceCol, p.pctCol, currentPrice, pct); err != nil {
					log.Printf("[outcomes] UpdateAlertOutcome(%s): %v", p.id, err)
				}
			}
		}
	}

	update(pending15m)
	update(pending1h)
}

// checkCustomAlerts checks all pending custom price alerts against current snapshot prices.
// Called after each pro cycle once the snapshots map is built.
func (w *Worker) checkCustomAlerts(ctx context.Context, snapshots map[string]symbolAlerts) {
	pending, err := w.db.GetPendingCustomPriceAlerts(ctx)
	if err != nil {
		log.Printf("[custom alerts] GetPendingCustomPriceAlerts: %v", err)
		return
	}
	if len(pending) == 0 {
		return
	}

	// Look up chat_id and discord webhook for each subscriber — cached to avoid repeat DB calls
	type subInfo struct {
		chatID     int64
		discordURL string
	}
	subCache := make(map[string]subInfo)
	getSubInfo := func(subscriberID string) subInfo {
		if info, ok := subCache[subscriberID]; ok {
			return info
		}
		subs, err := w.db.GetActiveSubscribers(ctx)
		if err != nil {
			return subInfo{}
		}
		for _, s := range subs {
			subCache[s.ID] = subInfo{chatID: s.ChatID, discordURL: s.DiscordWebhookURL}
		}
		return subCache[subscriberID]
	}

	for _, ca := range pending {
		sa, ok := snapshots[ca.Symbol]
		if !ok {
			continue
		}
		currentPrice := sa.snap.LiquidationMap.CurrentPrice
		if currentPrice == 0 {
			continue
		}

		triggered := false
		switch ca.Direction {
		case "above":
			triggered = currentPrice >= ca.TargetPrice
		case "below":
			triggered = currentPrice <= ca.TargetPrice
		}
		if !triggered {
			continue
		}

		// Mark triggered first (avoid double-send on crash)
		if err := w.db.MarkCustomPriceAlertTriggered(ctx, ca.ID); err != nil {
			log.Printf("[custom alerts] MarkTriggered(%s): %v", ca.ID, err)
			continue
		}

		info := getSubInfo(ca.SubscriberID)
		if info.chatID == 0 {
			log.Printf("[custom alerts] no chatID for subscriber %s", ca.SubscriberID)
			continue
		}

		dirWord := "reached above"
		if ca.Direction == "below" {
			dirWord = "dropped below"
		}
		noteStr := ""
		if ca.Note != "" {
			noteStr = fmt.Sprintf("\n📝 %s", ca.Note)
		}
		msg := fmt.Sprintf(
			"🎯 <b>PRICE ALERT — %s</b>\n\nPrice has %s your target of <b>$%s</b>\nCurrent price: <b>$%s</b>%s\n\n📊 Dashboard → derivlens.io",
			ca.Symbol,
			dirWord,
			formatPriceWorker(ca.TargetPrice),
			formatPriceWorker(currentPrice),
			noteStr,
		)
		if err := w.notifier.SendMessage(ctx, info.chatID, msg); err != nil {
			log.Printf("[custom alerts] SendMessage(chat=%d): %v", info.chatID, err)
		} else {
			log.Printf("[custom alerts] triggered %s %s $%.2f for subscriber %s", ca.Symbol, ca.Direction, ca.TargetPrice, ca.SubscriberID)
		}

		// Fire Discord if subscriber has a webhook configured
		if info.discordURL != "" {
			discordMsg := fmt.Sprintf("🎯 **PRICE ALERT — %s**\n\nPrice has %s your target of **$%s**\nCurrent price: **$%s**%s\n\n📊 Dashboard → derivlens.io",
				ca.Symbol, dirWord, formatPriceWorker(ca.TargetPrice), formatPriceWorker(currentPrice), noteStr)
			go func(hookURL, content string) {
				if err := notify.SendDiscordMessage(context.Background(), hookURL, content); err != nil {
					log.Printf("[custom alerts] discord send: %v", err)
				}
			}(info.discordURL, discordMsg)
		}
	}
}

// formatAlertMessage produces the same clean format as the public channel text alerts.
func formatAlertMessage(symbol string, alert models.Alert, snap models.MarketSnapshot, sigs models.MarketSignals) string {
	magnet := sigs.LiquidationMagnet
	if magnet == nil {
		return alert.Message
	}
	return fmt.Sprintf(`%s — %s

%s

Sweep Probability: %d%%
Cascade Risk: %s (%d/100)
Liquidity Pressure: %+d (%s)
Gravity: %.1f%% %s

📊 Full dashboard → derivlens.io`,
		severityEmojiWorker(alert.Severity),
		symbol,
		alert.Message,
		magnet.Probability,
		sigs.CascadeRisk.Level,
		sigs.CascadeRisk.Score,
		sigs.LiquidityPressure.Score,
		sigs.LiquidityPressure.Label,
		math.Max(sigs.LiquidityGravity.UpwardPull, sigs.LiquidityGravity.DownwardPull),
		sigs.LiquidityGravity.Dominant,
	)
}

func severityEmojiWorker(severity string) string {
	switch strings.ToLower(severity) {
	case "high":
		return "🔴 HIGH ALERT"
	case "medium":
		return "🟡 MEDIUM ALERT"
	default:
		return "🔵 ALERT"
	}
}

func formatPriceStr(p float64) string {
	if p >= 1000 {
		return fmt.Sprintf("$%.2f", p)
	} else if p >= 1 {
		return fmt.Sprintf("$%.3f", p)
	}
	return fmt.Sprintf("$%.4f", p)
}

func formatPriceWorker(p float64) string {
	switch {
	case p >= 1000:
		return fmt.Sprintf("$%.0f", p)
	case p >= 10:
		return fmt.Sprintf("$%.2f", p)
	case p >= 1:
		return fmt.Sprintf("$%.3f", p)
	case p >= 0.1:
		return fmt.Sprintf("$%.4f", p)
	case p >= 0.01:
		return fmt.Sprintf("$%.5f", p)
	default:
		return fmt.Sprintf("$%.6f", p)
	}
}

func rankEmoji(n int) string {
	if n >= 1 && n <= 3 {
		return []string{"1️⃣", "2️⃣", "3️⃣"}[n-1]
	}
	return fmt.Sprintf("%d.", n)
}

func directionLabel(dominant string) string {
	switch dominant {
	case "upward":
		return "↑"
	case "downward":
		return "↓"
	default:
		return "—"
	}
}

func formatUSDWorker(usd float64) string {
	if usd >= 1_000_000 {
		return fmt.Sprintf("$%.2fM", usd/1_000_000)
	}
	if usd >= 1_000 {
		return fmt.Sprintf("$%.2fk", usd/1_000)
	}
	return fmt.Sprintf("$%.0f", usd)
}

// defaultThresholds are the baseline values applied when a subscriber has no saved rules
// or has not configured a specific threshold. These match the frontend UI defaults.
var defaultThresholds = map[string]float64{
	"long_bias_threshold":     65.0,
	"short_bias_threshold":    65.0,
	"oi_divergence_threshold": 10.0,
	"funding_spike_threshold": 0.05,
}

// shouldSendAlert checks whether a subscriber's rules JSONB permits a given alert.
// Rules supports both boolean on/off flags and numeric thresholds:
//
//	{"funding_spike": true, "long_bias": true, "long_bias_threshold": 73.0}
//
// Threshold resolution order:
//  1. User's saved value for that key
//  2. defaultThresholds fallback (matches frontend UI defaults)
//
// An alert passes if its rule key is absent or true, and its Value meets the threshold.
func (w *Worker) shouldSendAlert(rules json.RawMessage, alert models.Alert) bool {
	var ruleMap map[string]interface{}
	if len(rules) > 0 {
		if err := json.Unmarshal(rules, &ruleMap); err != nil {
			return true // fail open on corrupt rules
		}
	}

	ruleKey := alertRuleKey(alert.Message)
	if ruleKey == "" {
		return true
	}

	// Check boolean on/off flag
	if ruleMap != nil {
		if raw, ok := ruleMap[ruleKey]; ok {
			if b, ok := raw.(bool); ok && !b {
				return false // rule explicitly disabled
			}
		}
	}

	// Check numeric threshold — alert.Value must be >= threshold to send.
	// User's saved value takes precedence; fall back to defaultThresholds.
	if alert.Value > 0 {
		threshKey := ruleKey + "_threshold"
		threshold := defaultThresholds[threshKey] // start with default
		if ruleMap != nil {
			if raw, ok := ruleMap[threshKey]; ok {
				if saved, ok := raw.(float64); ok {
					threshold = saved // override with user's value
				}
			}
		}
		if threshold > 0 && alert.Value < threshold {
			return false
		}
	}

	return true
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
	case strings.Contains(msg, "oi up") || strings.Contains(msg, "oi down") || strings.Contains(msg, "open interest"):
		return "oi_divergence"
	case strings.Contains(msg, "traders are long"):
		return "long_bias"
	case strings.Contains(msg, "traders are short"):
		return "short_bias"
	case strings.Contains(msg, "liquidation"):
		return "liquidation_cluster"
	default:
		return ""
	}
}
