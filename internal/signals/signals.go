package signals

import (
	"fmt"
	"math"
	"sort"

	"derivs-backend/internal/models"
)

// Engine computes all signals from a snapshot
type Engine struct{}

func New() *Engine { return &Engine{} }

func (e *Engine) Analyze(snap models.MarketSnapshot, momentum float64) models.MarketSignals {
	sig := models.MarketSignals{Symbol: snap.Symbol}

	// 1. OI Trend (price + OI correlation)
	sig.OITrend = detectOITrend(snap)

	// 2. Liquidation Magnet (with momentum filter)
	sig.LiquidationMagnet = detectLiquidationMagnet(snap, momentum)

	// 3. Liquidity Gravity
	sig.LiquidityGravity = calcLiquidityGravity(snap)

	// 4. Squeeze Probabilities
	sig.ShortSqueezeProbability, sig.LongSqueezeProbability = calcSqueezeProbability(snap)

	// 5. Leverage Imbalance
	sig.LeverageImbalance = detectLeverageImbalance(snap)

	// 6. Volatility Expansion
	sig.Volatility = calcVolatilityExpansion(snap)

	// 7. Market Regime
	sig.Regime, sig.RegimeConfidence = detectRegime(snap, sig)

	// 8. Stop Hunt Probability (with momentum adjustment)
	sig.StopHunt = calcStopHunt(snap, sig.LiquidityGravity, sig.LiquidationMagnet, momentum)

	// 9. Exchange Divergence
	sig.ExchangeDivergence = calcExchangeDivergence(snap.LongShortRatios)

	// 9.5. Funding Velocity
	sig.FundingVelocity = calcFundingVelocity(snap)

	// 9.6. OI Delta
	sig.OIDelta = calcOIDelta(snap)

	// 10. Cascade Risk (must be last — depends on all other signals)
	sig.CascadeRisk = calcCascadeRisk(snap, sig)

	// 11. Liquidity Pressure Index (depends on gravity, funding, stop hunt, squeeze)
	sig.LiquidityPressure = calcLiquidityPressure(snap, sig)

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

func detectLiquidationMagnet(snap models.MarketSnapshot, momentum float64) *models.LiquidationMagnet {
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

	magnet := &models.LiquidationMagnet{
		Side:        best.lvl.Side,
		Price:       best.lvl.Price,
		SizeUSD:     best.lvl.SizeUsd,
		Distance:    best.distance,
		Probability: prob,
	}
	magnet.Probability = applyMomentumFilter(magnet.Probability, magnet.Side, momentum)
	return magnet
}

func applyMomentumFilter(baseProb int, side string, momentum float64) int {
	// momentum > 0 = price moving up
	// momentum < 0 = price moving down
	adjusted := float64(baseProb)

	// Short cluster above price — upward momentum increases probability
	if side == "short" {
		if momentum > 0.05 {
			adjusted *= 1.15 // price moving toward short cluster
		} else if momentum < -0.05 {
			adjusted *= 0.75 // price moving away from short cluster
		}
	}

	// Long cluster below price — downward momentum increases probability
	if side == "long" {
		if momentum < -0.05 {
			adjusted *= 1.15 // price moving toward long cluster
		} else if momentum > 0.05 {
			adjusted *= 0.75 // price moving away from long cluster
		}
	}

	// Clamp to 0-98 (never 100% certain)
	if adjusted > 98 {
		adjusted = 98
	}
	if adjusted < 0 {
		adjusted = 0
	}
	return int(adjusted)
}

func calcLiquidityGravity(snap models.MarketSnapshot) models.LiquidityGravity {
	currentPrice := snap.LiquidationMap.CurrentPrice
	if currentPrice == 0 {
		return models.LiquidityGravity{Dominant: "neutral", UpwardPull: 50, DownwardPull: 50}
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

		// Use linear weight (size only) — distance is too small for distance² model
		// with tight orderbook data
		weight := lvl.SizeUsd

		// Side multiplier: short clusters above = stronger upward pull (market hunts them)
		// long clusters below = stronger downward pull
		pullMultiplier := 1.0
		if lvl.Price > currentPrice {
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

		gravityLevels = append(gravityLevels, models.GravityLevel{
			Price:   lvl.Price,
			SizeUSD: lvl.SizeUsd,
			Side:    lvl.Side,
			Weight:  weight * pullMultiplier,
		})
	}

	total := upwardWeight + downwardWeight
	if total == 0 {
		return models.LiquidityGravity{
			Dominant:     "neutral",
			UpwardPull:   50,
			DownwardPull: 50,
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

func calcVolatilityExpansion(snap models.MarketSnapshot) models.VolatilityExpansion {
	score := 0
	var triggers []string

	// Signal 1 — OI expanding rapidly (new leverage entering)
	if snap.OpenInterest.OIChange1h > 2.0 {
		score += 25
		triggers = append(triggers, fmt.Sprintf("OI spiking +%.1f%% in 1h", snap.OpenInterest.OIChange1h))
	} else if snap.OpenInterest.OIChange1h > 1.0 {
		score += 15
		triggers = append(triggers, fmt.Sprintf("OI rising +%.1f%% in 1h", snap.OpenInterest.OIChange1h))
	}

	// Signal 2 — OI collapsing (liquidation cascade underway)
	if snap.OpenInterest.OIChange1h < -2.0 {
		score += 30
		triggers = append(triggers, fmt.Sprintf("OI collapsing %.1f%% in 1h — liquidation cascade risk", snap.OpenInterest.OIChange1h))
	} else if snap.OpenInterest.OIChange1h < -1.0 {
		score += 15
		triggers = append(triggers, fmt.Sprintf("OI dropping %.1f%% in 1h", snap.OpenInterest.OIChange1h))
	}

	// Signal 3 — Extreme funding (over-leveraged market)
	absRate := math.Abs(snap.FundingRate.Rate)
	if absRate > 0.0005 {
		score += 25
		triggers = append(triggers, fmt.Sprintf("Extreme funding rate %.4f%% — market over-leveraged", snap.FundingRate.Rate*100))
	} else if absRate > 0.0003 {
		score += 15
		triggers = append(triggers, fmt.Sprintf("Elevated funding rate %.4f%%", snap.FundingRate.Rate*100))
	}

	// Signal 4 — Funding and OI divergence (dangerous setup)
	if snap.OpenInterest.OIChange1h > 1.0 && snap.FundingRate.Rate < -0.0002 {
		score += 20
		triggers = append(triggers, "OI rising + negative funding — aggressive short positioning")
	}
	if snap.OpenInterest.OIChange1h > 1.0 && snap.FundingRate.Rate > 0.0004 {
		score += 20
		triggers = append(triggers, "OI rising + high positive funding — aggressive long positioning")
	}

	// Signal 5 — Crowded positioning (one side dominates)
	avgLong := avgLongPct(snap.LongShortRatios)
	if avgLong > 70 || avgLong < 30 {
		score += 20
		triggers = append(triggers, fmt.Sprintf("Positioning extremely crowded — %.1f%% longs", avgLong))
	} else if avgLong > 65 || avgLong < 35 {
		score += 10
		triggers = append(triggers, fmt.Sprintf("Positioning crowded — %.1f%% longs", avgLong))
	}

	// Signal 6 — Large liquidation cluster at current price
	currentPrice := snap.LiquidationMap.CurrentPrice
	if currentPrice > 0 {
		for _, lvl := range snap.LiquidationMap.Levels {
			dist := math.Abs(lvl.Price-currentPrice) / currentPrice * 100
			if dist < 0.5 && lvl.SizeUsd > 200_000 {
				score += 20
				triggers = append(triggers, fmt.Sprintf("Large $%.0fk liquidation cluster within 0.5%% of price", lvl.SizeUsd/1000))
				break
			}
		}
	}

	if score > 100 {
		score = 100
	}

	// Determine state
	var state models.VolatilityState
	var expectedMove string

	oiChanging := math.Abs(snap.OpenInterest.OIChange1h) > 1.0

	switch {
	case score >= 70:
		state = models.VolStateExpanding
		expectedMove = "High — volatility expanding, expect sharp directional move"
	case score >= 50:
		if oiChanging {
			state = models.VolStateExpanding
			expectedMove = "High — volatility expanding, expect sharp directional move"
		} else {
			state = models.VolStateElevated
			expectedMove = "Medium — elevated volatility, directional bias unclear"
		}
	case score >= 30:
		state = models.VolStateElevated
		expectedMove = "Medium — elevated volatility, directional bias unclear"
	case absRate < 0.0001 && math.Abs(snap.OpenInterest.OIChange1h) < 0.3:
		state = models.VolStateCompressed
		expectedMove = "Low current volatility — breakout expansion likely"
	default:
		state = models.VolStateContracting
		if score >= 30 {
			expectedMove = "Medium — volatility declining but not yet compressed"
		} else {
			expectedMove = "Low — stable market conditions"
		}
	}

	return models.VolatilityExpansion{
		State:         state,
		Score:         score,
		ExpansionProb: score,
		Triggers:      triggers,
		ExpectedMove:  expectedMove,
	}
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

func calcStopHunt(snap models.MarketSnapshot, gravity models.LiquidityGravity, magnet *models.LiquidationMagnet, momentum float64) models.StopHuntSignal {
	rate := snap.FundingRate.Rate
	avgLong := avgLongPct(snap.LongShortRatios)

	// === DIRECTIONAL SCORE: which side is more likely to be hunted ===
	// Seed from gravity (directional distribution), then adjust with independent signals.
	// Positive = short hunt (upward sweep), Negative = long hunt (downward sweep).
	directional := gravity.UpwardPull - gravity.DownwardPull // -50 to +50

	// Funding rate: positive = longs paying = long liquidation pressure
	if rate > 0.0005 {
		directional -= 30
	} else if rate > 0.0003 {
		directional -= 20
	} else if rate > 0.0001 {
		directional -= 10
	} else if rate < -0.0005 {
		directional += 30
	} else if rate < -0.0003 {
		directional += 20
	} else if rate < -0.0001 {
		directional += 10
	}

	// Long/short ratio: crowded longs = downward hunt, crowded shorts = upward hunt
	if avgLong > 65 {
		directional -= 20
	} else if avgLong > 60 {
		directional -= 10
	} else if avgLong < 35 {
		directional += 20
	} else if avgLong < 40 {
		directional += 10
	}

	// Momentum: price already moving toward a side amplifies that side
	if momentum > 0.1 {
		directional += 15
	} else if momentum > 0.05 {
		directional += 8
	} else if momentum < -0.1 {
		directional -= 15
	} else if momentum < -0.05 {
		directional -= 8
	}

	// Convert directional score to side probabilities (range kept 5–95)
	shortHuntPct := 50.0 + directional*0.5
	if shortHuntPct > 95 {
		shortHuntPct = 95
	}
	if shortHuntPct < 5 {
		shortHuntPct = 5
	}
	shortHuntProb := int(math.Round(shortHuntPct))
	longHuntProb := 100 - shortHuntProb

	// === CONFIDENCE SCORE: how likely any hunt occurs at all ===
	// Built entirely from signals independent of gravity percentage.
	// Max 70 — without a validated liquidation magnet we should never claim high certainty.
	confidence := 0

	// Funding magnitude: extreme funding creates urgency to rebalance
	absRate := math.Abs(rate)
	if absRate > 0.0005 {
		confidence += 25
	} else if absRate > 0.0003 {
		confidence += 15
	} else if absRate > 0.0001 {
		confidence += 8
	}

	// OI change 1h: rapid position buildup = fragile, liquidation-prone
	if snap.OpenInterest.OIChange1h > 2.0 {
		confidence += 20
	} else if snap.OpenInterest.OIChange1h > 1.0 {
		confidence += 12
	} else if snap.OpenInterest.OIChange1h > 0.5 {
		confidence += 6
	}

	// Nearby liquidation cluster: proximity + size = imminent sweep risk
	currentPrice := snap.LiquidationMap.CurrentPrice
	if currentPrice > 0 {
		for _, lvl := range snap.LiquidationMap.Levels {
			distPct := math.Abs(lvl.Price-currentPrice) / currentPrice * 100
			if distPct < 1.0 && lvl.SizeUsd > 500_000 {
				confidence += 25
				break
			} else if distPct < 2.0 && lvl.SizeUsd > 200_000 {
				confidence += 15
				break
			} else if distPct < 3.0 && lvl.SizeUsd > 100_000 {
				confidence += 8
				break
			}
		}
	}

	// Extreme positioning crowding (high probability trapped positions exist)
	if avgLong > 0 && (avgLong > 70 || avgLong < 30) {
		confidence += 10
	}

	// OI declining sharply while funding is still high = trapped positions unwinding
	if snap.OpenInterest.OIChange24h < -5 && absRate > 0.0003 {
		confidence += 10
	}

	// Strong momentum (price already moving = execution risk is live)
	absMomentum := math.Abs(momentum)
	if absMomentum > 0.1 {
		confidence += 8
	} else if absMomentum > 0.05 {
		confidence += 4
	}

	if confidence > 70 {
		confidence = 70
	}

	targetSide := "longs"
	targetPrice := gravity.DownwardTarget
	if shortHuntProb > longHuntProb {
		targetSide = "shorts"
		targetPrice = gravity.UpwardTarget
	}
	if magnet != nil {
		targetPrice = magnet.Price
	}

	reasoning := fmt.Sprintf(
		"%.1f%% upward liquidity pull with %s funding — %s side more likely to be hunted first (confidence: %d%%)",
		gravity.UpwardPull,
		fundingDescription(rate),
		targetSide,
		confidence,
	)

	return models.StopHuntSignal{
		ShortSideProb: shortHuntProb,
		LongSideProb:  longHuntProb,
		Confidence:    confidence,
		TargetSide:    targetSide,
		TargetPrice:   targetPrice,
		Reasoning:     reasoning,
	}
}

func fundingDescription(rate float64) string {
	switch {
	case rate > 0.0003:
		return "elevated positive"
	case rate > 0.0001:
		return "positive"
	case rate < -0.0003:
		return "strongly negative"
	case rate < -0.0001:
		return "negative"
	default:
		return "neutral"
	}
}

func calcFundingVelocity(snap models.MarketSnapshot) models.FundingVelocitySignal {
	rate := snap.FundingRate.Rate

	var direction, description string
	alert := false

	switch {
	case rate > 0.0005:
		direction = "accelerating_positive"
		description = fmt.Sprintf("Funding accelerating positive at %.4f%% — longs being crowded fast", rate*100)
		alert = true
	case rate < -0.0005:
		direction = "accelerating_negative"
		description = fmt.Sprintf("Funding accelerating negative at %.4f%% — shorts being crowded fast", rate*100)
		alert = true
	case rate > 0.0003:
		direction = "rising_positive"
		description = fmt.Sprintf("Funding rising positive at %.4f%% — longs building", rate*100)
	case rate < -0.0003:
		direction = "rising_negative"
		description = fmt.Sprintf("Funding rising negative at %.4f%% — shorts building", rate*100)
	default:
		direction = "stable"
		description = "Funding stable — no crowding detected"
	}

	return models.FundingVelocitySignal{
		RatePerHour: rate * 100,
		Direction:   direction,
		Alert:       alert,
		Description: description,
	}
}

func calcOIDelta(snap models.MarketSnapshot) models.OIDeltaSignal {
	change := snap.OpenInterest.OIChange1h

	var velocity, description string
	alert := false

	switch {
	case change > 5:
		velocity = "surging"
		description = fmt.Sprintf("OI surging +%.1f%% in 1h — aggressive leverage entering", change)
		alert = true
	case change > 2:
		velocity = "rising"
		description = fmt.Sprintf("OI rising +%.1f%% in 1h — new positions building", change)
	case change > -2:
		velocity = "stable"
		description = "OI stable — no significant leverage change"
	case change > -5:
		velocity = "falling"
		description = fmt.Sprintf("OI falling %.1f%% in 1h — positions unwinding", change)
	default:
		velocity = "collapsing"
		description = fmt.Sprintf("OI collapsing %.1f%% in 1h — rapid deleveraging", change)
		alert = true
	}

	return models.OIDeltaSignal{
		ChangePercent: change,
		Velocity:      velocity,
		Alert:         alert,
		Description:   description,
	}
}

func calcExchangeDivergence(ratios []models.LongShortRatio) models.ExchangeDivergence {
	if len(ratios) < 2 {
		return models.ExchangeDivergence{}
	}

	maxLong := ratios[0]
	minLong := ratios[0]
	for _, r := range ratios {
		if r.LongPct > maxLong.LongPct {
			maxLong = r
		}
		if r.LongPct < minLong.LongPct {
			minLong = r
		}
	}

	spread := maxLong.LongPct - minLong.LongPct
	detected := spread >= 5.0

	signal := "Exchanges aligned — no divergence"
	if detected {
		huntSide := "short-side"
		if maxLong.LongPct > 60 {
			huntSide = "long-side"
		}
		signal = fmt.Sprintf(
			"%s heavily long (%.1f%%) vs %s short-heavy (%.1f%%) — divergence suggests liquidity hunt likely. Watch for move toward %s liquidations.",
			maxLong.Exchange, maxLong.LongPct,
			minLong.Exchange, minLong.LongPct,
			huntSide,
		)
	}

	return models.ExchangeDivergence{
		Detected:   detected,
		MaxSpread:  math.Round(spread*10) / 10,
		BullishEx:  maxLong.Exchange,
		BearishEx:  minLong.Exchange,
		BullishPct: maxLong.LongPct,
		BearishPct: minLong.LongPct,
		Signal:     signal,
	}
}

func calcCascadeRisk(snap models.MarketSnapshot, sig models.MarketSignals) models.CascadeRiskScore {
	score := 0
	var factors []string

	if snap.OpenInterest.OIChange1h > 2.0 {
		score += 20
		factors = append(factors, fmt.Sprintf("OI expanding +%.1f%% in 1h", snap.OpenInterest.OIChange1h))
	} else if snap.OpenInterest.OIChange1h > 1.0 {
		score += 10
		factors = append(factors, "OI rising")
	}

	absRate := math.Abs(snap.FundingRate.Rate)
	if absRate > 0.0005 {
		score += 25
		factors = append(factors, fmt.Sprintf("Extreme funding %.4f%%", snap.FundingRate.Rate*100))
	} else if absRate > 0.0003 {
		score += 15
		factors = append(factors, "Elevated funding rate")
	}

	avgLong := avgLongPct(snap.LongShortRatios)
	if avgLong > 70 || avgLong < 30 {
		score += 20
		factors = append(factors, fmt.Sprintf("Positioning extreme — %.1f%% longs", avgLong))
	} else if avgLong > 65 || avgLong < 35 {
		score += 10
		factors = append(factors, fmt.Sprintf("Positioning crowded — %.1f%% longs", avgLong))
	}

	if sig.Volatility.State == models.VolStateCompressed {
		score += 20
		factors = append(factors, "Volatility compressed — breakout energy building")
	}

	if sig.LiquidationMagnet != nil && sig.LiquidationMagnet.Probability >= 80 {
		score += 20
		factors = append(factors, fmt.Sprintf("%.0f%% probability liquidation sweep at $%.0f",
			float64(sig.LiquidationMagnet.Probability), sig.LiquidationMagnet.Price))
	} else if sig.LiquidationMagnet != nil && sig.LiquidationMagnet.Probability >= 65 {
		score += 10
		factors = append(factors, "High-probability liquidation cluster nearby")
	}

	dominantPull := sig.LiquidityGravity.UpwardPull
	if sig.LiquidityGravity.DownwardPull > dominantPull {
		dominantPull = sig.LiquidityGravity.DownwardPull
	}
	if dominantPull >= 80 {
		score += 15
		factors = append(factors, fmt.Sprintf("Strong liquidity gravity — %.1f%% directional pull", dominantPull))
	}

	if sig.ExchangeDivergence.Detected {
		score += 10
		factors = append(factors, fmt.Sprintf("Cross-exchange divergence %.1f%% spread", sig.ExchangeDivergence.MaxSpread))
	}

	if score > 100 {
		score = 100
	}

	// If magnet probability >= 85%, cascade score should be at least MEDIUM (25+)
	if sig.LiquidationMagnet != nil && sig.LiquidationMagnet.Probability >= 85 {
		if score < 25 {
			score = 25
			factors = append(factors, fmt.Sprintf(
				"High sweep probability %.0f%% overrides low cascade score",
				float64(sig.LiquidationMagnet.Probability),
			))
		}
	}

	var level, description string
	switch {
	case score >= 75:
		level = "CRITICAL"
		description = "Multiple cascade signals aligned — high probability of rapid liquidation event"
	case score >= 50:
		level = "HIGH"
		description = "Several cascade conditions present — watch for accelerated move if trigger breaks"
	case score >= 25:
		level = "MEDIUM"
		description = "Some cascade conditions building — monitor closely"
	default:
		level = "LOW"
		description = "Market stable — no immediate cascade risk"
	}

	return models.CascadeRiskScore{
		Level:       level,
		Score:       score,
		Factors:     factors,
		Description: description,
	}
}

func calcLiquidityPressure(snap models.MarketSnapshot, sig models.MarketSignals) models.LiquidityPressureIndex {
	score := 0

	// Factor 1 — Liquidity gravity direction (+/-40 max)
	if sig.LiquidityGravity.UpwardPull > sig.LiquidityGravity.DownwardPull {
		dominance := sig.LiquidityGravity.UpwardPull - sig.LiquidityGravity.DownwardPull
		score += int(dominance * 0.4) // 100% dominance = +40
	} else {
		dominance := sig.LiquidityGravity.DownwardPull - sig.LiquidityGravity.UpwardPull
		score -= int(dominance * 0.4)
	}

	// Factor 2 — Funding rate (+/-25 max)
	rate := snap.FundingRate.Rate
	if rate < -0.0003 {
		score += 25 // very negative funding = shorts crowded = bullish pressure
	} else if rate < -0.0001 {
		score += 15
	} else if rate < 0 {
		score += 5
	} else if rate > 0.0003 {
		score -= 25
	} else if rate > 0.0001 {
		score -= 15
	} else if rate > 0 {
		score -= 5
	}

	// Factor 3 — Stop hunt direction (+/-20 max)
	if sig.StopHunt.TargetSide == "shorts" {
		score += int(float64(sig.StopHunt.ShortSideProb) * 0.2)
	} else if sig.StopHunt.TargetSide == "longs" {
		score -= int(float64(sig.StopHunt.LongSideProb) * 0.2)
	}

	// Factor 4 — Short/long squeeze probability (+/-15 max)
	score += int(float64(sig.ShortSqueezeProbability) * 0.15)
	score -= int(float64(sig.LongSqueezeProbability) * 0.15)

	// Clamp to -100/+100
	if score > 100 {
		score = 100
	}
	if score < -100 {
		score = -100
	}

	// Label and direction
	var label, direction, description string
	switch {
	case score >= 60:
		label = "Strong Squeeze Risk"
		direction = "bullish"
		description = "High probability short squeeze — price likely hunts upward liquidity"
	case score >= 30:
		label = "Bullish Pressure"
		direction = "bullish"
		description = "Liquidity favors upward move — shorts at risk"
	case score >= 10:
		label = "Mild Bullish Bias"
		direction = "bullish"
		description = "Slight upward liquidity pressure — no strong signal yet"
	case score > -10:
		label = "Neutral"
		direction = "neutral"
		description = "No clear directional pressure — market in equilibrium"
	case score > -30:
		label = "Mild Bearish Bias"
		direction = "bearish"
		description = "Slight downward liquidity pressure — no strong signal yet"
	case score > -60:
		label = "Bearish Pressure"
		direction = "bearish"
		description = "Liquidity favors downward move — longs at risk"
	default:
		label = "Strong Liquidation Risk"
		direction = "bearish"
		description = "High probability long liquidation — price likely hunts downward liquidity"
	}

	return models.LiquidityPressureIndex{
		Score:       score,
		Label:       label,
		Direction:   direction,
		Description: description,
	}
}
