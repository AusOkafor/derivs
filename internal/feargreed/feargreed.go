package feargreed

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"derivs-backend/internal/models"
)

type AlternativeFearGreed struct {
	Value     int
	Label     string
	Timestamp string
}

func fetchAlternativeFearGreed() (*AlternativeFearGreed, error) {
	resp, err := http.Get("https://api.alternative.me/fng/?limit=1")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		Data []struct {
			Value               string `json:"value"`
			ValueClassification string `json:"value_classification"`
			Timestamp           string `json:"timestamp"`
		} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	if len(result.Data) == 0 {
		return nil, fmt.Errorf("no data")
	}

	val, _ := strconv.Atoi(result.Data[0].Value)
	return &AlternativeFearGreed{
		Value:     val,
		Label:     result.Data[0].ValueClassification,
		Timestamp: result.Data[0].Timestamp,
	}, nil
}

type Calculator struct {
	marketFGMu   sync.Mutex
	marketFG     *AlternativeFearGreed
	marketFGAt   time.Time
	marketFGTTL  time.Duration
}

func New() *Calculator {
	return &Calculator{marketFGTTL: time.Hour}
}

// GetMarketIndex returns the Alternative.me global Fear & Greed index (cached for 1 hour).
func (c *Calculator) GetMarketIndex() (*AlternativeFearGreed, error) {
	c.marketFGMu.Lock()
	defer c.marketFGMu.Unlock()
	if c.marketFG != nil && time.Since(c.marketFGAt) < c.marketFGTTL {
		return c.marketFG, nil
	}
	fg, err := fetchAlternativeFearGreed()
	if err != nil {
		return nil, err
	}
	c.marketFG = fg
	c.marketFGAt = time.Now()
	return fg, nil
}

// Calculate derives a 0-100 Fear & Greed score from a MarketSnapshot.
// Pure computation — no external calls.
func (c *Calculator) Calculate(snap models.MarketSnapshot) models.FearGreedScore {
	log.Printf("feargreed: calculating for %s, funding=%.6f, oi24h=%.2f",
		snap.Symbol, snap.FundingRate.Rate, snap.OpenInterest.OIChange24h)
	fs := fundingScore(snap.FundingRate.Rate)
	os := oiScore(snap.OpenInterest.OIChange24h)
	ls := longShortScore(snap.LongShortRatios)
	liqS := liquidationScore(snap.LiquidationMap.Levels)

	total := (fs*25 + os*25 + ls*25 + liqS*25) / 100

	var fg models.FearGreedScore
	fg.Symbol = snap.Symbol
	fg.Score = total
	fg.Label = label(total)
	fg.Components.FundingScore = fs
	fg.Components.OIScore = os
	fg.Components.LongShortScore = ls
	fg.Components.LiquidationScore = liqS
	fg.Timestamp = time.Now().UTC()
	return fg
}

// ─── Component scorers ────────────────────────────────────────────────────────

func fundingScore(rate float64) int {
	// Added intermediate levels — previously jumped from 50→70 for any positive rate.
	switch {
	case rate >= 0.0005:
		return 90
	case rate >= 0.0003:
		return 75
	case rate >= 0.0001:
		return 60
	case rate >= -0.0001:
		return 50
	case rate >= -0.0003:
		return 35
	case rate >= -0.0005:
		return 25
	default:
		return 10
	}
}

func oiScore(change24h float64) int {
	switch {
	case change24h >= 5.0:
		return 85
	case change24h >= 2.0:
		return 65
	case change24h >= -2.0:
		return 50
	case change24h >= -5.0:
		return 35
	default:
		return 15
	}
}

func longShortScore(ratios []models.LongShortRatio) int {
	if len(ratios) == 0 {
		return 50 // neutral when no data
	}
	var sum float64
	for _, r := range ratios {
		sum += r.LongPct
	}
	avg := sum / float64(len(ratios))

	// Bell-curve: extreme positioning in EITHER direction = overcrowding = risk = Fear.
	// In derivatives, >65% longs or <35% longs signals a crowded trade prone to cascades —
	// not sustained greed. Previously this was monotonic (more longs = more greed), which
	// contradicted liquidationScore and ignored liquidation cascade risk at extremes.
	switch {
	case avg >= 75:
		return 20 // extreme long crowding — cascade risk
	case avg >= 65:
		return 35 // overcrowded longs — caution
	case avg >= 55:
		return 60 // longs slightly leading — mild greed
	case avg >= 45:
		return 50 // balanced — neutral
	case avg >= 35:
		return 40 // shorts slightly leading — mild fear
	default:
		return 20 // extreme short crowding — squeeze/unwind risk
	}
}

func liquidationScore(levels []models.LiquidationLevel) int {
	// Use TOTAL cluster size on each side — previously used only the largest single cluster.
	// Large long clusters below price = trapped longs = bearish pressure = Fear.
	// Large short clusters above price = trapped shorts = upward pressure = Greed.
	var totalLong, totalShort float64
	for _, l := range levels {
		if l.Side == "long" {
			totalLong += l.SizeUsd
		} else {
			totalShort += l.SizeUsd
		}
	}

	total := totalLong + totalShort
	if total == 0 {
		return 50 // neutral when no data
	}

	ratio := totalLong / total
	switch {
	case ratio >= 0.7:
		return 20 // dominated by long clusters — trapped longs = bearish
	case ratio >= 0.55:
		return 38
	case ratio >= 0.45:
		return 50
	case ratio >= 0.3:
		return 62
	default:
		return 80 // dominated by short clusters — trapped shorts = bullish pressure
	}
}

func label(score int) string {
	switch {
	case score <= 20:
		return "Extreme Fear"
	case score <= 40:
		return "Fear"
	case score <= 60:
		return "Neutral"
	case score <= 80:
		return "Greed"
	default:
		return "Extreme Greed"
	}
}
