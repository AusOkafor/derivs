package analysis

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"derivs-backend/internal/models"
)

var (
	aiEnabled = true
	aiMu     sync.Mutex
)

func SetAIEnabled(enabled bool) {
	aiMu.Lock()
	defer aiMu.Unlock()
	aiEnabled = enabled
}

func IsAIEnabled() bool {
	aiMu.Lock()
	defer aiMu.Unlock()
	return aiEnabled
}

type Analyzer struct {
	apiKey     string
	httpClient *http.Client
	sem        chan struct{}
}

func New(apiKey string) *Analyzer {
	return &Analyzer{
		apiKey:     apiKey,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		sem:        make(chan struct{}, 2), // max 2 concurrent Claude calls
	}
}

// ─── Anthropic API shapes ─────────────────────────────────────────────────────

type anthropicRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	System    string             `json:"system"`
	Messages  []anthropicMessage `json:"messages"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicResponse struct {
	Content []anthropicContent `json:"content"`
}

type anthropicContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// claudeJSON is the structured JSON shape expected inside Claude's text block.
type claudeJSON struct {
	Summary    string `json:"summary"`
	Sentiment  string `json:"sentiment"`
	Confidence int    `json:"confidence"`
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

func errFallback(symbol string) models.AIAnalysis {
	return models.AIAnalysis{
		Symbol:      symbol,
		Summary:     "Analysis temporarily unavailable",
		Sentiment:   "neutral",
		Confidence:  0,
		GeneratedAt: time.Now().UTC(),
	}
}

// stripFences removes accidental markdown code fences Claude may wrap its JSON in.
func stripFences(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		if idx := strings.Index(s, "\n"); idx != -1 {
			s = s[idx+1:]
		}
	}
	s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	return strings.TrimSpace(s)
}

// buildPrompt assembles the user-turn prompt from live snapshot data and pre-computed signals.
func buildPrompt(snap models.MarketSnapshot, sigs models.MarketSignals) string {
	avgLong := 50.0
	if len(snap.LongShortRatios) > 0 {
		sum := 0.0
		for _, r := range snap.LongShortRatios {
			sum += r.LongPct
		}
		avgLong = sum / float64(len(snap.LongShortRatios))
	}

	return fmt.Sprintf(`You are a crypto derivatives analyst. Write like a trader talking to another trader — direct, specific, no hedging for its own sake.

WRITING RULES:
- 2-3 sentences maximum. No bullets, no arrows, no markdown.
- Lead with the most actionable insight, not the regime label.
- Name the specific price level that matters and what happens if it breaks.
- If the signal is weak or mixed, say so in one sentence then give the one thing to watch.
- Do not repeat numbers already obvious from the data labels (e.g. do not restate the regime name verbatim).
- Do not use both "78%%" and "78.1%%" in the same sentence — pick one.

SYMBOL: %s
MARKET REGIME: %s (Confidence: %d%%)
OI TREND: %s
LEVERAGE IMBALANCE: %s
SHORT SQUEEZE PROBABILITY: %d%%
LONG SQUEEZE PROBABILITY: %d%%
%s

LIQUIDITY GRAVITY:
↑ Upward pull: %.1f%% toward $%.0f ($%.2fM)
↓ Downward pull: %.1f%% toward $%.0f ($%.2fM)
Dominant direction: %s

VOLATILITY:
State: %s (Score: %d/100)
Expected Move: %s
Triggers: %s

STOP HUNT PROBABILITY:
Short side hunted first: %d%%
Long side hunted first: %d%%
Target: %s side near $%.0f
Reasoning: %s

EXCHANGE DIVERGENCE:
Detected: %v (spread: %.1f%%)
%s long-heavy: %.1f%% | %s short-heavy: %.1f%%
Signal: %s

FUNDING VELOCITY: %s (%.4f%%/hr) %s
OI DELTA: %s (%.1f%% in 1h) %s

LIQUIDATION CASCADE RISK: %s (%d/100)
%s
Factors: %s

LIQUIDITY PRESSURE INDEX: %d/100 (%s)
%s

RAW DATA:
Funding Rate: %.4f%%
Open Interest: $%.2fM (1h: %.1f%%, 24h: %.1f%%)
Long/Short: %.1f%% longs

