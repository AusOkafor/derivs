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

	return fmt.Sprintf(`You are a professional crypto derivatives analyst.

Market regime has been pre-calculated for you. Your job is to explain it clearly.

SYMBOL: %s
MARKET REGIME: %s (Confidence: %d%%)
OI TREND: %s
LEVERAGE IMBALANCE: %s
SHORT SQUEEZE PROBABILITY: %d%%
LONG SQUEEZE PROBABILITY: %d%%
%s

RAW DATA:
Funding Rate: %.4f%%
Open Interest: $%.2fM (1h: %.1f%%, 24h: %.1f%%)
Long/Short: %.1f%% longs

In 2-3 sentences, explain:
1. What the market regime means for traders right now
2. Which side is more at risk (longs or shorts)
3. What traders should watch for

Be direct and actionable. Never predict exact price.

Respond ONLY with a valid JSON object, no markdown, no explanation:
{
  "summary": "2-3 sentence plain English analysis",
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

func (a *Analyzer) Analyze(ctx context.Context, snap models.MarketSnapshot, sigs models.MarketSignals, tier string) (models.AIAnalysis, error) {
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

	reqBody := anthropicRequest{
		Model:     "claude-haiku-4-5-20251001",
		MaxTokens: 1024,
		System:    "You are a crypto derivatives analyst. Always respond with valid JSON only.",
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
	req.Header.Set("x-api-key", a.apiKey)
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
