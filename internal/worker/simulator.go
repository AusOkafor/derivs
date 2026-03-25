package worker

// simulator.go — scenario capture pipeline for Signal Trainer
//
// Flow:
//   1. When a playbook candidate fires, captureSimulatorScenario() saves a
//      snapshot to the simulator_scenarios table in Supabase.
//   2. resolveSimulatorOutcomes() runs every hour, fetches scenarios that are
//      1–6h old with no outcome, checks current price, and fills in the outcome.
//
// Outcome logic:
//   move_pct = (current_price - entry_price) / entry_price * 100
//   >= +1.0%  → outcome = "long"  (price went up)
//   <= -1.0%  → outcome = "short" (price went down)
//   otherwise → outcome = "skip"  (range-bound — patience was correct)

import (
	"context"
	"fmt"
	"log"
	"math"
	"sync"
	"time"

	"derivs-backend/internal/models"
	"derivs-backend/internal/supabase"
)

const (
	simCaptureInterval = 2 * time.Hour  // min gap between captures per symbol
	simOutcomeMinAge   = 1 * time.Hour  // resolve after at least 1h
	simOutcomeMaxAge   = 6 * time.Hour  // give up resolving after 6h
	simOutcomeWinPct   = 1.0            // ±1% move = directional outcome
)

// simCaptureTracker rate-limits captures per symbol.
type simCaptureTracker struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func newSimCaptureTracker() *simCaptureTracker {
	return &simCaptureTracker{last: make(map[string]time.Time)}
}

func (t *simCaptureTracker) allow(symbol string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if last, ok := t.last[symbol]; ok && time.Since(last) < simCaptureInterval {
		return false
	}
	t.last[symbol] = time.Now()
	return true
}

// captureSimulatorScenario saves a resolved playbook signal as a training scenario.
// Called asynchronously after a confirmed candidate fires.
func (w *Worker) captureSimulatorScenario(ctx context.Context, c *playbookCandidate, snap *models.MarketSnapshot, sigs *models.MarketSignals) {
	if snap == nil || sigs == nil {
		return
	}
	if !w.simCapture.allow(c.symbol) {
		log.Printf("[sim] %s capture rate-limited — skipping", c.symbol)
		return
	}

	// Build the row
	row := supabase.SimulatorScenarioRow{
		Symbol:     c.symbol,
		CapturedAt: time.Now(),
		Price:      snap.LiquidationMap.CurrentPrice,
		Funding:    snap.FundingRate.Rate,
		OIChange1h: snap.OpenInterest.OIChange1h,
		Regime:     string(sigs.Regime),
		OITrend:    string(sigs.OITrend),
		Difficulty: simDifficulty(c),
		SetupType:  simSetupType(c, sigs),
	}

	lpi := sigs.LiquidityPressure
	row.LPIScore = lpi.Score
	row.Bias = lpi.Label

	if m := sigs.LiquidationMagnet; m != nil {
		side := m.Side
		price := m.Price
		size := m.SizeUSD
		dist := m.Distance
		row.ClusterSide  = &side
		row.ClusterPrice = &price
		row.ClusterSize  = &size
		row.ClusterDist  = &dist
	}

	row.KeySignal = buildSimKeySignal(c, sigs)

	if err := w.db.SaveSimulatorScenario(ctx, row); err != nil {
		log.Printf("[sim] save scenario %s: %v", c.symbol, err)
		return
	}
	log.Printf("[sim] captured scenario %s stage=%s setup=%s difficulty=%s",
		c.symbol, c.stage, row.SetupType, row.Difficulty)
}

// resolveSimulatorOutcomes fetches unresolved scenarios and fills in outcomes.
// Should be called once per hour from the worker loop.
func (w *Worker) resolveSimulatorOutcomes(ctx context.Context) {
	rows, err := w.db.GetUnresolvedScenarios(ctx, simOutcomeMinAge, simOutcomeMaxAge)
	if err != nil {
		log.Printf("[sim] GetUnresolvedScenarios: %v", err)
		return
	}
	if len(rows) == 0 {
		return
	}
	log.Printf("[sim] resolving %d scenario(s)", len(rows))

	for _, row := range rows {
		// Fetch current price
		sa, err := w.aggregator.FetchSnapshot(ctx, row.Symbol)
		if err != nil {
			log.Printf("[sim] FetchSnapshot(%s): %v", row.Symbol, err)
			continue
		}
		currentPrice := sa.LiquidationMap.CurrentPrice
		if currentPrice == 0 || row.Price == 0 {
			continue
		}

		movePct := (currentPrice - row.Price) / row.Price * 100

		outcome := "skip"
		if movePct >= simOutcomeWinPct {
			outcome = "long"
		} else if movePct <= -simOutcomeWinPct {
			outcome = "short"
		}

		if err := w.db.ResolveSimulatorScenario(ctx, row.ID, outcome, currentPrice, movePct); err != nil {
			log.Printf("[sim] resolve %s: %v", row.ID, err)
			continue
		}
		log.Printf("[sim] resolved %s %s move=%.2f%% → %s", row.Symbol, row.ID[:8], movePct, outcome)
	}
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func simSetupType(c *playbookCandidate, sigs *models.MarketSignals) string {
	regime := string(sigs.Regime)
	oiChange := string(sigs.OITrend)

	// OI divergence: price at high but OI declining
	if containsAny(oiChange, "diverg", "declin") {
		return "divergence"
	}
	// Cascade regime with heavy positioning
	if regime == "Cascade" {
		return "cascade"
	}
	// Trending with close cluster = sweep
	if regime == "Trending" && sigs.LiquidationMagnet != nil && sigs.LiquidationMagnet.Distance < 0.2 {
		return "sweep"
	}
	// Neutral or no clear cluster
	if regime == "Neutral" {
		return "neutral"
	}
	return "sweep"
}

func simDifficulty(c *playbookCandidate) string {
	switch {
	case c.score >= 80:
		return "beginner"
	case c.score >= 60:
		return "intermediate"
	default:
		return "advanced"
	}
}

func buildSimKeySignal(c *playbookCandidate, sigs *models.MarketSignals) string {
	m := sigs.LiquidationMagnet
	if m == nil {
		return fmt.Sprintf("%s regime. OI: %s", sigs.Regime, sigs.OITrend)
	}
	sideLabel := "Short"
	if m.Side == "long" {
		sideLabel = "Long"
	}
	distStr := fmt.Sprintf("%.2f%%", m.Distance)
	sizeStr := ""
	if m.SizeUSD >= 1_000_000 {
		sizeStr = fmt.Sprintf("$%.1fM", m.SizeUSD/1_000_000)
	} else {
		sizeStr = fmt.Sprintf("$%.0fK", m.SizeUSD/1000)
	}

	lpiDesc := ""
	if lpi := sigs.LiquidityPressure; math.Abs(float64(lpi.Score)) > 10 {
		lpiDesc = fmt.Sprintf(" LPI: %+d (%s).", lpi.Score, lpi.Label)
	}

	return fmt.Sprintf("%s cluster %s away (%s). %s regime. OI: %s.%s Stage: %s (score %d).",
		sideLabel, distStr, sizeStr, sigs.Regime, sigs.OITrend, lpiDesc, c.stage, c.score)
}

func containsAny(s string, subs ...string) bool {
	sl := toLower(s)
	for _, sub := range subs {
		if len(sl) >= len(sub) {
			for i := 0; i <= len(sl)-len(sub); i++ {
				if sl[i:i+len(sub)] == sub {
					return true
				}
			}
		}
	}
	return false
}

func toLower(s string) string {
	b := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 32
		}
		b[i] = c
	}
	return string(b)
}
