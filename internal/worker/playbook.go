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
	playbookProximityPct  = 0.3 // check when within 0.3% of cluster
	playbookCooldown      = 30 * time.Minute
	playbookDisplacementR = 0.3 // confirmed: close must reclaim >30% of wick
	playbookMinMomentum   = 0.4 // candle range must be ≥40% of prior avg to count
	playbookKlineInterval = "5m"
	playbookKlineLimit    = 6 // 5 closed + 1 open (current)
)

// playbookCooldowns tracks the last fire time per "SYMBOL:level:stage" key.
// Stage is "forming" or "confirmed" — each fires at most once per cooldown window.
type playbookCooldowns struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func newPlaybookCooldowns() *playbookCooldowns {
	return &playbookCooldowns{last: make(map[string]time.Time)}
}

func (c *playbookCooldowns) allow(symbol string, level float64, stage string) bool {
	key := fmt.Sprintf("%s:%.2f:%s", symbol, level, stage)
	c.mu.Lock()
	defer c.mu.Unlock()
	if t, ok := c.last[key]; ok && time.Since(t) < playbookCooldown {
		return false
	}
	c.last[key] = time.Now()
	return true
}

// checkPlaybookTriggers scans pro-cycle snapshots for candle rejections at
// liquidation cluster levels. Fires two stages:
//
//   - "forming"   — open candle has wick past the cluster level (early warning)
//   - "confirmed" — a closed candle satisfies the displacement rule (actionable)
func (w *Worker) checkPlaybookTriggers(ctx context.Context, snapshots map[string]symbolAlerts) {
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
		// Need at least 2 candles: 1 closed reference + 1 current
		if len(candles) < 2 {
			continue
		}

		// Binance returns candles oldest-first; last entry is the current open candle.
		currentCandle := candles[len(candles)-1]
		closedCandles := candles[:len(candles)-1]

		// Average range of closed candles — used for momentum filter.
		avgRange := averageCandleRange(closedCandles)

		// ── Stage 1: Forming (current open candle) ───────────────────────────
		if wickPastLevel(currentCandle, m.Side, m.Price) {
			candleRange := currentCandle.High - currentCandle.Low
			if avgRange == 0 || candleRange >= avgRange*playbookMinMomentum {
				if w.playbookCooldown.allow(symbol, m.Price, "forming") {
					msg := buildFormingAlert(symbol, m, sa.sigs.Regime)
					log.Printf("[playbook] %s forming at %.4f (prob=%d%%)", symbol, m.Price, m.Probability)
					if err := w.notifier.SendToAdmin(msg); err != nil {
						log.Printf("[playbook] SendToAdmin forming(%s): %v", symbol, err)
					}
				}
			}
		}

		// ── Stage 2: Confirmed (most recent closed candle first) ─────────────
		for i := len(closedCandles) - 1; i >= 0; i-- {
			c := closedCandles[i]
			wick, body, depth := rejectionMetrics(c, m.Side, m.Price)
			if wick == 0 {
				continue // no wick past the level
			}
			if body < wick*playbookDisplacementR {
				continue // insufficient reclaim
			}

			// Momentum: candle range vs average (excluding itself)
			candleRange := c.High - c.Low
			if avgRange > 0 && candleRange < avgRange*playbookMinMomentum {
				log.Printf("[playbook] %s closed candle too small (%.4f < %.4f avg), skipping", symbol, candleRange, avgRange)
				break
			}

			score := scoreRejection(wick, body, depth, m.Price, m.Side, sa.sigs.Regime, candleRange, avgRange)
			if w.playbookCooldown.allow(symbol, m.Price, "confirmed") {
				msg := buildConfirmedAlert(symbol, m, sa.sigs.Regime, score)
				log.Printf("[playbook] %s confirmed at %.4f score=%d (prob=%d%%)", symbol, m.Price, score, m.Probability)
				if err := w.notifier.SendToAdmin(msg); err != nil {
					log.Printf("[playbook] SendToAdmin confirmed(%s): %v", symbol, err)
				}
			}
			break // only evaluate the most recent valid closed candle
		}
	}
}

// ── Candle analysis helpers ───────────────────────────────────────────────────

// wickPastLevel returns true if the candle's wick crossed the cluster price.
func wickPastLevel(c models.Kline, side string, clusterPrice float64) bool {
	if side == "long" {
		return c.Low < clusterPrice
	}
	return c.High > clusterPrice
}

// rejectionMetrics returns (wickSize, bodyClose, penetrationDepth) for a closed candle.
// wickSize    = how far price moved past the cluster level
// bodyClose   = how far the close reclaimed past the level
// depth       = penetration depth (same as wickSize — exposed for scoring)
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

// averageCandleRange returns the mean (high-low) range of a set of candles.
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

// ── Signal scoring (0–100) ────────────────────────────────────────────────────

// scoreRejection grades the quality of a confirmed rejection candle.
//
// Components:
//   - Reclaim strength  (0–40): how much of the wick was reclaimed
//   - Penetration depth (0–20): deep sweeps sweep more liquidity → stronger
//   - Bias alignment    (0–20): cluster direction matches market regime
//   - Candle momentum   (0–20): candle is meaningfully sized vs recent average
func scoreRejection(wick, body, depth, clusterPrice float64, side string, regime models.MarketRegime, candleRange, avgRange float64) int {
	score := 0

	// Reclaim strength — capped at 40
	if wick > 0 {
		reclaimRatio := body / wick
		score += int(math.Min(reclaimRatio*50, 40))
	}

	// Penetration depth — expressed as % of cluster price
	depthPct := depth / clusterPrice * 100
	if depthPct >= 0.3 {
		score += 20
	} else if depthPct >= 0.15 {
		score += 10
	}

	// Bias alignment — long cluster swept in liquidation/distribution (bearish context)
	// short cluster swept in trending/accumulation (bullish context)
	if side == "long" && (regime == models.RegimeLiquidation || regime == models.RegimeDistribution) {
		score += 20
	} else if side == "short" && (regime == models.RegimeTrending || regime == models.RegimeAccumulation) {
		score += 20
	}

	// Candle momentum
	if avgRange > 0 && candleRange >= avgRange*0.8 {
		score += 20
	} else if avgRange > 0 && candleRange >= avgRange*0.5 {
		score += 10
	}

	return score
}

// ── Alert formatters ──────────────────────────────────────────────────────────

func buildFormingAlert(symbol string, m *models.LiquidationMagnet, regime models.MarketRegime) string {
	sweepDir := "downward"
	if m.Side == "short" {
		sweepDir = "upward"
	}
	return fmt.Sprintf(
		"⚡ <b>%s Rejection Forming</b>\n\n%s bias | %s cluster at %s (%s)\n\nWick past level — watch for candle close\nProximity: %.2f%% from level",
		symbol,
		string(regime),
		m.Side,
		formatPriceStr(m.Price),
		sweepDir,
		m.Distance,
	)
}

func buildConfirmedAlert(symbol string, m *models.LiquidationMagnet, regime models.MarketRegime, score int) string {
	sweepDir := "downward"
	if m.Side == "short" {
		sweepDir = "upward"
	}
	strengthLabel := strengthLabel(score)
	return fmt.Sprintf(
		"✅ <b>%s Confirmed Rejection</b>\n\n%s bias intact | %s cluster at %s (%s)\n\n5m candle closed — playbook active\n\nSetup strength: %d%% (%s)",
		symbol,
		string(regime),
		m.Side,
		formatPriceStr(m.Price),
		sweepDir,
		score,
		strengthLabel,
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
