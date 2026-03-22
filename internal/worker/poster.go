package worker

import (
	"context"
	"fmt"
	"log"
	"math"
	"strings"
	"time"

	"derivs-backend/internal/cards"
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
		log.Println("poster: no snapshots available — worker cache empty, skipping")
		return
	}
	log.Printf("poster: scanning %d symbols from cache", len(snapshots))

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
	log.Printf("poster: best signal = %s (score %d)", best, bestScore)
	if m := sa.sigs.LiquidationMagnet; m != nil {
		log.Printf("poster: %s magnet = %s cluster at %.2f, size=%.0f, prob=%d%%", best, m.Side, m.Price, m.SizeUSD, m.Probability)
	} else {
		log.Printf("poster: %s has no liquidation magnet", best)
	}

	// Enforce per-symbol minimum cluster size — small clusters on large caps aren't tweet-worthy.
	if sa.sigs.LiquidationMagnet != nil {
		minCluster := minClusterUSD(best)
		if sa.sigs.LiquidationMagnet.SizeUSD < minCluster {
			log.Printf("poster: %s cluster too small (%.0f < %.0f), skipping", best, sa.sigs.LiquidationMagnet.SizeUSD, minCluster)
			return
		}
		// Hard floor on sweep probability — low probability setups produce misleading posts.
		if sa.sigs.LiquidationMagnet.Probability < 50 {
			log.Printf("poster: %s sweep probability too low (%d%% < 50%%), skipping", best, sa.sigs.LiquidationMagnet.Probability)
			return
		}
	}

	post := formatPost(sa.snap, sa.sigs)

	// Generate a visual card to attach to the X post.
	cardData := buildPostCardData(sa.snap, sa.sigs)
	imgBytes, cardErr := cards.GenerateAlertCard(cardData)
	if cardErr == nil {
		// Send image + post text as caption so admin can attach both to the tweet.
		caption := fmt.Sprintf("📢 SUGGESTED X POST\n\n%s", post)
		if err := w.notifier.SendPhotoToAdmin(imgBytes, caption); err != nil {
			log.Printf("poster: failed to send card to admin: %v, falling back to text", err)
			cardErr = err
		}
	}
	if cardErr != nil {
		// Fall back to text-only if card generation or delivery failed.
		msg := fmt.Sprintf("📢 <b>SUGGESTED X POST</b>\n\n<code>%s</code>", post)
		if err := w.notifier.SendToAdmin(msg); err != nil {
			log.Printf("poster: failed to send post to admin: %v", err)
			return
		}
	}
	log.Printf("poster: sent suggested post for %s (score: %d, card: %v)", best, bestScore, cardErr == nil)
}

// minClusterUSD returns the minimum liquidation cluster size required before posting for a symbol.
// Large caps need bigger clusters to be tweet-worthy.
// minClusterUSD returns the minimum liquidation cluster size required before posting.
// Thresholds are calibrated per coin — large caps need bigger clusters to be tweet-worthy.
func minClusterUSD(symbol string) float64 {
	switch symbol {
	// Tier 1 — mega caps: need large clusters to move price meaningfully
	case "BTCUSDT":
		return 1_000_000 // $1M
	case "ETHUSDT":
		return 750_000 // $750K
	case "BNBUSDT":
		return 500_000 // $500K
	case "XRPUSDT":
		return 400_000 // $400K

	// Tier 2 — large caps: $300K+
	case "SOLUSDT":
		return 300_000
	case "DOGEUSDT":
		return 300_000
	case "AVAXUSDT":
		return 250_000
	case "LINKUSDT":
		return 250_000
	case "TONUSDT":
		return 250_000

	// Tier 3 — mid caps: $150K+
	case "ARBUSDT":
		return 150_000
	case "OPUSDT":
		return 150_000
	case "INJUSDT":
		return 150_000
	case "SUIUSDT":
		return 150_000

	// Tier 4 — smaller / newer: $100K+
	case "WLDUSDT":
		return 100_000
	case "TIAUSDT":
		return 100_000
	case "PENDLEUSDT":
		return 100_000

	default:
		return 150_000
	}
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

		// Direction: a long cluster gets swept downward; a short cluster gets swept upward.
		sweepDir := "downward"
		if sideLabel == "short" {
			sweepDir = "upward"
		}

		// Series header
		sb.WriteString("DerivLens — Liquidity Watch\n\n")

		// Line 1: symbol + cluster
		sb.WriteString(fmt.Sprintf("%s — %s %s cluster at %s\n", symbol, clusterStr, sideLabel, priceStr))
		sb.WriteString("Liquidity in focus.\n\n")

		// Line 2: context block
		sb.WriteString("Context:\n")
		sb.WriteString(fmt.Sprintf("• %.2f%% from the level\n• %.0f%% longs\n• Funding: %s%.4f%%\n\n",
			m.Distance, avgLong, fundingSign, fundingPct))

		// Line 3: model read — explicit direction + probability
		sb.WriteString(fmt.Sprintf("Model favors a %s sweep first. Sweep probability: %d%%.\n\n", sweepDir, m.Probability))

		// Line 4: plan
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

// buildPostCardData constructs AlertCardData for the poster card from live snapshot + signals.
func buildPostCardData(snap models.MarketSnapshot, sigs models.MarketSignals) cards.AlertCardData {
	clusterPrice := 0.0
	clusterSize := 0.0
	distance := 0.0
	sweepProb := 0
	sev := "MEDIUM"

	if sigs.LiquidationMagnet != nil {
		m := sigs.LiquidationMagnet
		clusterPrice = m.Price
		clusterSize = m.SizeUSD
		distance = m.Distance / 100
		sweepProb = m.Probability
		if m.Probability >= 70 {
			sev = "HIGH"
		}
	}

	gravityPct := math.Max(sigs.LiquidityGravity.UpwardPull, sigs.LiquidityGravity.DownwardPull)

	return cards.AlertCardData{
		Symbol:       snap.Symbol,
		Severity:     sev,
		AlertType:    "Liquidity Watch",
		Price:        snap.LiquidationMap.CurrentPrice,
		ClusterPrice: clusterPrice,
		ClusterSize:  clusterSize,
		Distance:     distance,
		SweepProb:    sweepProb,
		CascadeLevel: sigs.CascadeRisk.Level,
		CascadeScore: sigs.CascadeRisk.Score,
		GravityDir:   sigs.LiquidityGravity.Dominant,
		GravityPct:   gravityPct,
		Funding:      snap.FundingRate.Rate,
		OIChange:     snap.OpenInterest.OIChange1h,
	}
}

