package signals

import (
	"math"
	"sort"

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

	// 5. Leverage Imbalance
	sig.LeverageImbalance = detectLeverageImbalance(snap)

	// 6. Market Regime (depends on all above)
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

	// Score = size_usd / distance% — prevents small nearby clusters from triggering
	// Minimum size: $50,000 to be considered
	type candidate struct {
		lvl      models.LiquidationLevel
		distance float64
		score    float64
	}

	var best *candidate
	for _, lvl := range snap.LiquidationMap.Levels {
		if lvl.SizeUsd < 50_000 {
			continue
		}
		distance := math.Abs(lvl.Price-currentPrice) / currentPrice * 100
		if distance > 3.0 || distance < 0.00001 {
			continue
		}
		score := lvl.SizeUsd / math.Max(distance, 0.01)
		if best == nil || score > best.score {
			best = &candidate{lvl: lvl, distance: distance, score: score}
		}
	}

	if best == nil {
		return nil
	}

	// Probability model — weighted by size and proximity
	prob := 40 // base
	// Size contribution (up to +30)
	sizeScore := int(math.Min(best.lvl.SizeUsd/100_000, 3) * 10)
	prob += sizeScore
	// Proximity contribution (up to +20)
	prob += int((3.0 - best.distance) / 3.0 * 20)
	// Funding confirms direction (+15)
	if best.lvl.Side == "short" && snap.FundingRate.Rate < 0 {
		prob += 15
	}
	if best.lvl.Side == "long" && snap.FundingRate.Rate > 0.0003 {
		prob += 15
	}
	// OI expanding adds conviction (+10)
	if math.Abs(snap.OpenInterest.OIChange1h) > 1.5 {
		prob += 10
	}
	if prob > 95 {
		prob = 95
	}

	return &models.LiquidationMagnet{
		Side:        best.lvl.Side,
		Price:       best.lvl.Price,
		SizeUSD:     best.lvl.SizeUsd,
		Distance:    best.distance,
		Probability: prob,
	}
}

func calcLiquidityGravity(snap models.MarketSnapshot) models.LiquidityGravity {
	currentPrice := snap.LiquidationMap.CurrentPrice
	if currentPrice == 0 {
		return models.LiquidityGravity{}
	}

	var upwardWeight, downwardWeight float64
	var upwardTarget, downwardTarget float64
	var upwardSize, downwardSize float64
	var bestUpWeight, bestDownWeight float64
	var gravityLevels []models.GravityLevel

	for _, lvl := range snap.LiquidationMap.Levels {
		if lvl.SizeUsd < 10_000 {
			continue
		}
		distance := math.Abs(lvl.Price-currentPrice) / currentPrice * 100
		if distance < 0.0001 {
			distance = 0.0001
		}

		weight := lvl.SizeUsd / (distance * distance)

		gravityLevels = append(gravityLevels, models.GravityLevel{
			Price:   lvl.Price,
			SizeUSD: lvl.SizeUsd,
			Side:    lvl.Side,
			Weight:  weight,
		})

		if lvl.Price > currentPrice {
			pullMultiplier := 1.0
			if lvl.Side == "short" {
				pullMultiplier = 1.5
			}
			upwardWeight += weight * pullMultiplier
			upwardSize += lvl.SizeUsd
			if weight*pullMultiplier > bestUpWeight {
				bestUpWeight = weight * pullMultiplier
				upwardTarget = lvl.Price
			}
		} else {
			pullMultiplier := 1.0
			if lvl.Side == "long" {
				pullMultiplier = 1.5
			}
			downwardWeight += weight * pullMultiplier
			downwardSize += lvl.SizeUsd
			if weight*pullMultiplier > bestDownWeight {
				bestDownWeight = weight * pullMultiplier
				downwardTarget = lvl.Price
			}
		}
	}

	total := upwardWeight + downwardWeight
	if total == 0 {
		return models.LiquidityGravity{
			UpwardPull:   50,
			DownwardPull: 50,
			Dominant:     "neutral",
		}
	}

	upPct := upwardWeight / total * 100
	downPct := downwardWeight / total * 100

	dominant := "upward"
	if downPct > upPct {
		dominant = "downward"
	}

	sort.Slice(gravityLevels, func(i, j int) bool {
		return gravityLevels[i].Weight > gravityLevels[j].Weight
	})
	if len(gravityLevels) > 5 {
		gravityLevels = gravityLevels[:5]
	}

	return models.LiquidityGravity{
		UpwardPull:     math.Round(upPct*10) / 10,
		DownwardPull:   math.Round(downPct*10) / 10,
		UpwardTarget:   upwardTarget,
		DownwardTarget: downwardTarget,
		UpwardSize:     upwardSize,
		DownwardSize:   downwardSize,
		Dominant:       dominant,
		Levels:         gravityLevels,
	}
}

func calcSqueezeProbability(snap models.MarketSnapshot) (shortSqueeze, longSqueeze int) {
	avgLong := avgLongPct(snap.LongShortRatios)
	rate := snap.FundingRate.Rate

	// SHORT SQUEEZE score (shorts trapped, price could spike up)
	shortScore := 0
	if rate < 0 {
		shortScore += 20
	}
	if rate < -0.0001 {
		shortScore += 15
	}
	if rate < -0.0003 {
		shortScore += 10
	}
	if avgLong < 50 {
		shortScore += 15
	}
	if avgLong < 45 {
		shortScore += 10
	}
	if snap.OpenInterest.OIChange1h > 0.5 {
		shortScore += 10
	}
	if snap.OpenInterest.OIChange24h < -3 {
		shortScore += 10
	}
	for _, lvl := range snap.LiquidationMap.Levels {
		if lvl.Side == "short" && lvl.Price > snap.LiquidationMap.CurrentPrice && lvl.SizeUsd > 100_000 {
			shortScore += 15
			break
		}
	}
	if shortScore > 95 {
		shortScore = 95
	}

	// LONG SQUEEZE score (longs trapped, price could drop)
	longScore := 0
	if rate > 0.0001 {
		longScore += 20
	}
	if rate > 0.0003 {
		longScore += 15
	}
	if rate > 0.0005 {
		longScore += 10
	}
	if avgLong > 60 {
		longScore += 15
	}
	if avgLong > 65 {
		longScore += 10
	}
	if snap.OpenInterest.OIChange1h > 1.0 {
		longScore += 10
	}
	if snap.OpenInterest.OIChange24h < -3 {
		longScore += 10
	}
	for _, lvl := range snap.LiquidationMap.Levels {
		if lvl.Side == "long" && lvl.Price < snap.LiquidationMap.CurrentPrice && lvl.SizeUsd > 100_000 {
			longScore += 15
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
