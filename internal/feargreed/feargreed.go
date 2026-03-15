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
	switch {
	case rate >= 0.0005:
		return 90
	case rate >= 0.0001:
		return 70
	case rate >= -0.0001:
		return 50
	case rate >= -0.0005:
		return 30
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

	switch {
	case avg >= 70:
		return 90
	case avg >= 60:
		return 65
	case avg >= 40:
		return 50
	case avg >= 30:
		return 35
	default:
		return 10
	}
}

func liquidationScore(levels []models.LiquidationLevel) int {
	var largestLong, largestShort float64
	for _, l := range levels {
		if l.Side == "long" && l.SizeUsd > largestLong {
			largestLong = l.SizeUsd
		}
		if l.Side == "short" && l.SizeUsd > largestShort {
			largestShort = l.SizeUsd
		}
	}

	total := largestLong + largestShort
	if total == 0 {
		return 50 // neutral when no data
	}

	ratio := largestLong / total
	switch {
	case ratio >= 0.7:
		return 25
	case ratio >= 0.55:
		return 40
	case ratio >= 0.45:
		return 50
	case ratio >= 0.3:
		return 60
	default:
		return 75
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
