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

	return fmt.Sprintf(`You are a crypto derivatives analyst writing for experienced traders.

Write EXACTLY 2 sentences using this structure:

SENTENCE 1: Name the key price level and the setup. If a liquidation magnet exists, include its sweep probability. No other numbers.
Format: "[Symbol] approaching $[price] — [one-line setup description]. Sweep probability: [N]%%." OR combine naturally.

SENTENCE 2: One qualitative observation about positioning or momentum and a watch condition.
Format: "[Qualitative signal] — watch for [if/then condition]."

HARD RULES:
- The ONLY numbers permitted are: the price level from the magnet, and the sweep probability. Cluster size (e.g. "$236k", "$1.2M") is NOT permitted — describe size qualitatively ("large cluster", "thin cluster") if needed. No other percentages, ratios, or scores.
- Use all other data (exchange divergence, funding, OI, long/short ratios) to form qualitative words only — never quote those numbers.
- No "will" — use "watch for", "likely", "if/then".
- No outcome predictions ("violent", "explosive", "aggressive").
- No bullets, no markdown.

SYMBOL: %s
MARKET REGIME: %s (confidence: %d%%)
OI TREND: %s
LEVERAGE IMBALANCE: %s
SHORT SQUEEZE PROBABILITY: %d%%
LONG SQUEEZE PROBABILITY: %d%%
%s

LIQUIDITY GRAVITY:
Dominant direction: %s (%.1f%% pull toward $%.0f, $%.2fM pool)
Opposing: %.1f%% pull toward $%.0f, $%.2fM pool

VOLATILITY:
State: %s (score: %d/100)
Expected Move: %s
Triggers: %s

STOP HUNT:
Short side probability: %d%%
Long side probability: %d%%
Target: %s side near $%.0f
Reasoning: %s

EXCHANGE DIVERGENCE:
Detected: %v (spread: %.1f%%)
%s longs: %.1f%% | %s longs: %.1f%%
Signal: %s

FUNDING VELOCITY: %s (%.4f%%/hr) — %s
OI DELTA: %s (%.1f%% in 1h) — %s

LIQUIDATION CASCADE RISK: %s (%d/100)
%s

LIQUIDITY PRESSURE INDEX: %d (%s)
%s

RAW DATA:
Funding Rate: %.4f%%
Open Interest: $%.2fM (1h: %.1f%%, 24h: %.1f%%)
Long/Short: %.1f%% longs (use for judgment only — do not quote this number)

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
		sigs.LiquidityGravity.Dominant,
		sigs.LiquidityGravity.UpwardPull, sigs.LiquidityGravity.UpwardTarget, sigs.LiquidityGravity.UpwardSize/1_000_000,
		sigs.LiquidityGravity.DownwardPull, sigs.LiquidityGravity.DownwardTarget, sigs.LiquidityGravity.DownwardSize/1_000_000,
		sigs.Volatility.State, sigs.Volatility.Score,
		sigs.Volatility.ExpectedMove,
		strings.Join(sigs.Volatility.Triggers, ", "),
		sigs.StopHunt.ShortSideProb,
		sigs.StopHunt.LongSideProb,
		sigs.StopHunt.TargetSide,
		sigs.StopHunt.TargetPrice,
		sigs.StopHunt.Reasoning,
		sigs.ExchangeDivergence.Detected, sigs.ExchangeDivergence.MaxSpread,
		sigs.ExchangeDivergence.BullishEx, sigs.ExchangeDivergence.BullishPct,
		sigs.ExchangeDivergence.BearishEx, sigs.ExchangeDivergence.BearishPct,
		sigs.ExchangeDivergence.Signal,
		sigs.FundingVelocity.Direction, sigs.FundingVelocity.RatePerHour, sigs.FundingVelocity.Description,
		sigs.OIDelta.Velocity, sigs.OIDelta.ChangePercent, sigs.OIDelta.Description,
		sigs.CascadeRisk.Level, sigs.CascadeRisk.Score,
		sigs.CascadeRisk.Description,
		sigs.LiquidityPressure.Score, sigs.LiquidityPressure.Label,
		sigs.LiquidityPressure.Description,
		snap.FundingRate.Rate*100,
		snap.OpenInterest.OIUsd/1_000_000,
		snap.OpenInterest.OIChange1h,
		snap.OpenInterest.OIChange24h,
		avgLong,
	)
}

// dominantTarget/dominantSize/opposingTarget/opposingSize return the correct
// gravity targets based on which direction is dominant, so the prompt shows
// qualitative direction without exposing raw pull percentages to Claude.
// oiChangeLabel converts a raw OI change percentage into a qualitative label.
func oiChangeLabel(pct float64) string {
	switch {
	case pct > 5:
		return "rising sharply"
	case pct > 1:
		return "rising"
	case pct > -1:
		return "flat"
	case pct > -5:
		return "falling"
	default:
		return "falling sharply"
	}
}

// longShortLabel converts a long percentage into a qualitative positioning label.
func longShortLabel(longPct float64) string {
	switch {
	case longPct > 70:
		return "heavily long"
	case longPct > 60:
		return "long-biased"
	case longPct > 55:
		return "slightly long"
	case longPct >= 45:
		return "balanced"
	case longPct >= 40:
		return "slightly short"
	case longPct >= 30:
		return "short-biased"
	default:
		return "heavily short"
	}
}

func squeezeRiskLabel(prob int) string {
	switch {
	case prob >= 75:
		return "high"
	case prob >= 50:
		return "elevated"
	case prob >= 25:
		return "moderate"
	default:
		return "low"
	}
}

func depthLabel(sizeMillions float64) string {
	switch {
	case sizeMillions >= 5:
		return "deep"
	case sizeMillions >= 1:
		return "moderate"
	case sizeMillions >= 0.3:
		return "thin"
	default:
		return "very thin"
	}
}

func dominantTarget(g models.LiquidityGravity) float64 {
	if g.UpwardPull >= g.DownwardPull {
		return g.UpwardTarget
	}
	return g.DownwardTarget
}

func dominantSize(g models.LiquidityGravity) float64 {
	if g.UpwardPull >= g.DownwardPull {
		return g.UpwardSize / 1_000_000
	}
	return g.DownwardSize / 1_000_000
}

func opposingTarget(g models.LiquidityGravity) float64 {
	if g.UpwardPull >= g.DownwardPull {
		return g.DownwardTarget
	}
	return g.UpwardTarget
}

func opposingSize(g models.LiquidityGravity) float64 {
	if g.UpwardPull >= g.DownwardPull {
		return g.DownwardSize / 1_000_000
	}
	return g.UpwardSize / 1_000_000
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
		System: `You are a crypto derivatives analyst writing for experienced traders.

OUTPUT FORMAT: Respond with valid JSON only — no markdown, no text outside the JSON object.

WRITING RULES (hard constraints — any violation makes the response unusable):
- 2-3 sentences. No bullets, no arrows, no markdown inside the summary string.
- Lead with the specific price level and the setup at that level.
- ONE number permitted in the entire summary. That number must be the liquidation magnet sweep probability. Never cite long/short ratios, liquidity gravity percentages, cascade scores, squeeze probabilities, or any other percentage.
- No certainty language: never use "will". Use "watch for", "likely", or if/then framing.
- Never describe a potential move as "violent", "aggressive", or "explosive".
- Do not restate regime labels or numbers already shown in the dashboard panels.`,
		Messages: []anthropicMessage{{Role: "user", Content: prompt}},
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
