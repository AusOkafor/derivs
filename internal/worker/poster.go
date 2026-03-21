package worker

import (
	"context"
	"fmt"
	"log"
	"math"
	"os"
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

	// Try to send as image card + caption; fall back to text-only
	if sent := w.trySendPostWithCard(sa.snap, sa.sigs, post); !sent {
		msg := fmt.Sprintf("📢 <b>SUGGESTED X POST</b>\n\n<code>%s</code>", post)
		if err := w.notifier.SendToAdmin(msg); err != nil {
			log.Printf("poster: failed to send post to admin: %v", err)
			return
		}
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

	// Direction
	biasEmoji := "⚪"
	biasLabel := "NEUTRAL"
	lpi := sigs.LiquidityPressure.Score
	if lpi >= 20 {
		biasEmoji = "🟢"
		biasLabel = "BULLISH"
	} else if lpi <= -20 {
		biasEmoji = "🔴"
		biasLabel = "BEARISH"
	}

	// Magnet line
	magnetLine := ""
	timingLabel := ""
	if sigs.LiquidationMagnet != nil {
		m := sigs.LiquidationMagnet
		sideLabel := strings.ToUpper(m.Side)
		clusterStr := formatClusterSize(m.SizeUSD)
		priceStr := formatPrice(m.Price)
		magnetLine = fmt.Sprintf("%s %s cluster at %s — %d%% sweep probability",
			clusterStr, sideLabel, priceStr, m.Probability)

		if m.Distance < 0.3 {
			timingLabel = "⚡ IMMINENT"
		} else if m.Distance < 0.8 {
			timingLabel = "⏳ FORMING"
		} else {
			timingLabel = "👀 WATCH"
		}
	}

	// Cascade
	cascadeLine := fmt.Sprintf("Cascade risk: %s (%d/100)", sigs.CascadeRisk.Level, sigs.CascadeRisk.Score)

	// Long bias
	avgLong := avgLongPct(snap)
	longLine := fmt.Sprintf("%.0f%% longs across exchanges", avgLong)

	// Funding
	fundingPct := snap.FundingRate.Rate * 100
	fundingSign := "+"
	if fundingPct < 0 {
		fundingSign = ""
	}
	fundingLine := fmt.Sprintf("Funding: %s%.4f%%", fundingSign, fundingPct)

	// Build post
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("📊 %s — SIGNAL DETECTED\n\n", symbol))
	sb.WriteString(fmt.Sprintf("%s %s", biasEmoji, biasLabel))
	if timingLabel != "" {
		sb.WriteString(fmt.Sprintf(" — %s", timingLabel))
	}
	sb.WriteString("\n")
	if magnetLine != "" {
		sb.WriteString(magnetLine + "\n")
	}
	sb.WriteString(cascadeLine + "\n")
	sb.WriteString(longLine + "\n")
	sb.WriteString(fundingLine + "\n")
	sb.WriteString("\nTrack live → derivlens.io")

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

// trySendPostWithCard generates a branded card image and sends it to the admin
// as a photo with the tweet text as caption. Returns true on success.
func (w *Worker) trySendPostWithCard(snap models.MarketSnapshot, sigs models.MarketSignals, post string) bool {
	adminChatID := os.Getenv("ADMIN_TELEGRAM_CHAT_ID")
	if adminChatID == "" {
		return false
	}
	if sigs.LiquidationMagnet == nil {
		return false
	}

	m := sigs.LiquidationMagnet
	severity := "MEDIUM"
	if m.Probability >= 80 || sigs.CascadeRisk.Level == "HIGH" || sigs.CascadeRisk.Level == "CRITICAL" {
		severity = "HIGH"
	}

	gravityPct := math.Max(sigs.LiquidityGravity.UpwardPull, sigs.LiquidityGravity.DownwardPull)

	data := cards.AlertCardData{
		Symbol:       snap.Symbol,
		Severity:     severity,
		AlertType:    "Liquidity Sweep",
		Price:        snap.LiquidationMap.CurrentPrice,
		ClusterPrice: m.Price,
		ClusterSize:  m.SizeUSD,
		Distance:     m.Distance / 100,
		SweepProb:    m.Probability,
		CascadeLevel: sigs.CascadeRisk.Level,
		CascadeScore: sigs.CascadeRisk.Score,
		GravityDir:   sigs.LiquidityGravity.Dominant,
		GravityPct:   gravityPct,
		Funding:      snap.FundingRate.Rate,
		OIChange:     snap.OpenInterest.OIChange1h,
	}

	imgBytes, err := cards.GenerateAlertCard(data)
	if err != nil {
		log.Printf("poster: card generation failed: %v", err)
		return false
	}

	caption := fmt.Sprintf("📢 SUGGESTED X POST\n\n%s", post)
	if err := w.notifier.SendPhoto(adminChatID, imgBytes, caption); err != nil {
		log.Printf("poster: SendPhoto to admin failed: %v", err)
		return false
	}
	return true
}
