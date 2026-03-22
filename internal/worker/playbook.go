package worker

import (
	"context"
	"fmt"
	"log"
	"math"
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
)

// ── Cooldown with score memory ────────────────────────────────────────────────

type cooldownEntry struct {
	firedAt time.Time
	score   int
}

// playbookCooldowns tracks fire time + score per "SYMBOL:level:stage" key.
// A new signal can override the cooldown if its score beats the previous by ≥15.
type playbookCooldowns struct {
	mu      sync.Mutex
	entries map[string]cooldownEntry
}

func newPlaybookCooldowns() *playbookCooldowns {
	return &playbookCooldowns{entries: make(map[string]cooldownEntry)}
}

func (c *playbookCooldowns) allow(symbol string, level float64, stage string, newScore int) bool {
	key := fmt.Sprintf("%s:%.2f:%s", symbol, level, stage)
	c.mu.Lock()
	defer c.mu.Unlock()

	if e, ok := c.entries[key]; ok && time.Since(e.firedAt) < playbookCooldown {
		// Within cooldown — only allow if new score is meaningfully better
		if newScore < e.score+playbookScoreOverride {
			return false
		}
		log.Printf("[playbook] score override: %s %s prev=%d new=%d", symbol, stage, e.score, newScore)
	}
	c.entries[key] = cooldownEntry{firedAt: time.Now(), score: newScore}
	return true
}

// ── Follow-through tracker ────────────────────────────────────────────────────

type followThroughEntry struct {
	symbol    string
	side      string  // cluster side: "long" or "short"
	firePrice float64 // price when confirmed signal fired
	fireTime  time.Time
	checked   bool
}

type followThroughTracker struct {
	mu      sync.Mutex
	pending []followThroughEntry
}

func newFollowThroughTracker() *followThroughTracker {
	return &followThroughTracker{}
}

func (ft *followThroughTracker) record(symbol, side string, price float64) {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	ft.pending = append(ft.pending, followThroughEntry{
		symbol:    symbol,
		side:      side,
		firePrice: price,
		fireTime:  time.Now(),
	})
}

// evaluate checks pending signals that are 10–20 min old against current prices.
// Logs outcome: whether price moved in the expected direction. No alerts fired yet.
func (ft *followThroughTracker) evaluate(snapshots map[string]symbolAlerts) {
	ft.mu.Lock()
	defer ft.mu.Unlock()

	now := time.Now()
	var remaining []followThroughEntry
	for _, e := range ft.pending {
		age := now.Sub(e.fireTime)
		if age < 10*time.Minute {
			remaining = append(remaining, e) // too early
			continue
		}
		if age > 20*time.Minute || e.checked {
			continue // expired, drop
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
		// Long cluster rejection expects price UP; short expects price DOWN.
		worked := (e.side == "long" && pctMove > 0.1) || (e.side == "short" && pctMove < -0.1)
		if worked {
			log.Printf("[playbook:ft] ✅ %s %s rejection held — moved %.2f%% in target direction", e.symbol, e.side, math.Abs(pctMove))
		} else {
			log.Printf("[playbook:ft] ❌ %s %s rejection failed — moved %.2f%% (wrong direction)", e.symbol, e.side, pctMove)
		}
		e.checked = true
	}
	ft.pending = remaining
}

// ── Main trigger checker ──────────────────────────────────────────────────────

// checkPlaybookTriggers scans pro-cycle snapshots for rejection candles at
// liquidation cluster levels. Fires two stages:
//
//   - "forming"   — open candle has wick past cluster level (early warning)
//   - "confirmed" — closed candle satisfies displacement + momentum + trend filter
func (w *Worker) checkPlaybookTriggers(ctx context.Context, snapshots map[string]symbolAlerts) {
	// Evaluate any pending follow-through checks first
	w.followThrough.evaluate(snapshots)

	for symbol, sa := range snapshots {
		m := sa.sigs.LiquidationMagnet
		if m == nil {
			continue
		}
		if m.Distance >= playbookProximityPct {
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

		// Binance returns oldest-first; last candle is the current open candle.
		currentCandle := candles[len(candles)-1]
		closedCandles := candles[:len(candles)-1]
		avgRange := averageCandleRange(closedCandles)

		// ── Stage 1: Forming ─────────────────────────────────────────────────
		if wickPastLevel(currentCandle, m.Side, m.Price) {
			candleRange := currentCandle.High - currentCandle.Low
			if avgRange == 0 || candleRange >= avgRange*playbookMinMomentum {
				if w.playbookCooldown.allow(symbol, m.Price, "forming", 0) {
					msg := buildFormingAlert(symbol, m, sa.sigs)
					log.Printf("[playbook] %s forming at %.4f (prob=%d%%)", symbol, m.Price, m.Probability)
					if err := w.notifier.SendToAdmin(msg); err != nil {
						log.Printf("[playbook] SendToAdmin forming(%s): %v", symbol, err)
					}
				}
			}
		}

		// ── Stage 2: Confirmed ───────────────────────────────────────────────
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
				log.Printf("[playbook] %s closed candle too small (%.4f < %.4f avg), skipping", symbol, candleRange, avgRange*playbookMinMomentum)
				break
			}

			// Trend pressure penalty — strong trend continuation reduces score
			trendPenalty := trendPressure(closedCandles, m.Side)

			score := scoreRejection(wick, body, depth, m.Price, m.Side, sa.sigs.Regime, candleRange, avgRange)
			score = max(0, score+trendPenalty)

			aligned := isBiasAligned(m.Side, sa.sigs.Regime)
			if w.playbookCooldown.allow(symbol, m.Price, "confirmed", score) {
				currentPrice := sa.snap.LiquidationMap.CurrentPrice
				msg := buildConfirmedAlert(symbol, m, sa.sigs, score, aligned)
				log.Printf("[playbook] %s confirmed at %.4f score=%d aligned=%v trend_penalty=%d",
					symbol, m.Price, score, aligned, trendPenalty)
				if err := w.notifier.SendToAdmin(msg); err != nil {
					log.Printf("[playbook] SendToAdmin confirmed(%s): %v", symbol, err)
				}
				// Record for follow-through evaluation
				w.followThrough.record(symbol, m.Side, currentPrice)
			}
			break
		}
	}
}

