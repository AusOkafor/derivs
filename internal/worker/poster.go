package worker

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"derivs-backend/internal/models"
)

// schedulePoster wires up 3 daily post generations: 8am, 2pm, 8pm UTC.
func (w *Worker) schedulePoster() {
	times := []int{8, 14, 20}
	for _, hour := range times {
		h := hour // capture for closure
		target := time.Date(0, 1, 1, h, 0, 0, 0, time.UTC)
		scheduleDaily(target, func() {
			go w.generateAndSendPost(context.Background())
		})
	}
}

// TriggerPost allows external callers (e.g. admin handler) to fire a post immediately.
func (w *Worker) TriggerPost(ctx context.Context) {
	w.generateAndSendPost(ctx)
}

// generateAndSendPost scans all cached snapshots, picks the strongest signal,
// formats a tweet-ready post, and sends it to the admin via Telegram.
func (w *Worker) generateAndSendPost(ctx context.Context) {
	w.lastSnapMu.Lock()
	snapshots := make(map[string]symbolAlerts, len(w.lastSnapshots))
	for k, v := range w.lastSnapshots {
		snapshots[k] = v
	}
	w.lastSnapMu.Unlock()

	if len(snapshots) == 0 {
		log.Println("poster: no snapshots available, skipping")
		return
	}

	best, bestScore := "", 0
	for symbol, sa := range snapshots {
		score := scoreSignal(sa.snap, sa.sigs)
		if score > bestScore {
			bestScore = score
			best = symbol
		}
	}

	if best == "" || bestScore < 30 {
		log.Printf("poster: no strong signal found (best score: %d), skipping", bestScore)
		return
	}

	sa := snapshots[best]
	post := formatPost(sa.snap, sa.sigs)

	msg := fmt.Sprintf("📢 <b>SUGGESTED X POST</b>\n\n<code>%s</code>", post)
	if err := w.notifier.SendToAdmin(msg); err != nil {
		log.Printf("poster: failed to send post to admin: %v", err)
		return
	}
	log.Printf("poster: sent suggested post for %s (score: %d)", best, bestScore)
}

// scoreSignal returns a 0-100+ score indicating how actionable the current signal is.
// Higher = stronger, more tweet-worthy setup.
func scoreSignal(snap models.MarketSnapshot, sigs models.MarketSignals) int {
	score := 0

	// Sweep probability from nearest liquidation magnet
	if sigs.LiquidationMagnet != nil {
		score += sigs.LiquidationMagnet.Probability

		// Timing bonus
		dist := sigs.LiquidationMagnet.Distance
		if dist < 0.3 {
			score += 30 // IMMINENT
		} else if dist < 0.8 {
			score += 15 // FORMING
		}

		// Large cluster bonus
		if sigs.LiquidationMagnet.SizeUSD >= 500_000 {
			score += 10
		}
	}

	// Cascade risk bonus
	switch sigs.CascadeRisk.Level {
	case "HIGH", "CRITICAL":
		score += 20
	case "MEDIUM":
		score += 10
	}

	// Strong directional pressure
	lpi := sigs.LiquidityPressure.Score
	if lpi < 0 {
		lpi = -lpi
	}
	if lpi > 40 {
		score += 10
	}

	// Regime bonus — liquidation events are most tweet-worthy
	if sigs.Regime == models.RegimeLiquidation {
		score += 15
	}

	return score
}

// formatPost generates a tweet-ready plain-text post from the signal data.
func formatPost(snap models.MarketSnapshot, sigs models.MarketSignals) string {
	symbol := snap.Symbol

	avgLong := avgLongPct(snap)
	fundingPct := snap.FundingRate.Rate * 100
	fundingSign := "+"
	if fundingPct < 0 {
		fundingSign = ""
	}

	var sb strings.Builder

	if sigs.LiquidationMagnet != nil {
		m := sigs.LiquidationMagnet
		clusterStr := formatClusterSize(m.SizeUSD)
		priceStr := formatPrice(m.Price)
		sideLabel := strings.ToLower(m.Side)

		// Series header
		sb.WriteString("DerivLens — Liquidity Watch\n\n")

		// Line 1: symbol + cluster
		sb.WriteString(fmt.Sprintf("%s — %s %s cluster at %s\n", symbol, clusterStr, sideLabel, priceStr))
		sb.WriteString("Liquidity in focus.\n\n")

		// Line 2: context block
		sb.WriteString("Context:\n")
		sb.WriteString(fmt.Sprintf("• %.0f%% longs (crowded)\n• Funding: %s%.4f%%\n\n",
			avgLong, fundingSign, fundingPct))

		// Line 3: model read
		sb.WriteString("Model favors a sweep first.\n\n")

		// Line 4: plan — anchor price in plan
		sb.WriteString(fmt.Sprintf("Plan:\nWatch reaction at %s\nSweep → reversal or continuation\n\n", priceStr))
		sb.WriteString("No confirmation = no trade.\n\n")
	} else {
		// No magnet — regime-based post
		sb.WriteString("DerivLens — Liquidity Watch\n\n")
		sb.WriteString(fmt.Sprintf("%s — %s\n\n", symbol, string(sigs.Regime)))
		sb.WriteString("Context:\n")
		sb.WriteString(fmt.Sprintf("• %.0f%% longs, funding %s%.4f%%\n• Cascade risk %s (%d/100)\n\n",
			avgLong, fundingSign, fundingPct, sigs.CascadeRisk.Level, sigs.CascadeRisk.Score))
		sb.WriteString("No strong liquidity magnet nearby — range conditions.\nFade extremes, wait for a trigger.\n\n")
	}

	sb.WriteString("Track live → derivlens.io")
	return sb.String()
}

func avgLongPct(snap models.MarketSnapshot) float64 {
	if len(snap.LongShortRatios) == 0 {
		return 50.0
	}
	sum := 0.0
	for _, r := range snap.LongShortRatios {
		sum += r.LongPct
	}
	return sum / float64(len(snap.LongShortRatios))
}

func formatPrice(p float64) string {
	if p >= 1000 {
		return fmt.Sprintf("$%.0f", p)
	} else if p >= 1 {
		return fmt.Sprintf("$%.4f", p)
	}
	return fmt.Sprintf("$%.6f", p)
}

func formatClusterSize(v float64) string {
	if v >= 1_000_000 {
		return fmt.Sprintf("$%.1fM", v/1_000_000)
	}
	return fmt.Sprintf("$%.0fK", v/1_000)
}

