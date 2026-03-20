package worker

import (
	"context"
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"derivs-backend/internal/models"
	"derivs-backend/internal/signals"
)

const briefSymbols = "BTC, ETH, SOL, ARB, DOGE, AVAX"
const upgradeCTA = "\n\n<i>Upgrade to Pro for all 16 symbols → derivlens.io</i>"

type symLongPct struct {
	sym     string
	longPct float64
}

// briefSnapshot holds snapshot + fear/greed for the brief.
type briefSnapshot struct {
	snap models.MarketSnapshot
	fg   models.FearGreedScore
}

// SendMorningBrief fetches data for all major symbols and sends a daily summary
// to all active subscribers via Telegram.
func (w *Worker) SendMorningBrief(ctx context.Context) {
	log.Println("worker: sending morning brief")

	symbols := []string{"BTC", "ETH", "SOL", "ARB", "DOGE", "AVAX"}

	// Fetch snapshots concurrently for all symbols
	type result struct {
		sym string
		bs  briefSnapshot
		err error
	}
	results := make([]result, len(symbols))
	var wg sync.WaitGroup
	for i, sym := range symbols {
		wg.Add(1)
		go func(idx int, symbol string) {
			defer wg.Done()
			snap, err := w.aggregator.FetchSnapshot(ctx, symbol)
			if err != nil {
				results[idx] = result{sym: symbol, err: err}
				return
			}
			fg := w.calc.Calculate(snap)
			results[idx] = result{sym: symbol, bs: briefSnapshot{snap: snap, fg: fg}}
		}(i, sym)
	}
	wg.Wait()

	// Build snapshots map (symbol -> briefSnapshot)
	snapshots := make(map[string]briefSnapshot)
	for _, r := range results {
		if r.err != nil {
			log.Printf("worker: brief FetchSnapshot(%s): %v", r.sym, r.err)
			continue
		}
		snapshots[r.sym] = r.bs
	}

	// Collect all long/short ratios for "most crowded" calculation
	var allLongPcts []symLongPct
	for sym, bs := range snapshots {
		for _, r := range bs.snap.LongShortRatios {
			allLongPcts = append(allLongPcts, symLongPct{sym: sym, longPct: r.LongPct})
		}
	}
	// Dedupe by symbol: use average long_pct per symbol
	symToLong := make(map[string][]float64)
	for _, p := range allLongPcts {
		symToLong[p.sym] = append(symToLong[p.sym], p.longPct)
	}
	var avgLongs []symLongPct
	for sym, pcts := range symToLong {
		var sum float64
		for _, p := range pcts {
			sum += p
		}
		avgLongs = append(avgLongs, symLongPct{sym: sym, longPct: sum / float64(len(pcts))})
	}

	// Top 2 most crowded longs (highest long_pct)
	sort.Slice(avgLongs, func(i, j int) bool { return avgLongs[i].longPct > avgLongs[j].longPct })
	topLongs := avgLongs
	if len(topLongs) > 2 {
		topLongs = topLongs[:2]
	}

	// Top 2 most crowded shorts: sort by long_pct ascending (lowest longs = most crowded shorts)
	avgShorts := make([]symLongPct, len(avgLongs))
	copy(avgShorts, avgLongs)
	sort.Slice(avgShorts, func(i, j int) bool { return avgShorts[i].longPct < avgShorts[j].longPct })
	topShorts := avgShorts
	if len(topShorts) > 2 {
		topShorts = topShorts[:2]
	}

	// Highest funding symbol (by absolute rate)
	var topFundingSym string
	var topFundingRate float64 // signed, for display
	var topAbsRate float64
	for sym, bs := range snapshots {
		rate := bs.snap.FundingRate.Rate
		absRate := rate
		if absRate < 0 {
			absRate = -absRate
		}
		if absRate > topAbsRate {
			topAbsRate = absRate
			topFundingRate = rate
			topFundingSym = sym
		}
	}

	now := time.Now().UTC()
	dateStr := now.Format("2006-01-02")

	// Build full brief (all 6 symbols)
	fullBrief := w.buildBrief(snapshots, symbols, topLongs, topShorts, topFundingSym, topFundingRate, dateStr, false)

	// Build free brief (BTC only)
	freeSnapshots := make(map[string]briefSnapshot)
	if bs, ok := snapshots["BTC"]; ok {
		freeSnapshots["BTC"] = bs
	}
	freeBrief := w.buildBrief(freeSnapshots, []string{"BTC"}, topLongs, topShorts, topFundingSym, topFundingRate, dateStr, true)

	// Fetch all active subscribers
	subs, err := w.db.GetActiveSubscribers(ctx)
	if err != nil {
		log.Printf("worker: brief GetActiveSubscribers: %v", err)
		return
	}

	sentFree, sentBasic, sentPro := 0, 0, 0
	for _, sub := range subs {
		if sub.ChatID == 0 {
			continue
		}
		tier := sub.Tier
		if tier == "" {
			tier = "free"
		}
		var msg string
		switch tier {
		case "free":
			msg = freeBrief
		case "basic":
			syms := sub.Symbols
			if len(syms) > 5 {
				syms = syms[:5]
			}
			basicSnapshots := make(map[string]briefSnapshot)
			for _, sym := range syms {
				if bs, ok := snapshots[sym]; ok {
					basicSnapshots[sym] = bs
				}
			}
			msg = w.buildBrief(basicSnapshots, syms, topLongs, topShorts, topFundingSym, topFundingRate, dateStr, true)
		default: // pro
			msg = fullBrief
		}
		tierLabel := tier
		if err := w.notifier.SendMessage(ctx, sub.ChatID, msg); err != nil {
			log.Printf("worker: brief SendMessage(%s %s): %v", tierLabel, sub.TelegramUsername, err)
		} else {
			switch tier {
			case "free":
				sentFree++
			case "basic":
				sentBasic++
			default:
				sentPro++
			}
		}
	}
	log.Printf("worker: morning brief sent to %d free, %d basic, %d pro subscribers", sentFree, sentBasic, sentPro)

	// Public channel morning signal — post after Pro briefs
	if bs, ok := snapshots["BTC"]; ok {
		engine := signals.New()
		btcSignals := engine.Analyze(bs.snap, 0)
		publicMessage := fmt.Sprintf(
			`🔍 <b>DerivLens Morning Signal</b> — %s UTC

<b>BTC</b>
- Cascade Risk: %s (%d/100)
- Liquidity Pressure: %+d (%s)
- Regime: %s %d%%
- Stop Hunt Target: %s near $%.0f
- Gravity: %.1f%% %s

<b>Top setup across 16 symbols tracked live.</b>
📊 Full dashboard → derivlens.io
🔔 Subscribe for alerts → t.me/derivlens_alerts_bot`,
			now.Format("02 Jan 2006"),
			btcSignals.CascadeRisk.Level,
			btcSignals.CascadeRisk.Score,
			btcSignals.LiquidityPressure.Score,
			btcSignals.LiquidityPressure.Label,
			btcSignals.Regime,
			btcSignals.RegimeConfidence,
			btcSignals.StopHunt.TargetSide,
			btcSignals.StopHunt.TargetPrice,
			math.Max(btcSignals.LiquidityGravity.UpwardPull, btcSignals.LiquidityGravity.DownwardPull),
			btcSignals.LiquidityGravity.Dominant,
		)
		if err := w.notifier.PostToChannel(publicMessage); err != nil {
			log.Printf("worker: brief PostToChannel: %v", err)
		}
	}
}