// ── Candle analysis helpers ───────────────────────────────────────────────────

func wickPastLevel(c models.Kline, side string, clusterPrice float64) bool {
	if side == "long" {
		return c.Low < clusterPrice
	}
	return c.High > clusterPrice
}

// rejectionMetrics returns (wickSize, bodyClose, penetrationDepth).
// Returns (0,0,0) if no wick past the level.
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

// trendPressure returns a score penalty if the last 3 candles show strong
// directional momentum against the expected rejection direction.
// A long cluster expects price to bounce UP — if candles are trending down with
// expanding range, the rejection is fighting the trend.
func trendPressure(candles []models.Kline, side string) int {
	if len(candles) < 3 {
		return 0
	}
	last := candles[len(candles)-3:]
	badCandles := 0
	prevRange := last[0].High - last[0].Low
	for i := 1; i < len(last); i++ {
		r := last[i].High - last[i].Low
		isBearish := last[i].Close < last[i].Open
		isBullish := last[i].Close > last[i].Open
		expanding := r >= prevRange*0.9
		if side == "long" && isBearish && expanding {
			badCandles++
		} else if side == "short" && isBullish && expanding {
			badCandles++
		}
		prevRange = r
	}
	if badCandles >= 2 {
		return -20 // strong trend against us — downgrade
	}
	return 0
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

// isBiasAligned returns true when the cluster's sweep direction matches
// the broader market regime.
func isBiasAligned(side string, regime models.MarketRegime) bool {
	if side == "long" {
		return regime == models.RegimeLiquidation || regime == models.RegimeDistribution
	}
	return regime == models.RegimeTrending || regime == models.RegimeAccumulation
}

// ── Alert formatters ──────────────────────────────────────────────────────────

func buildFormingAlert(symbol string, m *models.LiquidationMagnet, sigs models.MarketSignals) string {
	sweepDir := "downward"
	if m.Side == "short" {
		sweepDir = "upward"
	}
	alignLabel := "⚠️ counter-trend"
	if isBiasAligned(m.Side, sigs.Regime) {
		alignLabel = "✅ aligned with market flow"
	}
	return fmt.Sprintf(
		"⚡ <b>%s Rejection Forming</b>\n\n%s bias | %s cluster at %s (%s)\nBias: %s\n\nWick past level — watch for candle close\nProximity: %.2f%% from level",
		symbol,
		string(sigs.Regime),
		m.Side,
		formatPriceStr(m.Price),
		sweepDir,
		alignLabel,
		m.Distance,
	)
}

func buildConfirmedAlert(symbol string, m *models.LiquidationMagnet, sigs models.MarketSignals, score int, aligned bool) string {
	sweepDir := "downward"
	if m.Side == "short" {
		sweepDir = "upward"
	}
	alignLabel := "⚠️ counter-trend — trade with caution"
	if aligned {
		alignLabel = "✅ aligned with market flow"
	}
	return fmt.Sprintf(
		"✅ <b>%s Confirmed Rejection</b>\n\n%s bias | %s cluster at %s (%s)\n\n5m candle closed — playbook active\n\nSetup strength: %d%% (%s)\nBias: %s",
		symbol,
		string(sigs.Regime),
		m.Side,
		formatPriceStr(m.Price),
		sweepDir,
		score,
		strengthLabel(score),
		alignLabel,
	)
}

func strengthLabel(score int) string {
	switch {
	case score >= 75:
		return "Strong rejection"
	case score >= 50:
		return "Moderate rejection"
	default:
		return "Weak — wait for more confirmation"
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
