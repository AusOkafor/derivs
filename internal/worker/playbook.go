package worker

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"
)

const (
	playbookProximityPct  = 0.3  // trigger check when within 0.3% of cluster
	playbookCooldown      = 30 * time.Minute
	playbookDisplacementR = 0.3  // close must push back at least 30% of wick size
	playbookKlineInterval = "5m"
	playbookKlineLimit    = 5
)

// playbookCooldowns tracks the last fire time per "SYMBOL:level" key.
type playbookCooldowns struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func newPlaybookCooldowns() *playbookCooldowns {
	return &playbookCooldowns{last: make(map[string]time.Time)}
}

func (c *playbookCooldowns) allow(symbol string, level float64) bool {
	key := fmt.Sprintf("%s:%.2f", symbol, level)
	c.mu.Lock()
	defer c.mu.Unlock()
	if t, ok := c.last[key]; ok && time.Since(t) < playbookCooldown {
		return false
	}
	c.last[key] = time.Now()
	return true
}

// checkPlaybookTriggers scans all pro-cycle snapshots for rejection candles
// forming at liquidation cluster levels. Fires a Telegram alert to all pro
// and basic subscribers when a displacement rejection is detected.
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
		if len(candles) == 0 {
			continue
		}

		// Evaluate latest candle first, then fallback to previous ones.
		detected := false
		for i := len(candles) - 1; i >= 0; i-- {
			c := candles[i]
			clusterPrice := m.Price

			if m.Side == "long" {
				// Long cluster: expect sweep below cluster level with close back above.
				if c.Low < clusterPrice && c.Close > clusterPrice {
					wickSize := clusterPrice - c.Low
					bodyClose := c.Close - clusterPrice
					if wickSize > 0 && bodyClose >= wickSize*playbookDisplacementR {
						detected = true
						break
					}
				}
			} else {
				// Short cluster: expect sweep above cluster level with close back below.
				if c.High > clusterPrice && c.Close < clusterPrice {
					wickSize := c.High - clusterPrice
					bodyClose := clusterPrice - c.Close
					if wickSize > 0 && bodyClose >= wickSize*playbookDisplacementR {
						detected = true
						break
					}
				}
			}
		}

		if !detected {
			continue
		}
		if !w.playbookCooldown.allow(symbol, m.Price) {
			log.Printf("[playbook] %s cooldown active at %.2f, skipping", symbol, m.Price)
			continue
		}

		biasLabel := string(sa.sigs.Regime)
		sweepDir := "downward"
		if m.Side == "short" {
			sweepDir = "upward"
		}
		msg := fmt.Sprintf(
			"⚡ <b>%s Playbook Trigger</b>\n\n%s bias intact\nSweep at %s (%s)\n\n5m rejection detected — await candle close confirmation\nSweep probability: %d%%",
			symbol,
			biasLabel,
			formatPriceStr(m.Price),
			sweepDir,
			m.Probability,
		)

		log.Printf("[playbook] %s rejection detected at %.2f (prob=%d%%), sending alert", symbol, m.Price, m.Probability)

		if err := w.notifier.SendToAdmin(msg); err != nil {
			log.Printf("[playbook] SendToAdmin(%s): %v", symbol, err)
		}
	}
}
