package worker

import (
	"context"
	"fmt"
	"log"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"derivs-backend/internal/models"
)

const (
	playbookProximityPct  = 0.3 // trigger check when within 0.3% of cluster
	playbookCooldown      = 30 * time.Minute
	playbookDisplacementR = 0.3 // confirmed: close must reclaim >30% of wick
	playbookMinMomentum   = 0.4 // candle range must be ≥40% of prior avg
	playbookScoreOverride = 15  // allow re-alert if new score exceeds previous by this much
	playbookKlineInterval = "5m"
	playbookKlineLimit    = 6 // 5 closed + 1 open (current)
	playbookMinAlertScore = 60 // confirmed alerts below this score are suppressed
	playbookMaxPerCycle   = 3  // max alerts sent in one worker cycle across all symbols

	// Minimum cluster size per symbol — mirrors frontend defaults.
	// Clusters below these thresholds are too small to be meaningful.
	playbookMinClusterBTC     = 500_000 // $500K
	playbookMinClusterETH     = 200_000 // $200K
	playbookMinClusterDefault = 100_000 // $100K for all other symbols

	// Follow-through: adaptive threshold = avgRangePct * this multiplier.
	// E.g. if avg 5m range = 0.3% of price, threshold = 0.3 * 0.25 = 0.075%.
	playbookFTMultiplier = 0.25
	playbookFTMinPct     = 0.05 // floor: never below 0.05%
	playbookFTMaxPct     = 0.30 // ceiling: never above 0.30%
)

// ── Live state store ─────────────────────────────────────────────────────────

// PlaybookState holds the most recent trigger state for a symbol.
// Exposed via GET /api/playbook/status so the frontend can show live context.
type PlaybookState struct {
	Symbol       string    `json:"symbol"`
	Stage        string    `json:"stage"`        // "forming", "confirmed", "idle"
	Score        int       `json:"score"`
	FiredAt      time.Time `json:"fired_at"`
	ClusterPrice float64   `json:"cluster_price"`
	Side         string    `json:"side"`
	Probability  int       `json:"probability"`
	Aligned      bool      `json:"aligned"`
	SweepDir     string    `json:"sweep_dir"`
	Regime       string    `json:"regime"`
}

type playbookStateStore struct {
	mu     sync.Mutex
	states map[string]PlaybookState
}

func newPlaybookStateStore() *playbookStateStore {
	return &playbookStateStore{states: make(map[string]PlaybookState)}
}

func (s *playbookStateStore) set(symbol string, state PlaybookState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.states[symbol] = state
}

// Get returns the current state for a symbol, falling back to "idle" if
// the state has expired (older than the cooldown window).
func (s *playbookStateStore) get(symbol string) PlaybookState {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.states[symbol]
	if !ok || time.Since(st.FiredAt) > playbookCooldown {
		return PlaybookState{Symbol: symbol, Stage: "idle"}
	}
	return st
}

// ── Cooldown with score memory ────────────────────────────────────────────────

type cooldownEntry struct {
	firedAt time.Time
	score   int
}

type playbookCooldowns struct {
	mu      sync.Mutex
	entries map[string]cooldownEntry
}

func newPlaybookCooldowns() *playbookCooldowns {
	return &playbookCooldowns{entries: make(map[string]cooldownEntry)}
}

// allow returns true if the signal may fire. Within the cooldown window, a new
// signal is only allowed if it scores at least playbookScoreOverride points higher.
//
// Key is symbol:stage only — NOT symbol:level:stage — because the cluster price
// shifts slightly each cycle as the liquidation map is recalculated, which would
// otherwise bypass the cooldown and spam on every cycle.
func (c *playbookCooldowns) allow(symbol string, level float64, stage string, newScore int) bool {
	key := fmt.Sprintf("%s:%s", symbol, stage)
	c.mu.Lock()
	defer c.mu.Unlock()

	if e, ok := c.entries[key]; ok && time.Since(e.firedAt) < playbookCooldown {
		if newScore < e.score+playbookScoreOverride {
			return false
		}
		log.Printf("[playbook] score override: %s %s prev=%d new=%d", symbol, stage, e.score, newScore)
	}
	c.entries[key] = cooldownEntry{firedAt: time.Now(), score: newScore}
	return true
}

// blockForming sets the forming cooldown for a symbol so it cannot fire
// after a confirmed signal has already been dispatched for that symbol.
func (c *playbookCooldowns) blockForming(symbol string, level float64) {
	key := fmt.Sprintf("%s:forming", symbol)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = cooldownEntry{firedAt: time.Now(), score: 999}
}