func (w *Worker) buildBrief(
	snapshots map[string]briefSnapshot,
	symbols []string,
	topLongs, topShorts []symLongPct,
	topFundingSym string,
	topFundingRate float64,
	dateStr string,
	addUpgradeCTA bool,
) string {
	// Market overview
	var overview string
	for _, sym := range symbols {
		bs, ok := snapshots[sym]
		if !ok {
			continue
		}
		fr := bs.snap.FundingRate.Rate * 100
		oi24 := bs.snap.OpenInterest.OIChange24h
		overview += fmt.Sprintf("• %s: FR %.4f%% | OI %.1f%% | F&G %d %s\n", sym, fr, oi24, bs.fg.Score, bs.fg.Label)
	}

	// Most crowded longs
	var longsStr string
	for _, p := range topLongs {
		longsStr += fmt.Sprintf("%s (%.1f%%), ", p.sym, p.longPct)
	}
	longsStr = strings.TrimSuffix(longsStr, ", ")

	// Most crowded shorts (display short_pct = 100 - long_pct)
	var shortsStr string
	for _, p := range topShorts {
		shortPct := 100 - p.longPct
		shortsStr += fmt.Sprintf("%s (%.1f%% shorts), ", p.sym, shortPct)
	}
	shortsStr = strings.TrimSuffix(shortsStr, ", ")

	// Highest funding
	var fundingStr string
	if topFundingSym != "" {
		if bs, ok := snapshots[topFundingSym]; ok {
			fundingStr = fmt.Sprintf("%s (%.4f%%)", topFundingSym, bs.snap.FundingRate.Rate*100)
		} else {
			fundingStr = fmt.Sprintf("%s (%.4f%%)", topFundingSym, topFundingRate*100)
		}
	}

	// Sentiment headline
	summary := buildSentimentSummary(snapshots, symbols, topLongs, topShorts)

	msg := fmt.Sprintf("🌅 <b>DerivLens Morning Brief</b> — %s UTC\n\n", dateStr)
	msg += summary
	msg += "<b>📊 Market Overview</b>\n" + overview
	msg += "\n<b>🔥 Most Crowded Longs</b>\n" + longsStr + "\n"
	msg += "\n<b>❄️ Most Crowded Shorts</b>\n" + shortsStr + "\n"
	msg += "\n<b>⚡ Highest Funding</b>\n" + fundingStr + "\n"
	msg += fmt.Sprintf("\n<i>DerivLens Pro • %s 13:00 UTC</i>", dateStr)
	if addUpgradeCTA {
		msg += upgradeCTA
	}
	return msg
}

