package alerts

import (
	"fmt"
	"math"
	"sort"
	"time"

	"derivs-backend/internal/models"
)

type Detector struct{}

// priceBucket holds aggregated liquidation levels in a price range.
type priceBucket struct {
	side     string
	totalUSD float64
	minPrice float64
	maxPrice float64
	count    int
}

// groupIntoBuckets groups levels by floor(price/bucketSize)*bucketSize and side.
// Returns buckets sorted by totalUSD descending.
// Uses dynamic bucket size for low-priced assets (e.g. SOL) so minPrice is never 0.
func groupIntoBuckets(levels []models.LiquidationLevel, bucketSize float64) []priceBucket {
	if len(levels) == 0 {
		return nil
	}
	var maxPrice float64
	for _, l := range levels {
		if l.Price > maxPrice {
			maxPrice = l.Price
		}
	}
	if maxPrice < 1000 && bucketSize > maxPrice/5 {
		bucketSize = math.Max(10, maxPrice/20)
	}
	type bucketKey struct {
		side  string
		start float64
	}
	m := make(map[bucketKey]*priceBucket)
	for _, lvl := range levels {
		bucketFloor := math.Floor(lvl.Price/bucketSize) * bucketSize
		k := bucketKey{side: lvl.Side, start: bucketFloor}
		if m[k] == nil {
			m[k] = &priceBucket{
				side:     lvl.Side,
				minPrice: bucketFloor,
				maxPrice: bucketFloor + bucketSize,
			}
		}
		m[k].totalUSD += lvl.SizeUsd
		m[k].count++
	}
	var buckets []priceBucket
	for _, b := range m {
		buckets = append(buckets, *b)
	}
	sort.Slice(buckets, func(i, j int) bool { return buckets[i].totalUSD > buckets[j].totalUSD })
	return buckets
}

func New() *Detector { return &Detector{} }