// ── Follow-through tracker ────────────────────────────────────────────────────

// followThroughEntry tracks a confirmed signal through three time checkpoints.
//
// Checkpoints: 10 min (fast move?), 20 min (initial hold?), 40 min (held or reversed?)
//
// On every cycle, MFE and MAE are updated:
//
//	MFE (max favorable excursion) — how far price went in the right direction
//	MAE (max adverse excursion)   — how much heat the position took
//
// Outcome classification (logged at 40 min):
//
//	clean win  — reached threshold AND held at 40m
//	weak win   — reached threshold BUT reversed by 40m
//	failure    — never reached threshold
type followThroughEntry struct {
	symbol    string
	side      string  // "long" or "short" cluster
	firePrice float64 // price when confirmed signal fired
	fireTime  time.Time
	threshold float64 // adaptive % threshold (avgRangePct * 0.25)
	score     int     // signal score — for future calibration bucketing
	session   string  // time-of-day session at fire time

	// Running extremes — updated every cycle
	mfe float64 // max favorable excursion %
	mae float64 // max adverse excursion %

	// Checkpoint flags
	check10Done bool
	check20Done bool
	check40Done bool

	// First-cross tracking
	movedAt    *time.Time
	initialPct float64 // % move at 10m checkpoint
}

type followThroughTracker struct {
	mu      sync.Mutex
	pending []*followThroughEntry
}

func newFollowThroughTracker() *followThroughTracker {
	return &followThroughTracker{}
}

// record starts tracking a new confirmed signal.
func (ft *followThroughTracker) record(symbol, side string, firePrice, avgRangePct float64, score int) {
	threshold := math.Max(playbookFTMinPct, math.Min(playbookFTMaxPct, avgRangePct*playbookFTMultiplier))
	ft.mu.Lock()
	defer ft.mu.Unlock()
	ft.pending = append(ft.pending, &followThroughEntry{
		symbol:    symbol,
		side:      side,
		firePrice: firePrice,
		fireTime:  time.Now(),
		threshold: threshold,
		score:     score,
		session:   marketSession(time.Now().UTC()),
	})
}

// evaluate runs each cycle — updates MFE/MAE and fires checkpoint logs.
func (ft *followThroughTracker) evaluate(snapshots map[string]symbolAlerts) {
	ft.mu.Lock()
	defer ft.mu.Unlock()

	now := time.Now()
	var remaining []*followThroughEntry

	for _, e := range ft.pending {
		age := now.Sub(e.fireTime)
		if age > 50*time.Minute {
			continue // expired — drop
		}

		sa, ok := snapshots[e.symbol]
		if !ok {
			remaining = append(remaining, e)
			continue
		}
		currentPrice := sa.snap.LiquidationMap.CurrentPrice
		if currentPrice == 0 {
			remaining = append(remaining, e)
			continue
		}

		pctMove := (currentPrice - e.firePrice) / e.firePrice * 100

		// Update MFE / MAE every cycle
		if e.side == "long" {
			if pctMove > e.mfe {
				e.mfe = pctMove
			}
			if -pctMove > e.mae {
				e.mae = -pctMove
			}
		} else {
			if -pctMove > e.mfe {
				e.mfe = -pctMove
			}
			if pctMove > e.mae {
				e.mae = pctMove
			}
		}

		// Track first threshold cross (time-to-move)
		movedFavorably := (e.side == "long" && pctMove >= e.threshold) ||
			(e.side == "short" && pctMove <= -e.threshold)
		if e.movedAt == nil && movedFavorably {
			t := now
			e.movedAt = &t
			timeToMove := now.Sub(e.fireTime)
			log.Printf("[playbook:ft] %s %s — threshold crossed %.3f%% | time-to-move: %s | score-bucket: %s | session: %s",
				e.symbol, e.side, math.Abs(pctMove), timeToMove.Round(time.Second), scoreBucket(e.score), e.session)
		}

		// Checkpoint: 10 min — fast move check
		if !e.check10Done && age >= 10*time.Minute {
			e.check10Done = true
			e.initialPct = pctMove
			log.Printf("[playbook:ft] 10m %s %s — move: %+.3f%% | MFE: %.3f%% MAE: %.3f%%",
				e.symbol, e.side, pctMove, e.mfe, e.mae)
		}

		// Checkpoint: 20 min — initial hold check
		if !e.check20Done && age >= 20*time.Minute {
			e.check20Done = true
			log.Printf("[playbook:ft] 20m %s %s — move: %+.3f%% | MFE: %.3f%% MAE: %.3f%%",
				e.symbol, e.side, pctMove, e.mfe, e.mae)
		}

		// Checkpoint: 40 min — final outcome
		if !e.check40Done && age >= 40*time.Minute {
			e.check40Done = true
			outcome := classifyOutcome(e, pctMove)
			log.Printf("[playbook:ft] OUTCOME %s %s — %s | score: %d (%s) | MFE: %.3f%% MAE: %.3f%% | session: %s | threshold: %.3f%%",
				e.symbol, e.side, outcome, e.score, scoreBucket(e.score), e.mfe, e.mae, e.session, e.threshold)
			continue // done — drop
		}

		remaining = append(remaining, e)
	}

	ft.pending = remaining
}

