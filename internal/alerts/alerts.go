package alerts

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"derivs-backend/internal/models"
)

// OnHighAlert is called when a HIGH severity alert fires. Set via SetOnHighAlert.
var OnHighAlert func(alert models.Alert, snap models.MarketSnapshot, sigs models.MarketSignals)

// SetOnHighAlert sets the callback invoked when a HIGH severity alert fires.
func SetOnHighAlert(fn func(alert models.Alert, snap models.MarketSnapshot, sigs models.MarketSignals)) {
	OnHighAlert = fn
}

type Detector struct{}

// LiquidationZone holds aggregated liquidation levels within 0.5% of each other.
type LiquidationZone struct {
	MinPrice   float64
	MaxPrice   float64
	TotalUSD   float64
	Side       string // "long", "short", or "mixed"
	LevelCount int
	HasWhale   bool // any single level > $500k
}

func aggregateLiquidationZones(levels []models.LiquidationLevel, currentPrice float64) []LiquidationZone {
	if len(levels) == 0 || currentPrice == 0 {
		return nil
	}

	sorted := make([]models.LiquidationLevel, len(levels))
	copy(sorted, levels)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Price < sorted[j].Price
	})

	var zones []LiquidationZone
	var current *LiquidationZone

	for _, lvl := range sorted {
		if lvl.SizeUsd < 10_000 {
			continue
		}
		if current == nil {
			current = &LiquidationZone{
				MinPrice:   lvl.Price,
				MaxPrice:   lvl.Price,
				TotalUSD:   lvl.SizeUsd,
				Side:       lvl.Side,
				LevelCount: 1,
				HasWhale:   lvl.SizeUsd >= 500_000,
			}
			continue
		}
		distFromZone := math.Abs(lvl.Price-current.MaxPrice) / currentPrice * 100
		if distFromZone <= 0.5 {
			current.MaxPrice = math.Max(current.MaxPrice, lvl.Price)
			current.MinPrice = math.Min(current.MinPrice, lvl.Price)
			current.TotalUSD += lvl.SizeUsd
			current.LevelCount++
			if lvl.Side != current.Side {
				current.Side = "mixed"
			}
			if lvl.SizeUsd >= 500_000 {
				current.HasWhale = true
			}
		} else {
			zones = append(zones, *current)
			current = &LiquidationZone{
				MinPrice:   lvl.Price,
				MaxPrice:   lvl.Price,
				TotalUSD:   lvl.SizeUsd,
				Side:       lvl.Side,
				LevelCount: 1,
				HasWhale:   lvl.SizeUsd >= 500_000,
			}
		}
	}
	if current != nil {
		zones = append(zones, *current)
	}
	return zones
}

const (
	MinClusterSize      = 200_000  // $200k minimum for ANY liquidation alert (zone, magnet, heat feed)
	highSeverityMinSize = 500_000  // $500k minimum for HIGH
	medSeverityMinSize  = 100_000  // $100k minimum for MEDIUM (must be >= minClusterSize to fire)
)

func zoneSeverity(zone LiquidationZone, distance float64) string {
	if zone.TotalUSD < MinClusterSize {
		return ""
	}
	if zone.TotalUSD >= highSeverityMinSize && distance <= 1.5 {
		return "high"
	}
	if zone.HasWhale && zone.TotalUSD >= highSeverityMinSize && distance <= 1.0 {
		return "high"
	}
	if zone.TotalUSD >= medSeverityMinSize && distance <= 3.0 {
		return "medium"
	}
	return ""
}

func formatUSD(usd float64) string {
	if usd >= 1_000_000 {
		return fmt.Sprintf("$%.2fM", usd/1_000_000)
	}
	if usd >= 1_000 {
		return fmt.Sprintf("$%.0fk", usd/1_000)
	}
	return fmt.Sprintf("$%.0f", usd)
}

