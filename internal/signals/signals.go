package signals

import (
	"math"

	"derivs-backend/internal/models"
)

// Engine computes all signals from a snapshot
type Engine struct{}

func New() *Engine { return &Engine{} }

func (e *Engine) Analyze(snap models.MarketSnapshot) models.MarketSignals {
	sig := models.MarketSignals{Symbol: snap.Symbol}

	// 1. OI Trend (price + OI correlation)
	sig.OITrend = detectOITrend(snap)

	// 2. Liquidation Magnet
	sig.LiquidationMagnet = detectLiquidationMagnet(snap)

	// 3. Squeeze Probabilities
	sig.ShortSqueezeProbability, sig.LongSqueezeProbability = calcSqueezeProbability(snap)

	// 4. Leverage Imbalance
	sig.LeverageImbalance = detectLeverageImbalance(snap)

	// 5. Market Regime (depends on all above)
	sig.Regime, sig.RegimeConfidence = detectRegime(snap, sig)

	return sig
}

func detectOITrend(snap models.MarketSnapshot) models.OITrend {
	oiRising := snap.OpenInterest.OIChange1h > 0
	priceRising := snap.FundingRate.Rate > 0

	switch {
	case priceRising && oiRising:
		return models.OITrendNewLongs
	case priceRising && !oiRising:
		return models.OITrendShortCovering
	case !priceRising && oiRising:
		return models.OITrendNewShorts
	default:
		return models.OITrendLongLiquidation
	}
}

func detectLiquidationMagnet(snap models.MarketSnapshot) *models.LiquidationMagnet {
	currentPrice := snap.LiquidationMap.CurrentPrice
	if currentPrice == 0 {
		return nil
	}

	levels := snap.LiquidationMap.Levels
	var largest *models.LiquidationLevel
	for i := range levels {
		lvl := &levels[i]
		distance := math.Abs(lvl.Price-currentPrice) / currentPrice * 100
		if distance <= 3.0 {
			if largest == nil || lvl.SizeUsd > largest.SizeUsd {
				largest = lvl
			}
		}
	}

	if largest == nil || largest.SizeUsd < 100_000 {
		return nil
	}

	distance := math.Abs(largest.Price-currentPrice) / currentPrice * 100

	// Probability model:
	// Base: 50% if within 3%
	// +10% for each 0.5% closer
	// +15% if funding confirms direction
	// +10% if OI expanding
	prob := 50
	prob += int((3.0 - distance) / 0.5 * 10)
	if largest.Side == "short" && snap.FundingRate.Rate < 0 {
		prob += 15 // negative funding + short cluster = squeeze up likely
	}
	if largest.Side == "long" && snap.FundingRate.Rate > 0.0003 {
		prob += 15 // high positive funding + long cluster = squeeze down likely
	}
	if math.Abs(snap.OpenInterest.OIChange1h) > 1.5 {
		prob += 10
	}
	if prob > 95 {
		prob = 95
	}

	return &models.LiquidationMagnet{
		Side:        largest.Side,
		Price:       largest.Price,
		SizeUSD:     largest.SizeUsd,
		Distance:    distance,
		Probability: prob,
	}
}

func calcSqueezeProbability(snap models.MarketSnapshot) (shortSqueeze, longSqueeze int) {
	shortScore := 0
	if snap.FundingRate.Rate < -0.0001 {
		shortScore += 25
	}
	if snap.FundingRate.Rate < -0.0003 {
		shortScore += 15
	}
	avgLong := avgLongPct(snap.LongShortRatios)
	if avgLong < 45 {
		shortScore += 20
	}
	if snap.OpenInterest.OIChange1h > 1.0 {
		shortScore += 20
	}
	if snap.OpenInterest.OIChange24h < -3.0 {
		shortScore += 10
	}
	for _, lvl := range snap.LiquidationMap.Levels {
		if lvl.Side == "short" && lvl.Price > snap.LiquidationMap.CurrentPrice && lvl.SizeUsd > 200_000 {
			shortScore += 10
			break
		}
	}
	if shortScore > 95 {
		shortScore = 95
	}

	longScore := 0
	if snap.FundingRate.Rate > 0.0003 {
		longScore += 25
	}
	if snap.FundingRate.Rate > 0.0005 {
		longScore += 15
	}
	if avgLong > 65 {
		longScore += 20
	}
	if avgLong > 72 {
		longScore += 10
	}
	if snap.OpenInterest.OIChange1h > 2.0 {
		longScore += 15
	}
	for _, lvl := range snap.LiquidationMap.Levels {
		if lvl.Side == "long" && lvl.Price < snap.LiquidationMap.CurrentPrice && lvl.SizeUsd > 200_000 {
			longScore += 10
			break
		}
	}
	if longScore > 95 {
		longScore = 95
	}

	return shortScore, longScore
}

func detectLeverageImbalance(snap models.MarketSnapshot) string {
	avgLong := avgLongPct(snap.LongShortRatios)
	rate := snap.FundingRate.Rate
	switch {
	case avgLong > 65 && rate > 0.0002:
		return "Longs overcrowded"
	case avgLong < 40 && rate < -0.0002:
		return "Shorts overcrowded"
	default:
		return "Balanced"
	}
}

func detectRegime(snap models.MarketSnapshot, sig models.MarketSignals) (models.MarketRegime, int) {
	oiChange := snap.OpenInterest.OIChange24h
	rate := snap.FundingRate.Rate
	avgLong := avgLongPct(snap.LongShortRatios)

	// Liquidation Event: OI dropping fast
	if oiChange < -5.0 {
		return models.RegimeLiquidation, 75
	}

	// Distribution: OI rising + high funding + price stalling
	if oiChange > 3.0 && rate > 0.0004 && avgLong > 65 {
		return models.RegimeDistribution, 70
	}

	// Accumulation: OI stable/rising + low/negative funding
	if rate < -0.0001 && oiChange > 0 && avgLong < 50 {
		return models.RegimeAccumulation, 65
	}

	// Trending: OI rising + moderate funding
	if oiChange > 2.0 && math.Abs(rate) < 0.0004 {
		return models.RegimeTrending, 60
	}

	// Default: Ranging
	return models.RegimeRanging, 50
}

func avgLongPct(ratios []models.LongShortRatio) float64 {
	if len(ratios) == 0 {
		return 50
	}
	sum := 0.0
	for _, r := range ratios {
		sum += r.LongPct
	}
	return sum / float64(len(ratios))
}