func classifyOutcome(e *followThroughEntry, finalPct float64) string {
	if e.movedAt == nil {
		return "❌ FAILURE (never reached threshold)"
	}
	holdThreshold := e.threshold * 0.5
	reversed := (e.side == "long" && finalPct < holdThreshold) ||
		(e.side == "short" && finalPct > -holdThreshold)
	if reversed {
		return fmt.Sprintf("⚠️ WEAK WIN (init %+.3f%% → final %+.3f%%)", e.initialPct, finalPct)
	}
	return fmt.Sprintf("✅ CLEAN WIN (init %+.3f%% → final %+.3f%%)", e.initialPct, finalPct)
}

// scoreBucket groups score into 10-point buckets for calibration grouping.
func scoreBucket(score int) string {
	low := (score / 10) * 10
	return fmt.Sprintf("%d-%d", low, low+10)
}

// marketSession returns a label for the UTC hour at signal fire time.
// Used to discover time-of-day patterns in outcome data.
func marketSession(t time.Time) string {
	h := t.Hour()
	switch {
	case h >= 0 && h < 8:
		return "Asia"
	case h >= 8 && h < 12:
		return "London Open"
	case h >= 12 && h < 17:
		return "NY Session"
	case h >= 17 && h < 21:
		return "Late NY"
	default:
		return "Pre-Asia"
	}
}

// ── Main trigger checker ──────────────────────────────────────────────────────

// playbookCandidate holds a ready-to-send alert collected during one cycle.
type playbookCandidate struct {
	symbol      string
	stage       string // "forming" or "confirmed"
	score       int
	msg         string
	state       PlaybookState
	currentPrice float64
	avgRangePct  float64
	side        string
}

