package models

import "time"

type FundingRate struct {
	Symbol          string    `json:"symbol"`
	Rate            float64   `json:"rate"`
	NextFundingTime int64     `json:"next_funding_time"`
	Timestamp       time.Time `json:"timestamp"`
}

type OpenInterest struct {
	Symbol     string    `json:"symbol"`
	OIUsd      float64   `json:"oi_usd"`
	OIChange1h float64   `json:"oi_change_1h"`
	OIChange4h float64   `json:"oi_change_4h"`
	OIChange24h float64  `json:"oi_change_24h"`
	Timestamp  time.Time `json:"timestamp"`
}

type LiquidationLevel struct {
	Symbol  string  `json:"symbol"`
	Price   float64 `json:"price"`
	Side    string  `json:"side"`
	SizeUsd float64 `json:"size_usd"`
}

type LiquidationMap struct {
	Symbol       string             `json:"symbol"`
	Levels       []LiquidationLevel `json:"levels"`
	CurrentPrice float64            `json:"current_price"`
	Timestamp    time.Time          `json:"timestamp"`
}

type LongShortRatio struct {
	Symbol    string    `json:"symbol"`
	Exchange  string    `json:"exchange"`
	LongPct   float64   `json:"long_pct"`
	ShortPct  float64   `json:"short_pct"`
	Ratio     float64   `json:"ratio"`
	Timestamp time.Time `json:"timestamp"`
}

type MarketSnapshot struct {
	Symbol          string           `json:"symbol"`
	FundingRate     FundingRate      `json:"funding_rate"`
	OpenInterest    OpenInterest     `json:"open_interest"`
	LiquidationMap  LiquidationMap   `json:"liquidation_map"`
	LongShortRatios []LongShortRatio `json:"long_short_ratios"`
	Timestamp       time.Time        `json:"timestamp"`
}

type AIAnalysis struct {
	Symbol      string    `json:"symbol"`
	Summary     string    `json:"summary"`
	Sentiment   string    `json:"sentiment"`
	Confidence  int       `json:"confidence"`
	GeneratedAt time.Time `json:"generated_at"`
}

type Alert struct {
	ID        string    `json:"id"`
	Symbol    string    `json:"symbol"`
	Message   string    `json:"message"`
	Severity  string    `json:"severity"` // "low" | "medium" | "high"
	Timestamp time.Time `json:"timestamp"`
}

type AlertHistoryEntry struct {
	ID          string    `json:"id"`
	Symbol      string    `json:"symbol"`
	AlertID     string    `json:"alert_id"`
	Message     string    `json:"message"`
	Severity    string    `json:"severity"`
	TriggeredAt time.Time `json:"triggered_at"`
}

type FearGreedScore struct {
	Symbol     string    `json:"symbol"`
	Score      int       `json:"score"` // 0-100
	Label      string    `json:"label"` // "Extreme Fear" | "Fear" | "Neutral" | "Greed" | "Extreme Greed"
	Components struct {
		FundingScore     int `json:"funding_score"`
		OIScore          int `json:"oi_score"`
		LongShortScore   int `json:"long_short_score"`
		LiquidationScore int `json:"liquidation_score"`
	} `json:"components"`
	Timestamp time.Time `json:"timestamp"`
}

// MarketRegime represents the current market state
type MarketRegime string

const (
	RegimeTrending     MarketRegime = "Trending"
	RegimeRanging      MarketRegime = "Ranging"
	RegimeLiquidation  MarketRegime = "Liquidation Event"
	RegimeAccumulation MarketRegime = "Accumulation"
	RegimeDistribution MarketRegime = "Distribution"
)

// OITrend represents OI + price correlation
type OITrend string

const (
	OITrendNewLongs       OITrend = "New longs entering — trend confirmation"
	OITrendShortCovering  OITrend = "Short covering rally"
	OITrendNewShorts     OITrend = "New shorts building"
	OITrendLongLiquidation OITrend = "Long liquidation — deleveraging"
)

// LiquidationMagnet represents a nearby liquidation cluster that may attract price
type LiquidationMagnet struct {
	Side        string  `json:"side"`        // "long" or "short"
	Price       float64 `json:"price"`
	SizeUSD     float64 `json:"size_usd"`
	Distance    float64 `json:"distance"`    // % distance from current price
	Probability int     `json:"probability"` // 0-100
}

// MarketSignals holds all pre-interpreted signals from the signal engine
type MarketSignals struct {
	Symbol                 string            `json:"symbol"`
	Regime                 MarketRegime     `json:"regime"`
	RegimeConfidence       int              `json:"regime_confidence"`
	OITrend                OITrend          `json:"oi_trend"`
	ShortSqueezeProbability int             `json:"short_squeeze_probability"` // 0-100
	LongSqueezeProbability  int             `json:"long_squeeze_probability"`  // 0-100
	LiquidationMagnet      *LiquidationMagnet `json:"liquidation_magnet,omitempty"`
	LeverageImbalance      string           `json:"leverage_imbalance"` // "Longs overcrowded" / "Shorts overcrowded" / "Balanced"
	SqueezeDirection       string           `json:"squeeze_direction"`  // "Long squeeze risk" / "Short squeeze risk" / "None"
}

type SnapshotWithAnalysis struct {
	Snapshot  MarketSnapshot `json:"snapshot"`
	Analysis  AIAnalysis     `json:"analysis"`
	Alerts    []Alert        `json:"alerts"`
	FearGreed FearGreedScore `json:"fear_greed"`
	Signals   MarketSignals  `json:"signals"`
}

type TickerInfo struct {
	Symbol    string    `json:"symbol"`
	Price     float64   `json:"price"`
	Change24h float64   `json:"change_24h"`
	Timestamp time.Time `json:"timestamp"`
}

type FundingRatePoint struct {
	Timestamp int64   `json:"timestamp"`
	Rate      float64 `json:"rate"`
}

type OICandle struct {
	Timestamp int64   `json:"timestamp"`
	OIUsd     float64 `json:"oi_usd"`
}

type HistoricalData struct {
	Symbol         string             `json:"symbol"`
	FundingHistory []FundingRatePoint `json:"funding_history"`
	OIHistory      []OICandle         `json:"oi_history"`
	Timestamp      time.Time          `json:"timestamp"`
}