// Analyze runs all detection rules against the snapshot and signals, and returns any
// triggered alerts. Returns an empty (non-nil) slice if nothing fires.
func (d *Detector) Analyze(snap models.MarketSnapshot, sigs models.MarketSignals) []models.Alert {
	var out []models.Alert
	now := time.Now().UTC()

	add := func(id, msg, severity string) {
		out = append(out, models.Alert{
			ID:        fmt.Sprintf("%s-%s", snap.Symbol, id),
			Symbol:    snap.Symbol,
			Message:   msg,
			Severity:  severity,
			Timestamp: now,
		})
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
	if math.Abs(oi1h) > 2.0 {
		change := oi1h
		add("oi-spike-1h",
			fmt.Sprintf("OI up %.1f%% in 1h — new money entering fast. Watch for volatile directional move as positions build.",
				change),
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

	// ── Rule 6: Large liquidation cluster ────────────────────────────────────
	for _, lvl := range snap.LiquidationMap.Levels {
		if lvl.SizeUsd > 500_000 {
			roundedPrice := math.Round(lvl.Price/100) * 100
			id := fmt.Sprintf("liq-cluster-%d", int(roundedPrice))
			direction := "downward"
			if lvl.Side == "short" {
				direction = "upward"
			}
			add(id,
				fmt.Sprintf("Large %s liquidation cluster at $%.0f worth $%.2fM — if price reaches this level, expect accelerated %s move.",
					lvl.Side, lvl.Price, lvl.SizeUsd/1_000_000, direction),
				"high",
			)
		}
	}

	// ── Rule 7: Negative funding (low) ────────────────────────────────────────
	// Only fires if Rule 1 didn't already fire (avoid double-alerting).
	if rate < -0.0001 && rate >= -0.0005 {
		add("funding-negative",
			fmt.Sprintf("Funding rate negative at %.4f%% — shorts paying longs, potential upward pressure. Watch for short squeeze rally.",
				rate*100),
			"low",
		)
	}

	// ── Rule 8: Whale cluster detected ─────────────────────────────────────────
	buckets := groupIntoBuckets(snap.LiquidationMap.Levels, 500.0)
	for _, b := range buckets {
		if b.totalUSD >= 2_000_000 {
			severity := "medium"
			if b.totalUSD >= 5_000_000 {
				severity = "high"
			}
			direction := "downward"
			if b.side == "short" {
				direction = "upward"
			}
			id := fmt.Sprintf("whale-%s-%.0f", b.side, b.minPrice)
			msg := fmt.Sprintf("🐋 Large %s whale cluster near $%.0f: $%.2fM across %d levels — significant %s pressure concentration.",
				b.side, b.minPrice, b.totalUSD/1_000_000, b.count, direction)
			out = append(out, models.Alert{
				ID:        fmt.Sprintf("%s-%s", snap.Symbol, id),
				Symbol:    snap.Symbol,
				Message:   msg,
				Severity:  severity,
				Timestamp: now,
			})
		}
	}

	// ── Rule 9: Short squeeze probability high ─────────────────────────────────
	if sigs.ShortSqueezeProbability >= 65 {
		id := fmt.Sprintf("short-squeeze-%d", sigs.ShortSqueezeProbability/10*10)
		out = append(out, models.Alert{
			ID:        fmt.Sprintf("%s-%s", snap.Symbol, id),
			Symbol:    snap.Symbol,
			Message:   fmt.Sprintf("Short squeeze probability at %d%% — negative funding, shorts overcrowded, liquidation clusters above price. Watch for rapid upward move.", sigs.ShortSqueezeProbability),
			Severity:  "high",
			Timestamp: now,
		})
	}

	// ── Rule 10: Long squeeze probability high ──────────────────────────────────
	if sigs.LongSqueezeProbability >= 65 {
		id := fmt.Sprintf("long-squeeze-%d", sigs.LongSqueezeProbability/10*10)
		out = append(out, models.Alert{
			ID:        fmt.Sprintf("%s-%s", snap.Symbol, id),
			Symbol:    snap.Symbol,
			Message:   fmt.Sprintf("Long squeeze probability at %d%% — elevated funding, longs overcrowded, liquidation clusters below price. Watch for rapid downward move.", sigs.LongSqueezeProbability),
			Severity:  "high",
			Timestamp: now,
		})
	}

	// ── Rule 11: Liquidation magnet nearby ──────────────────────────────────────
	if sigs.LiquidationMagnet != nil && sigs.LiquidationMagnet.Probability >= 65 {
		m := sigs.LiquidationMagnet
		fundingCtx := "neutral funding"
		if snap.FundingRate.Rate < -0.0001 {
			fundingCtx = "negative funding — shorts vulnerable"
		} else if snap.FundingRate.Rate > 0.0003 {
			fundingCtx = "elevated funding — longs vulnerable"
		}
		oiCtx := fmt.Sprintf("OI %.1f%% in 24h", snap.OpenInterest.OIChange24h)
		id := fmt.Sprintf("liq-magnet-%.0f", m.Price)
		out = append(out, models.Alert{
			ID:       fmt.Sprintf("%s-%s", snap.Symbol, id),
			Symbol:   snap.Symbol,
			Message:  fmt.Sprintf("Liquidity sweep alert: large %s cluster at $%.0f (%.2f%% away)\n\nMarket context:\n• %s\n• %s\n• %s\n\nSweep probability: %d%%", m.Side, m.Price, m.Distance, fundingCtx, oiCtx, sigs.LeverageImbalance, m.Probability),
			Severity: "high",
			Timestamp: now,
		})
	}

	// ── Rule 12: Market regime change to Liquidation Event ──────────────────────
	if sigs.Regime == models.RegimeLiquidation {
		out = append(out, models.Alert{
			ID:        fmt.Sprintf("%s-regime-liquidation", snap.Symbol),
			Symbol:    snap.Symbol,
			Message:   "Market regime: Liquidation Event detected. OI dropping sharply — forced position closures underway. Potential local top/bottom forming.",
			Severity:  "high",
			Timestamp: now,
		})
	}

	if out == nil {
		return []models.Alert{}
	}
	return out
}