func (w *Worker) checkPlaybookTriggers(ctx context.Context, snapshots map[string]symbolAlerts) {
	w.followThrough.evaluate(snapshots)

	// ── Pass 1: collect all candidates ───────────────────────────────────────
	// One entry per symbol — confirmed takes priority over forming.
	candidates := make(map[string]*playbookCandidate) // keyed by symbol

	for symbol, sa := range snapshots {
		m := sa.sigs.LiquidationMagnet
		if m == nil {
			continue
		}
		if m.Distance >= playbookProximityPct {
			continue
		}
		if m.SizeUSD < playbookMinClusterSize(symbol) {
			log.Printf("[playbook] %s cluster $%.0f below minimum $%.0f — skipped", symbol, m.SizeUSD, playbookMinClusterSize(symbol))
			continue
		}

		candles, err := w.aggregator.FetchKlines(ctx, symbol, playbookKlineInterval, playbookKlineLimit)
		if err != nil {
			log.Printf("[playbook] FetchKlines(%s): %v", symbol, err)
			continue
		}
		if len(candles) < 2 {
			continue
		}

		currentCandle := candles[len(candles)-1]
		closedCandles := candles[:len(candles)-1]
		avgRange := averageCandleRange(closedCandles)
		currentPrice := sa.snap.LiquidationMap.CurrentPrice

		avgRangePct := 0.0
		if currentPrice > 0 {
			avgRangePct = avgRange / currentPrice * 100
		}

		aligned := isBiasAligned(m.Side, sa.sigs.Regime)

		// ── Check confirmed first (higher priority) ───────────────────────
		for i := len(closedCandles) - 1; i >= 0; i-- {
			c := closedCandles[i]
			wick, body, depth := rejectionMetrics(c, m.Side, m.Price)
			if wick == 0 {
				continue
			}
			if body < wick*playbookDisplacementR {
				continue
			}
			candleRange := c.High - c.Low
			if avgRange > 0 && candleRange < avgRange*playbookMinMomentum {
				log.Printf("[playbook] %s closed candle too small, skipping", symbol)
				break
			}
			trendPenalty := trendPressure(closedCandles, m.Side)
			score := scoreRejection(wick, body, depth, m.Price, m.Side, sa.sigs.Regime, candleRange, avgRange)
			score = max(0, score+trendPenalty)

			if score < playbookMinAlertScore {
				log.Printf("[playbook] %s confirmed score %d below min %d — suppressed", symbol, score, playbookMinAlertScore)
				break
			}
			if !w.playbookCooldown.allow(symbol, m.Price, "confirmed", score) {
				break
			}

			candidates[symbol] = &playbookCandidate{
				symbol:       symbol,
				stage:        "confirmed",
				score:        score,
				msg:          buildConfirmedAlert(symbol, m, sa.sigs, score, aligned),
				currentPrice: currentPrice,
				avgRangePct:  avgRangePct,
				side:         m.Side,
				state: PlaybookState{
					Symbol: symbol, Stage: "confirmed", Score: score,
					FiredAt: time.Now(), ClusterPrice: m.Price, Side: m.Side,
					Probability: m.Probability, Aligned: aligned,
					SweepDir: sweepDirStr(m.Side), Regime: string(sa.sigs.Regime),
				},
			}
			// Block forming for this level — confirmed supersedes it on all future cycles
			w.playbookCooldown.blockForming(symbol, m.Price)
			log.Printf("[playbook] %s confirmed candidate score=%d trend_penalty=%d avg_range_pct=%.3f%%",
				symbol, score, trendPenalty, avgRangePct)
			break
		}

		// ── Check forming — only if no confirmed candidate for this symbol ─
		if _, hasConfirmed := candidates[symbol]; !hasConfirmed {
			if wickPastLevel(currentCandle, m.Side, m.Price) {
				candleRange := currentCandle.High - currentCandle.Low
				if avgRange == 0 || candleRange >= avgRange*playbookMinMomentum {
					if w.playbookCooldown.allow(symbol, m.Price, "forming", 0) {
						candidates[symbol] = &playbookCandidate{
							symbol: symbol,
							stage:  "forming",
							score:  0,
							msg:    buildFormingAlert(symbol, m, sa.sigs),
							state: PlaybookState{
								Symbol: symbol, Stage: "forming", Score: 0,
								FiredAt: time.Now(), ClusterPrice: m.Price, Side: m.Side,
								Probability: m.Probability, Aligned: aligned,
								SweepDir: sweepDirStr(m.Side), Regime: string(sa.sigs.Regime),
							},
						}
						log.Printf("[playbook] %s forming candidate at %.4f (prob=%d%%)", symbol, m.Price, m.Probability)
					}
				}
			}
		}
	}

	if len(candidates) == 0 {
		return
	}

	// Fetch subscribers once — used in Pass 3 to deliver alerts to users
	subs, err := w.db.GetActiveSubscribers(ctx)
	if err != nil {
		log.Printf("[playbook] GetActiveSubscribers: %v", err)
		subs = nil
	}

	// ── Pass 2: sort — confirmed before forming, then by score desc ──────────
	sorted := make([]*playbookCandidate, 0, len(candidates))
	for _, c := range candidates {
		sorted = append(sorted, c)
	}
	// Simple insertion sort (small slice)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0; j-- {
			a, b := sorted[j-1], sorted[j]
			aRank := candidateRank(a)
			bRank := candidateRank(b)
			if bRank > aRank {
				sorted[j-1], sorted[j] = sorted[j], sorted[j-1]
			} else {
				break
			}
		}
	}

	// Resolve admin chat ID once — used to skip double-delivery to admin who is also a subscriber.
	adminChatID := int64(0)
	if s := os.Getenv("ADMIN_TELEGRAM_CHAT_ID"); s != "" {
		if id, err := strconv.ParseInt(s, 10, 64); err == nil {
			adminChatID = id
		}
	}

	// ── Pass 3: send — candidates are sorted highest-priority first ─────────
	// Each subscriber receives at most one playbook alert per cycle (the
	// highest-ranked candidate whose symbol matches their subscription).
	// This prevents flooding users who have many symbols configured.
	subReceived := make(map[int64]bool) // chatID → already received this cycle

	sent := 0
	for _, c := range sorted {
		if sent >= playbookMaxPerCycle {
			log.Printf("[playbook] cycle cap reached (%d) — suppressing %s %s", playbookMaxPerCycle, c.symbol, c.stage)
			break
		}

		// Admin monitoring
		if err := w.notifier.SendToAdmin(c.msg); err != nil {
			log.Printf("[playbook] SendToAdmin %s %s: %v", c.symbol, c.stage, err)
		}

		// Deliver to Basic/Pro subscribers who have this symbol enabled
		// and have not already received a playbook alert this cycle.
		// Skip admin chat ID — already received via SendToAdmin above.
		subsSent := 0
		for _, sub := range subs {
			tier := sub.Tier
			if tier == "" {
				tier = "free"
			}
			if tier == "free" || sub.ChatID == 0 {
				continue
			}
			if sub.ChatID == adminChatID {
				continue // admin already notified via SendToAdmin
			}
			if subReceived[sub.ChatID] {
				continue // already got the highest-priority alert this cycle
			}
			for _, sym := range allowedSymbols(sub) {
				if sym == c.symbol {
					if err := w.notifier.SendMessage(ctx, sub.ChatID, c.msg); err != nil {
						log.Printf("[playbook] SendMessage(chat=%d) %s %s: %v", sub.ChatID, c.symbol, c.stage, err)
					} else {
						subsSent++
						subReceived[sub.ChatID] = true
					}
					break
				}
			}
		}
		log.Printf("[playbook] %s %s dispatched to %d subscribers", c.symbol, c.stage, subsSent)

		// Persist cooldown to Supabase so restarts don't bypass the 30-min window.
		firedAt := time.Now()
		cooldownKey := fmt.Sprintf("%s:%s", c.symbol, c.stage)
		go func(key string, score int, at time.Time) {
			if err := w.db.UpsertPlaybookCooldown(context.Background(), key, at, score); err != nil {
				log.Printf("[playbook] persist cooldown %s: %v", key, err)
			}
			// If confirmed, also persist the forming block so it can't fire during cooldown.
			if strings.HasSuffix(key, ":confirmed") {
				sym := strings.TrimSuffix(key, ":confirmed")
				if err := w.db.UpsertPlaybookCooldown(context.Background(), sym+":forming", at, 999); err != nil {
					log.Printf("[playbook] persist forming block %s: %v", sym+":forming", err)
				}
			}
		}(cooldownKey, c.score, firedAt)

		w.playbookStates.set(c.symbol, c.state)
		if c.stage == "confirmed" {
			w.followThrough.record(c.symbol, c.side, c.currentPrice, c.avgRangePct, c.score)
		}
		sent++
	}
}