Respond ONLY with valid JSON, no markdown:
{
  "summary": "2-3 sentence analysis",
  "sentiment": "bullish" | "neutral" | "bearish",
  "confidence": 0-100
}`,
		snap.Symbol,
		sigs.Regime, sigs.RegimeConfidence,
		sigs.OITrend,
		sigs.LeverageImbalance,
		sigs.ShortSqueezeProbability,
		sigs.LongSqueezeProbability,
		formatMagnet(sigs.LiquidationMagnet),
		sigs.LiquidityGravity.UpwardPull,
		sigs.LiquidityGravity.UpwardTarget,
		sigs.LiquidityGravity.UpwardSize/1_000_000,
		sigs.LiquidityGravity.DownwardPull,
		sigs.LiquidityGravity.DownwardTarget,
		sigs.LiquidityGravity.DownwardSize/1_000_000,
		sigs.LiquidityGravity.Dominant,
		sigs.Volatility.State,
		sigs.Volatility.Score,
		sigs.Volatility.ExpectedMove,
		strings.Join(sigs.Volatility.Triggers, ", "),
		sigs.StopHunt.ShortSideProb,
		sigs.StopHunt.LongSideProb,
		sigs.StopHunt.TargetSide,
		sigs.StopHunt.TargetPrice,
		sigs.StopHunt.Reasoning,
		sigs.ExchangeDivergence.Detected,
		sigs.ExchangeDivergence.MaxSpread,
		sigs.ExchangeDivergence.BullishEx,
		sigs.ExchangeDivergence.BullishPct,
		sigs.ExchangeDivergence.BearishEx,
		sigs.ExchangeDivergence.BearishPct,
		sigs.ExchangeDivergence.Signal,
		sigs.FundingVelocity.Direction,
		sigs.FundingVelocity.RatePerHour,
		sigs.FundingVelocity.Description,
		sigs.OIDelta.Velocity,
		sigs.OIDelta.ChangePercent,
		sigs.OIDelta.Description,
		sigs.CascadeRisk.Level,
		sigs.CascadeRisk.Score,
		sigs.CascadeRisk.Description,
		strings.Join(sigs.CascadeRisk.Factors, "; "),
		sigs.LiquidityPressure.Score,
		sigs.LiquidityPressure.Label,
		sigs.LiquidityPressure.Description,
		snap.FundingRate.Rate*100,
		snap.OpenInterest.OIUsd/1_000_000,
		snap.OpenInterest.OIChange1h,
		snap.OpenInterest.OIChange24h,
		avgLong,
	)
}

func formatMagnet(m *models.LiquidationMagnet) string {
	if m == nil {
		return "LIQUIDATION MAGNET: None detected within 3%%"
	}
	return fmt.Sprintf("LIQUIDATION MAGNET: %s cluster at $%.0f (%.1f%% away, %d%% probability of sweep)",
		m.Side, m.Price, m.Distance, m.Probability)
}

// ─── Public ───────────────────────────────────────────────────────────────────

func (a *Analyzer) Analyze(ctx context.Context, snap models.MarketSnapshot, sigs models.MarketSignals, tier string, userAPIKey string, preferredModel string) (models.AIAnalysis, error) {
	if !IsAIEnabled() {
		return models.AIAnalysis{
			Symbol:      snap.Symbol,
			Summary:     "AI analysis is currently paused.",
			Sentiment:   "neutral",
			Confidence:  0,
			GeneratedAt: time.Now().UTC(),
		}, nil
	}
	if tier != "pro" {
		return models.AIAnalysis{
			Symbol:      snap.Symbol,
			Summary:     "Upgrade to Pro to unlock AI-powered market analysis.",
			Sentiment:   "neutral",
			Confidence:  0,
			GeneratedAt: time.Now().UTC(),
		}, nil
	}

	select {
	case a.sem <- struct{}{}:
		defer func() { <-a.sem }()
	case <-ctx.Done():
		return errFallback(snap.Symbol), ctx.Err()
	}

	prompt := buildPrompt(snap, sigs)
	promptPreview := prompt
	if len(promptPreview) > 200 {
		promptPreview = promptPreview[:200]
	}
	log.Printf("analysis: generating for %s, prompt[:200]: %s", snap.Symbol, promptPreview)

	apiKey := a.apiKey
	if userAPIKey != "" {
		apiKey = userAPIKey
	}
	model := "claude-haiku-4-5-20251001"
	if preferredModel != "" {
		model = preferredModel
	}

	reqBody := anthropicRequest{
		Model:     model,
		MaxTokens: 1024,
		System:    "You are a crypto derivatives analyst writing for experienced traders. Be direct and specific. Respond with valid JSON only — no markdown, no explanation outside the JSON.",
		Messages:  []anthropicMessage{{Role: "user", Content: prompt}},
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		log.Printf("analysis: marshal request: %v", err)
		return errFallback(snap.Symbol), fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost,
		"https://api.anthropic.com/v1/messages",
		bytes.NewReader(bodyBytes),
	)
	if err != nil {
		log.Printf("analysis: build request: %v", err)
		return errFallback(snap.Symbol), fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		log.Printf("analysis: do request: %v", err)
		return errFallback(snap.Symbol), fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		log.Printf("analysis: anthropic returned status %d: %s", resp.StatusCode, string(body))
		return errFallback(snap.Symbol), fmt.Errorf("anthropic returned status %d", resp.StatusCode)
	}

	var apiResp anthropicResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		log.Printf("analysis: decode response: %v", err)
		return errFallback(snap.Symbol), fmt.Errorf("decode response: %w", err)
	}

	// Find the first text content block.
	var raw string
	for _, block := range apiResp.Content {
		if block.Type == "text" {
			raw = block.Text
			break
		}
	}
	if raw == "" {
		log.Printf("analysis: no text content block for %s", snap.Symbol)
		return errFallback(snap.Symbol), fmt.Errorf("no text content in response")
	}

	raw = stripFences(raw)

	var cj claudeJSON
	if err := json.Unmarshal([]byte(raw), &cj); err != nil {
		log.Printf("analysis: unmarshal claude JSON for %s: %v (raw: %s)", snap.Symbol, err, raw)
		return errFallback(snap.Symbol), fmt.Errorf("unmarshal claude JSON: %w", err)
	}

	return models.AIAnalysis{
		Symbol:      snap.Symbol,
		Summary:     cj.Summary,
		Sentiment:   cj.Sentiment,
		Confidence:  cj.Confidence,
		GeneratedAt: time.Now().UTC(),
	}, nil
}
