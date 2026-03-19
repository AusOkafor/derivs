package models

import "time"

type FundingRate struct {
	Symbol          string    `json:"symbol"`
	Rate            float64   `json:"rate"`
	NextFundingTime int64     `json:"next_funding_time"`
	Timestamp       time.Time `json:"timestamp"`
}

// ExchangeFundingRate holds per-exchange funding rate for display.
type ExchangeFundingRate struct {
	Exchange string  `json:"exchange"`
	Rate     float64 `json:"rate"`
	RatePct  float64 `json:"rate_pct"` // rate * 100
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

// RecentLiquidations holds real-time liquidation data from Binance WebSocket.
type RecentLiquidations struct {
	TotalLongUSD  float64 `json:"total_long_usd"`
	TotalShortUSD float64 `json:"total_short_usd"`
	BurstDetected bool    `json:"burst_detected"`
	BurstSizeUSD  float64 `json:"burst_size_usd"`
	Window        string  `json:"window"`
}

type MarketSnapshot struct {
	Symbol              string               `json:"symbol"`
	FundingRate         FundingRate          `json:"funding_rate"`
	ExchangeFunding     []ExchangeFundingRate `json:"exchange_funding"`
	OpenInterest        OpenInterest         `json:"open_interest"`
	LiquidationMap      LiquidationMap       `json:"liquidation_map"`
	LongShortRatios     []LongShortRatio     `json:"long_short_ratios"`
	RecentLiquidations  *RecentLiquidations  `json:"recent_liquidations,omitempty"`
	Timestamp           time.Time            `json:"timestamp"`
}

type AIAnalysis struct {
	Symbol      string    `json:"symbol"`
	Summary     string    `json:"summary"`
	Sentiment   string    `json:"sentiment"`
	Confidence  int       `json:"confidence"`
	GeneratedAt time.Time `json:"generated_at"`
}

type Alert struct {
	ID           string    `json:"id"`
	Symbol       string    `json:"symbol"`
	Message      string    `json:"message"`
	Severity     string    `json:"severity"` // "low" | "medium" | "high"
	Timestamp    time.Time `json:"timestamp"`
	ClusterPrice float64   `json:"cluster_price"`
	ClusterSize  float64   `json:"cluster_size"`
	Distance     float64   `json:"distance"`     // stored as decimal (0.01 = 1%) for cards
	Probability  int       `json:"probability"`
}

type AlertHistoryEntry struct {
	ID            string     `json:"id"`
	Symbol        string     `json:"symbol"`
	AlertID       string     `json:"alert_id"`
	Message       string     `json:"message"`
	Severity      string     `json:"severity"`
	TriggeredAt   time.Time  `json:"triggered_at"`
	PriceAtAlert  *float64   `json:"price_at_alert,omitempty"`
	Price15m      *float64   `json:"price_15m,omitempty"`
	Price1h       *float64   `json:"price_1h,omitempty"`
	OutcomePct15m *float64   `json:"outcome_pct_15m,omitempty"`
	OutcomePct1h  *float64   `json:"outcome_pct_1h,omitempty"`
}

// CustomPriceAlert is a user-defined price alert stored in custom_price_alerts.
type CustomPriceAlert struct {
	ID          string     `json:"id"`
	SubscriberID string    `json:"subscriber_id"`
	Symbol      string     `json:"symbol"`
	TargetPrice float64    `json:"target_price"`
	Direction   string     `json:"direction"` // "above" | "below"
	Note        string     `json:"note,omitempty"`
	Triggered   bool       `json:"triggered"`
	TriggeredAt *time.Time `json:"triggered_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

type MarketFearGreed struct {
	Value int    `json:"value"`
	Label string `json:"label"`
}

type FearGreedScore struct {
	Symbol         string           `json:"symbol"`
	Score          int              `json:"score"` // 0-100
	Label          string           `json:"label"` // "Extreme Fear" | "Fear" | "Neutral" | "Greed" | "Extreme Greed"
	MarketFearGreed *MarketFearGreed `json:"market_fear_greed,omitempty"`
	Components     struct {
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

// GravityLevel holds a single liquidation level with its gravitational weight
type GravityLevel struct {
	Price   float64 `json:"price"`
	SizeUSD float64 `json:"size_usd"`
	Side    string  `json:"side"`
	Weight  float64 `json:"weight"` // size / distance²
}

// LiquidityGravity represents directional pull from liquidation clusters
type LiquidityGravity struct {
	UpwardPull     float64        `json:"upward_pull"`
	DownwardPull   float64        `json:"downward_pull"`
	UpwardTarget   float64        `json:"upward_target"`
	DownwardTarget float64        `json:"downward_target"`
	UpwardSize     float64        `json:"upward_size"`
	DownwardSize   float64        `json:"downward_size"`
	Dominant       string         `json:"dominant"` // "upward", "downward", or "neutral"
	Levels         []GravityLevel `json:"levels"`
}

// VolatilityState represents the current volatility regime
type VolatilityState string

const (
	VolStateExpanding   VolatilityState = "Expanding"
	VolStateContracting VolatilityState = "Contracting"
	VolStateElevated    VolatilityState = "Elevated"
	VolStateCompressed  VolatilityState = "Compressed"
)

// VolatilityExpansion predicts likelihood of volatility expansion
type VolatilityExpansion struct {
	State         VolatilityState `json:"state"`
	Score         int             `json:"score"`          // 0-100
	ExpansionProb int             `json:"expansion_prob"`  // % chance of volatility expansion
	Triggers      []string        `json:"triggers"`       // reasons e.g. "OI spike", "Funding divergence"
	ExpectedMove  string          `json:"expected_move"`   // "High", "Medium", "Low"
}

// StopHuntSignal indicates which side is more likely to be hunted first
type StopHuntSignal struct {
	ShortSideProb int     `json:"short_side_prob"` // probability shorts get hunted first
	LongSideProb  int     `json:"long_side_prob"`  // probability longs get hunted first
	TargetSide    string  `json:"target_side"`    // "shorts" or "longs"
	TargetPrice   float64 `json:"target_price"`   // most likely hunt target price
	Reasoning     string  `json:"reasoning"`
}

// CascadeRiskScore indicates likelihood of liquidation cascade
type CascadeRiskScore struct {
	Level       string   `json:"level"`        // "LOW", "MEDIUM", "HIGH", "CRITICAL"
	Score       int      `json:"score"`        // 0-100
	Factors     []string `json:"factors"`     // contributing signals
	Description string   `json:"description"`
}

// LiquidityPressureIndex combines gravity, funding, stop hunt, and squeeze into a single -100 to +100 score.
type LiquidityPressureIndex struct {
	Score       int    `json:"score"`       // -100 to +100
	Label       string `json:"label"`       // "Strong Squeeze Risk", "Neutral", "Strong Liquidation Risk"
	Direction   string `json:"direction"`  // "bullish", "bearish", "neutral"
	Description string `json:"description"`
}

// FundingVelocitySignal indicates how fast funding rate is changing.
type FundingVelocitySignal struct {
	RatePerHour float64 `json:"rate_per_hour"` // how fast funding is changing
	Direction   string  `json:"direction"`     // "accelerating_positive", "accelerating_negative", "stable"
	Alert       bool    `json:"alert"`         // true when velocity is extreme
	Description string  `json:"description"`
}

// OIDeltaSignal indicates OI change velocity.
type OIDeltaSignal struct {
	ChangePercent float64 `json:"change_percent"` // OI change in last 1h
	Velocity      string  `json:"velocity"`      // "surging", "rising", "stable", "falling", "collapsing"
	Alert         bool    `json:"alert"`
	Description   string  `json:"description"`
}

// ExchangeDivergence captures cross-exchange long/short positioning divergence
type ExchangeDivergence struct {
	Detected   bool    `json:"detected"`
	MaxSpread  float64 `json:"max_spread"`  // max long% difference between exchanges
	BullishEx  string  `json:"bullish_ex"`  // exchange most long-heavy
	BearishEx  string  `json:"bearish_ex"`  // exchange most short-heavy
	BullishPct float64 `json:"bullish_pct"`
	BearishPct float64 `json:"bearish_pct"`
	Signal     string  `json:"signal"` // interpretation
}

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
	LiquidityGravity       LiquidityGravity  `json:"liquidity_gravity"`
	Volatility             VolatilityExpansion `json:"volatility"`
	LeverageImbalance      string               `json:"leverage_imbalance"` // "Longs overcrowded" / "Shorts overcrowded" / "Balanced"
	SqueezeDirection       string               `json:"squeeze_direction"`  // "Long squeeze risk" / "Short squeeze risk" / "None"
	StopHunt               StopHuntSignal       `json:"stop_hunt"`
	ExchangeDivergence     ExchangeDivergence   `json:"exchange_divergence"`
	CascadeRisk            CascadeRiskScore     `json:"cascade_risk"`
	LiquidityPressure      LiquidityPressureIndex `json:"liquidity_pressure"`
	FundingVelocity        FundingVelocitySignal `json:"funding_velocity"`
	OIDelta                OIDeltaSignal         `json:"oi_delta"`
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

// TickerResult is the full compare-page payload: snapshot, signals, fear_greed, and price data.
type TickerResult struct {
	Symbol    string             `json:"symbol"`
	Snapshot  MarketSnapshot     `json:"snapshot"`
	Signals   MarketSignals      `json:"signals"`
	FearGreed FearGreedScore     `json:"fear_greed"`
	Price     float64            `json:"price"`
	Change24h float64            `json:"change_24h"`
	Timestamp time.Time          `json:"timestamp"`
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