// candidateRank returns a sort key — higher = more important.
// confirmed outranks forming; within same stage, higher score wins.
func candidateRank(c *playbookCandidate) int {
	if c.stage == "confirmed" {
		return 1000 + c.score
	}
	return c.score
}

// ── Candle analysis helpers ───────────────────────────────────────────────────

func wickPastLevel(c models.Kline, side string, clusterPrice float64) bool {
	if side == "long" {
		return c.Low < clusterPrice
	}
	return c.High > clusterPrice
}

func rejectionMetrics(c models.Kline, side string, clusterPrice float64) (wick, body, depth float64) {
	if side == "long" {
		if c.Low >= clusterPrice {
			return 0, 0, 0
		}
		wick = clusterPrice - c.Low
		body = math.Max(0, c.Close-clusterPrice)
		depth = wick
	} else {
		if c.High <= clusterPrice {
			return 0, 0, 0
		}
		wick = c.High - clusterPrice
		body = math.Max(0, clusterPrice-c.Close)
		depth = wick
	}
	return
}

func averageCandleRange(candles []models.Kline) float64 {
	if len(candles) == 0 {
		return 0
	}
	sum := 0.0
	for _, c := range candles {
		sum += c.High - c.Low
	}
	return sum / float64(len(candles))
}

// trendPressure returns a proportional score penalty based on how many of the
// last 3 closed candles show expanding-range momentum against the expected
// rejection direction.
//
//	2 opposing candles (moderate pressure)      → -10
//	2 opposing candles + strong final expansion → -20
func trendPressure(candles []models.Kline, side string) int {
	if len(candles) < 3 {
		return 0
	}
	last := candles[len(candles)-3:]
	badCount := 0
	strongExpansion := false
	prevRange := last[0].High - last[0].Low

	for i := 1; i < len(last); i++ {
		r := last[i].High - last[i].Low
		isBearish := last[i].Close < last[i].Open
		isBullish := last[i].Close > last[i].Open
		expanding := r >= prevRange*0.9

		if side == "long" && isBearish && expanding {
			badCount++
			if r >= prevRange*1.5 {
				strongExpansion = true
			}
		} else if side == "short" && isBullish && expanding {
			badCount++
			if r >= prevRange*1.5 {
				strongExpansion = true
			}
		}
		prevRange = r
	}

	switch {
	case badCount >= 2 && strongExpansion:
		return -20
	case badCount >= 2:
		return -10
	default:
		return 0
	}
}