func formatPrice(p float64) string {
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

func New() *Detector { return &Detector{} }

// Analyze runs all detection rules against the snapshot and signals, and returns raw
// triggered alerts. Returns an empty (non-nil) slice if nothing fires.
// Filtering (cooldown, size/distance) is done by Engine.Process().
func (d *Detector) Analyze(snap models.MarketSnapshot, sigs models.MarketSignals) []models.Alert {
	var out []models.Alert
	now := time.Now().UTC()

	symbol := snap.Symbol
	add := func(id, msg, severity string) {
		a := models.Alert{
			ID:        fmt.Sprintf("%s-%s", snap.Symbol, id),
			Symbol:    snap.Symbol,
			Message:   msg,
			Severity:  severity,
			Timestamp: now,
		}
		out = append(out, a)
		if severity == "high" && OnHighAlert != nil {
			OnHighAlert(a, snap, sigs)
		}
	}

	// ── Rule 0: Liquidation burst (real-time from Binance) ─────────────────────
	if snap.RecentLiquidations != nil && snap.RecentLiquidations.BurstDetected {
		add("liq-burst",
			fmt.Sprintf("🚨 Liquidation burst detected — $%.1fM liquidated in 30 seconds\nCascade may be underway",
				snap.RecentLiquidations.BurstSizeUSD/1_000_000),
			"high",
		)
	}

	// ── Rule 1: Elevated funding rate ────────────────────────────────────────
	rate := snap.FundingRate.Rate
	if rate > 0.0005 {
		add("funding-elevated",
			fmt.Sprintf("Funding rate spiking at +%.4f%% (APR %.1f%%) — longs paying heavily, overleveraged market. Watch for long squeeze if price stalls.",
				rate*100, rate*100*3*365),
			"high",
		)
	} else if rate < -0.0005 {
		add("funding-elevated",
			fmt.Sprintf("Funding rate negative at %.4f%% — shorts paying longs, potential upward pressure. Watch for short squeeze rally.",
				rate*100),
			"high",
		)
	}

	// ── Rule 2: OI spike (1h) ─────────────────────────────────────────────────
	oi1h := snap.OpenInterest.OIChange1h
	if oi1h > 2.0 {
		add("oi-spike-1h",
			fmt.Sprintf("OI up %.1f%% in 1h — new money entering fast. Watch for volatile directional move.", oi1h),
			"high",
		)
	} else if oi1h < -2.0 {
		add("oi-spike-1h",
			fmt.Sprintf("OI down %.1f%% in 1h — rapid deleveraging detected. Liquidation cascade risk.", math.Abs(oi1h)),
			"high",
		)
	}

	// ── Rule 3: OI divergence (24h) ───────────────────────────────────────────
	oi24h := snap.OpenInterest.OIChange24h
	if oi24h > 5.0 {
		add("oi-divergence-24h",
			fmt.Sprintf("OI up %.1f%% in 24h — new money entering fast. Watch for volatile directional move as positions build.",
				oi24h),
			"medium",
		)
	} else if oi24h < -5.0 {
		add("oi-divergence-24h",
			fmt.Sprintf("Open interest down %.1f%% in 24h — leverage unwinding detected. Market deleveraging, expect lower volatility.",
				math.Abs(oi24h)),
			"medium",
		)
	}

	// ── Rules 4 & 5: Long/short bias ─────────────────────────────────────────
	if len(snap.LongShortRatios) > 0 {
		var sumLong float64
		for _, r := range snap.LongShortRatios {
			sumLong += r.LongPct
		}
		avgLong := sumLong / float64(len(snap.LongShortRatios))
		shortPct := 100.0 - avgLong

		switch {
		case avgLong > 72.0:
			add("long-bias",
				fmt.Sprintf("%.1f%% of traders are long across exchanges — crowded trade. High liquidation risk below current price if bulls lose control.",
					avgLong),
				"medium",
			)
		case avgLong < 28.0:
			add("short-bias",
				fmt.Sprintf("%.1f%% of traders are short — crowded short. Watch for short squeeze, especially on any positive catalyst.",
					shortPct),
				"medium",
			)
		}
	}

	// ── Zone-based liquidation alerts ────────────────────────────────────────
	zones := aggregateLiquidationZones(snap.LiquidationMap.Levels, snap.LiquidationMap.CurrentPrice)
	for _, zone := range zones {
		var distanceToZone float64
		if snap.LiquidationMap.CurrentPrice < zone.MinPrice {
			distanceToZone = (zone.MinPrice - snap.LiquidationMap.CurrentPrice) / snap.LiquidationMap.CurrentPrice * 100
		} else if snap.LiquidationMap.CurrentPrice > zone.MaxPrice {
			distanceToZone = (snap.LiquidationMap.CurrentPrice - zone.MaxPrice) / snap.LiquidationMap.CurrentPrice * 100
		} else {
			distanceToZone = 0
		}

		severity := zoneSeverity(zone, distanceToZone)
		if severity == "" {
			continue
		}

		midPrice := (zone.MinPrice + zone.MaxPrice) / 2
		zoneID := fmt.Sprintf("%s-zone-%.0f", symbol, math.Round(midPrice/5)*5)

		sizeStr := fmt.Sprintf("$%.2fM", zone.TotalUSD/1_000_000)
		if zone.TotalUSD < 1_000_000 {
			sizeStr = fmt.Sprintf("$%.0fk", zone.TotalUSD/1_000)
		}

		priceRange := formatPrice(zone.MinPrice)
		if zone.MaxPrice != zone.MinPrice {
			priceRange = fmt.Sprintf("%s – %s", formatPrice(zone.MinPrice), formatPrice(zone.MaxPrice))
		}

		var directionMsg string
		switch zone.Side {
		case "long":
			directionMsg = "Long liquidations — if swept, expect accelerated downward move"
		case "short":
			directionMsg = "Short liquidations — if swept, expect accelerated upward move (short squeeze)"
		case "mixed":
			directionMsg = "Mixed liquidations — both sides trapped in this zone"
		default:
			directionMsg = ""
		}

		whaleTag := ""
		if zone.HasWhale {
			whaleTag = "\n• 🐋 Whale cluster detected"
		}

		distStr := fmt.Sprintf("%.2f%%", distanceToZone)
		if distanceToZone < 0.1 {
			distStr = "at current price"
		}

		sideTitle := zone.Side
		if len(zone.Side) > 0 {
			sideTitle = strings.ToUpper(zone.Side[:1]) + zone.Side[1:]
		}

		message := fmt.Sprintf(
			"%s liquidation zone\n%s | %s | %d levels\nDistance: %s\n\n%s%s",
			sideTitle,
			priceRange,
			sizeStr,
			zone.LevelCount,
			distStr,
			directionMsg,
			whaleTag,
		)

		sweepProb := 0
		if sigs.LiquidationMagnet != nil {
			sweepProb = sigs.LiquidationMagnet.Probability
		}
		a := models.Alert{
			ID:           zoneID,
			Symbol:       symbol,
			Message:      message,
			Severity:     severity,
			Timestamp:    now,
			ClusterPrice: (zone.MinPrice + zone.MaxPrice) / 2,
			ClusterSize:  zone.TotalUSD,
			Distance:     distanceToZone / 100,
			Probability:  sweepProb,
		}
		out = append(out, a)
		if severity == "high" && OnHighAlert != nil {
			OnHighAlert(a, snap, sigs)
		}
	}

	// ── Rule 7: Negative funding (low) ────────────────────────────────────────
	if rate < -0.0001 && rate >= -0.0005 {
		add("funding-negative",
			fmt.Sprintf("Funding rate negative at %.4f%% — shorts paying longs, potential upward pressure. Watch for short squeeze rally.",
				rate*100),
			"low",
		)
	}

	// ── Rule 9: Short squeeze probability high ─────────────────────────────────
	if sigs.ShortSqueezeProbability >= 65 {
		id := fmt.Sprintf("short-squeeze-%d", sigs.ShortSqueezeProbability/10*10)
		a := models.Alert{
			ID:        fmt.Sprintf("%s-%s", snap.Symbol, id),
			Symbol:    snap.Symbol,
			Message:   fmt.Sprintf("Short squeeze probability at %d%% — negative funding, shorts overcrowded, liquidation clusters above price. Watch for rapid upward move.", sigs.ShortSqueezeProbability),
			Severity:  "high",
			Timestamp: now,
		}
		out = append(out, a)
		if OnHighAlert != nil {
			OnHighAlert(a, snap, sigs)
		}
	}

	// ── Rule 10: Long squeeze probability high ──────────────────────────────────
	if sigs.LongSqueezeProbability >= 65 {
		id := fmt.Sprintf("long-squeeze-%d", sigs.LongSqueezeProbability/10*10)
		a := models.Alert{
			ID:        fmt.Sprintf("%s-%s", snap.Symbol, id),
			Symbol:    snap.Symbol,
			Message:   fmt.Sprintf("Long squeeze probability at %d%% — elevated funding, longs overcrowded, liquidation clusters below price. Watch for rapid downward move.", sigs.LongSqueezeProbability),
			Severity:  "high",
			Timestamp: now,
		}
		out = append(out, a)
		if OnHighAlert != nil {
			OnHighAlert(a, snap, sigs)
		}
	}

	// ── Rule 11: Liquidation magnet nearby ──────────────────────────────────────
	if sigs.LiquidationMagnet != nil && sigs.LiquidationMagnet.Probability >= 65 {
		m := sigs.LiquidationMagnet
		if m.SizeUSD < MinClusterSize || m.Distance < 0.1 {
			// Skip — cluster below $200k or distance below 0.1%
		} else {
			magnetRound := 10.0
			if m.Price < 100 {
				magnetRound = 1.0
			} else if m.Price < 1000 {
				magnetRound = 5.0
			}
			roundedMagnetPrice := math.Round(m.Price/magnetRound) * magnetRound
			id := fmt.Sprintf("liq-magnet-%.0f", roundedMagnetPrice)

			severity := "high"
			if m.SizeUSD < 500_000 {
				severity = "medium"
			}
			var message string
			if m.Side == "short" {
				message = fmt.Sprintf("Short liquidity above price — upward sweep likely\n\nCluster: %s | Distance: %.2f%% | Probability: %d%%",
					formatUSD(m.SizeUSD), m.Distance, m.Probability)
			} else {
				message = fmt.Sprintf("Long liquidity below price — downward sweep likely\n\nCluster: %s | Distance: %.2f%% | Probability: %d%%",
					formatUSD(m.SizeUSD), m.Distance, m.Probability)
			}
			a := models.Alert{
				ID:           fmt.Sprintf("%s-%s", snap.Symbol, id),
				Symbol:       snap.Symbol,
				Message:      message,
				Severity:     severity,
				Timestamp:    now,
				ClusterPrice: m.Price,
				ClusterSize:  m.SizeUSD,
				Distance:     m.Distance / 100,
				Probability:  m.Probability,
			}
			out = append(out, a)
			if severity == "high" && OnHighAlert != nil {
				OnHighAlert(a, snap, sigs)
			}
		}
	}

	// ── Rule 12: Market regime change to Liquidation Event ──────────────────────
	if sigs.Regime == models.RegimeLiquidation {
		a := models.Alert{
			ID:        fmt.Sprintf("%s-regime-liquidation", snap.Symbol),
			Symbol:    snap.Symbol,
			Message:   "Market regime: Liquidation Event detected. OI dropping sharply — forced position closures underway. Potential local top/bottom forming.",
			Severity:  "high",
			Timestamp: now,
		}
		out = append(out, a)
		if OnHighAlert != nil {
			OnHighAlert(a, snap, sigs)
		}
	}

	// If a liq-magnet alert exists for this symbol, remove ALL zone alerts
	hasMagnetAlert := false
	for _, a := range out {
		if strings.HasPrefix(a.ID, symbol+"-liq-magnet-") {
			hasMagnetAlert = true
			break
		}
	}
	if hasMagnetAlert {
		filtered := out[:0]
		for _, a := range out {
			if !strings.HasPrefix(a.ID, symbol+"-zone-") {
				filtered = append(filtered, a)
			}
		}
		out = filtered
	}

	if out == nil {
		return []models.Alert{}
	}
	return out
}