// buildSentimentSummary produces a 1–2 line headline that tells the trader
// what the overall mood is and where the squeeze risk sits — before they
// read any numbers.
func buildSentimentSummary(snapshots map[string]briefSnapshot, symbols []string, topLongs, topShorts []symLongPct) string {
	if len(snapshots) == 0 {
		return ""
	}

	// Average F&G across available symbols
	var fgSum, fgCount float64
	for _, sym := range symbols {
		if bs, ok := snapshots[sym]; ok {
			fgSum += float64(bs.fg.Score)
			fgCount++
		}
	}
	avgFG := fgSum / fgCount

	// Overall sentiment word
	var sentiment string
	switch {
	case avgFG >= 60:
		sentiment = "Risk-on"
	case avgFG <= 35:
		sentiment = "Cautious"
	case avgFG <= 45:
		sentiment = "Neutral, leaning cautious"
	default:
		sentiment = "Neutral"
	}

	// Crowded side note
	var crowdedNote string
	if len(topLongs) > 0 && topLongs[0].longPct >= 68 {
		var syms []string
		for _, p := range topLongs {
			if p.longPct >= 68 {
				syms = append(syms, p.sym)
			}
		}
		crowdedNote = "Longs overcrowded on " + strings.Join(syms, " + ") + "."
	} else if len(topShorts) > 0 && (100-topShorts[0].longPct) >= 38 {
		var syms []string
		for _, p := range topShorts {
			if (100 - p.longPct) >= 38 {
				syms = append(syms, p.sym)
			}
		}
		crowdedNote = "Shorts overcrowded on " + strings.Join(syms, " + ") + "."
	}

	// Squeeze risk: symbols with negative funding above 0.01% absolute
	var negFunding, posFunding []string
	for _, sym := range symbols {
		bs, ok := snapshots[sym]
		if !ok {
			continue
		}
		rate := bs.snap.FundingRate.Rate * 100
		absRate := rate
		if absRate < 0 {
			absRate = -absRate
		}
		if absRate < 0.01 {
			continue
		}
		if rate < 0 {
			negFunding = append(negFunding, fmt.Sprintf("%s (%.4f%%)", sym, rate))
		} else {
			posFunding = append(posFunding, fmt.Sprintf("%s (%.4f%%)", sym, rate))
		}
	}

	var squeezeNote string
	switch {
	case len(negFunding) > 0 && len(posFunding) > 0:
		squeezeNote = fmt.Sprintf(
			"Negative funding on %s — short squeeze risk elevated. Positive funding on %s — longs paying up.",
			strings.Join(negFunding, ", "), strings.Join(posFunding, ", "),
		)
	case len(negFunding) > 0:
		squeezeNote = fmt.Sprintf("Negative funding on %s — short squeeze risk elevated.", strings.Join(negFunding, ", "))
	case len(posFunding) > 0:
		squeezeNote = fmt.Sprintf("Positive funding on %s — longs paying up, long squeeze risk if price drops.", strings.Join(posFunding, ", "))
	}

	// Compose summary block
	line1 := fmt.Sprintf("<b>Sentiment: %s.</b>", sentiment)
	if crowdedNote != "" {
		line1 += " " + crowdedNote
	}

	var summary string
	if squeezeNote != "" {
		summary = line1 + "\n" + squeezeNote + "\n\n"
	} else {
		summary = line1 + "\n\n"
	}
	return summary
}