// ── Signal scoring (0–100) ────────────────────────────────────────────────────

func scoreRejection(wick, body, depth, clusterPrice float64, side string, regime models.MarketRegime, candleRange, avgRange float64) int {
	score := 0

	// Reclaim strength (0–40)
	if wick > 0 {
		reclaimRatio := body / wick
		score += int(math.Min(reclaimRatio*50, 40))
	}

	// Penetration depth (0–20)
	depthPct := depth / clusterPrice * 100
	if depthPct >= 0.3 {
		score += 20
	} else if depthPct >= 0.15 {
		score += 10
	}

	// Bias alignment (0–20)
	if isBiasAligned(side, regime) {
		score += 20
	}

	// Candle momentum (0–20)
	if avgRange > 0 && candleRange >= avgRange*0.8 {
		score += 20
	} else if avgRange > 0 && candleRange >= avgRange*0.5 {
		score += 10
	}

	return score
}

func isBiasAligned(side string, regime models.MarketRegime) bool {
	if side == "long" {
		return regime == models.RegimeLiquidation || regime == models.RegimeDistribution
	}
	return regime == models.RegimeTrending || regime == models.RegimeAccumulation
}

// ── Alert formatters ──────────────────────────────────────────────────────────

func buildFormingAlert(symbol string, m *models.LiquidationMagnet, sigs models.MarketSignals) string {
	sweepArrow := "↓"
	if m.Side == "short" {
		sweepArrow = "↑"
	}
	aligned := isBiasAligned(m.Side, sigs.Regime)
	biasLabel := "⚠️ Counter-trend"
	actionLine := "→ Wait for 5m close + extra confirmation before acting"
	if aligned {
		biasLabel = "✅ Aligned"
		actionLine = "→ Watch for 5m close — reaction holds = valid setup"
	}
	wickDir := "below"
	if m.Side == "short" {
		wickDir = "above"
	}
	return fmt.Sprintf(
		"⚡ <b>%s — Rejection Forming</b>\n\nLevel: %s (%s cluster %s)\nBias: %s\n\nWick %s level\nWatching for 5m close\n\n%s",
		symbol,
		formatPriceStr(m.Price),
		m.Side,
		sweepArrow,
		biasLabel,
		wickDir,
		actionLine,
	)
}

func buildConfirmedAlert(symbol string, m *models.LiquidationMagnet, sigs models.MarketSignals, score int, aligned bool) string {
	sweepArrow := "↓"
	if m.Side == "short" {
		sweepArrow = "↑"
	}
	biasLabel := "⚠️ Counter-trend"
	actionLine := "→ Counter-trend: wait for extra confirmation"
	if aligned {
		biasLabel = "✅ Aligned"
		actionLine = "→ Watch for continuation if reaction holds"
	}
	return fmt.Sprintf(
		"✅ <b>%s — Rejection Confirmed</b>\n\nLevel: %s (%s cluster %s)\nStrength: %d%% (%s)\nBias: %s\n\nPlaybook active\n\n%s",
		symbol,
		formatPriceStr(m.Price),
		m.Side,
		sweepArrow,
		score,
		strengthLabel(score),
		biasLabel,
		actionLine,
	)
}

func strengthLabel(score int) string {
	switch {
	case score >= 75:
		return "Strong"
	case score >= 50:
		return "Moderate"
	default:
		return "Weak"
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// playbookMinClusterSize returns the minimum cluster size in USD for a symbol.
// Mirrors the frontend defaults in clusterSizeOptionsForSymbol.
func playbookMinClusterSize(symbol string) float64 {
	switch symbol {
	case "BTC":
		return playbookMinClusterBTC
	case "ETH":
		return playbookMinClusterETH
	default:
		return playbookMinClusterDefault
	}
}

// sweepDirStr returns the expected sweep direction for a given cluster side.
func sweepDirStr(side string) string {
	if side == "long" {
		return "downward"
	}
	return "upward"
}
