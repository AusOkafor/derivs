package worker

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"derivs-backend/internal/models"
)

const briefSymbols = "BTC, ETH, SOL, ARB, DOGE, AVAX"
const upgradeCTA = "\n\n<i>Upgrade to Pro for all 6 symbols → derivlens-pro.vercel.app</i>"

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

	sentFree, sentPro := 0, 0
	for _, sub := range subs {
		if sub.ChatID == 0 {
			continue
		}
		msg := fullBrief
		if isFreeTier(sub.Tier) {
			msg = freeBrief
			if err := w.notifier.SendMessage(ctx, sub.ChatID, msg); err != nil {
				log.Printf("worker: brief SendMessage(free %s): %v", sub.TelegramUsername, err)
			} else {
				sentFree++
			}
		} else {
			if err := w.notifier.SendMessage(ctx, sub.ChatID, msg); err != nil {
				log.Printf("worker: brief SendMessage(pro %s): %v", sub.TelegramUsername, err)
			} else {
				sentPro++
			}
		}
	}
	log.Printf("worker: morning brief sent to %d free, %d pro subscribers", sentFree, sentPro)
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

	msg := fmt.Sprintf("🌅 <b>DerivLens Morning Brief</b> — %s UTC\n\n", dateStr)
	msg += "<b>📊 Market Overview</b>\n" + overview
	msg += "\n<b>🔥 Most Crowded Longs</b>\n" + longsStr + "\n"
	msg += "\n<b>❄️ Most Crowded Shorts</b>\n" + shortsStr + "\n"
	msg += "\n<b>⚡ Highest Funding</b>\n" + fundingStr + "\n"
	msg += fmt.Sprintf("\n<i>DerivLens Pro • %s 08:00 UTC</i>", dateStr)
	if addUpgradeCTA {
		msg += upgradeCTA
	}
	return msg
}
