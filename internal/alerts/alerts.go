package alerts

import (
	"fmt"
	"math"
	"time"

	"derivs-backend/internal/models"
)

type Detector struct{}

func New() *Detector { return &Detector{} }

// Analyze runs all detection rules against the snapshot and returns any
// triggered alerts. Returns an empty (non-nil) slice if nothing fires.
func (d *Detector) Analyze(snap models.MarketSnapshot) []models.Alert {
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
	if rate > 0.0005 || rate < -0.0005 {
		side := "long"
		if rate < 0 {
			side = "short"
		}
		add("funding-elevated",
			fmt.Sprintf("Funding rate at %.4f%% — elevated %s squeeze risk", rate*100, side),
			"high",
		)
	}

	// ── Rule 2: OI spike (1h) ─────────────────────────────────────────────────
	oi1h := snap.OpenInterest.OIChange1h
	if math.Abs(oi1h) > 2.0 {
		direction := "accumulation"
		if oi1h < 0 {
			direction = "distribution"
		}
		add("oi-spike-1h",
			fmt.Sprintf("OI spike %+.2f%% in 1h without price confirmation — potential %s", oi1h, direction),
			"high",
		)
	}

	// ── Rule 3: OI divergence (24h) ───────────────────────────────────────────
	oi24h := snap.OpenInterest.OIChange24h
	if math.Abs(oi24h) > 5.0 {
		upDown := "up"
		action := "building"
		if oi24h < 0 {
			upDown = "down"
			action = "unwinding"
		}
		add("oi-divergence-24h",
			fmt.Sprintf("Open interest %s %.2f%% in 24h — %s leverage detected", upDown, math.Abs(oi24h), action),
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

		switch {
		case avgLong > 65.0:
			add("long-bias",
				fmt.Sprintf("Long bias at %.1f%% across exchanges — crowded long, liquidation risk below current price", avgLong),
				"medium",
			)
		case avgLong < 35.0:
			add("short-bias",
				fmt.Sprintf("Short bias at %.1f%% across exchanges — crowded short, squeeze risk above current price", avgLong),
				"medium",
			)
		}
	}

	// ── Rule 6: Large liquidation cluster ────────────────────────────────────
	for _, lvl := range snap.LiquidationMap.Levels {
		if lvl.SizeUsd > 500_000 {
			add(fmt.Sprintf("liq-cluster-%.0f", lvl.Price),
				fmt.Sprintf("Large %s liquidation cluster at $%.0f worth $%.2fM",
					lvl.Side, lvl.Price, lvl.SizeUsd/1_000_000),
				"high",
			)
		}
	}

	// ── Rule 7: Negative funding (low) ────────────────────────────────────────
	// Only fires if Rule 1 didn't already fire (avoid double-alerting).
	if rate < -0.0001 && rate >= -0.0005 {
		add("funding-negative",
			fmt.Sprintf("Funding rate negative at %.4f%% — shorts paying longs, potential upward pressure", rate*100),
			"low",
		)
	}

	if out == nil {
		return []models.Alert{}
	}
	return out
}
