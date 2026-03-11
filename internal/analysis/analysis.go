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

// largestLevel returns the liquidation level with the highest SizeUsd for the given side.
func largestLevel(levels []models.LiquidationLevel, side string) models.LiquidationLevel {
	var best models.LiquidationLevel
	for _, l := range levels {
		if l.Side == side && l.SizeUsd > best.SizeUsd {
			best = l
		}
	}
	return best
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

// buildPrompt assembles the user-turn prompt from live snapshot data.
func buildPrompt(snap models.MarketSnapshot) string {
	fr := snap.FundingRate
	oi := snap.OpenInterest

	nextFunding := time.UnixMilli(fr.NextFundingTime).UTC().Format("15:04 UTC")

	longLiq := largestLevel(snap.LiquidationMap.Levels, "long")
	shortLiq := largestLevel(snap.LiquidationMap.Levels, "short")

	var longPct, shortPct float64
	if len(snap.LongShortRatios) > 0 {
		longPct = snap.LongShortRatios[0].LongPct
		shortPct = snap.LongShortRatios[0].ShortPct
	}

	return fmt.Sprintf(
		`You are a crypto derivatives market analyst. Analyze the following market data and provide a structured assessment.

Symbol: %s
Funding Rate: %.4f%% (next funding: %s)
Open Interest: $%.2f (1h: %.2f%%, 4h: %.2f%%, 24h: %.2f%%)
Liquidation Clusters:
  - Largest long liquidation zone: $%.2f ($%.2fM)
  - Largest short liquidation zone: $%.2f ($%.2fM)
Long/Short Ratio (Binance): %.1f%% longs vs %.1f%% shorts

Respond ONLY with a valid JSON object, no markdown, no explanation:
{
  "summary": "2-3 sentence plain English analysis of current positioning",
  "sentiment": "bullish" | "neutral" | "bearish",
  "confidence": 0-100
}`,
		snap.Symbol,
		fr.Rate*100, nextFunding,
		oi.OIUsd, oi.OIChange1h, oi.OIChange4h, oi.OIChange24h,
		longLiq.Price, longLiq.SizeUsd/1_000_000,
		shortLiq.Price, shortLiq.SizeUsd/1_000_000,
		longPct, shortPct,
	)
}

// ─── Public ───────────────────────────────────────────────────────────────────

func (a *Analyzer) Analyze(ctx context.Context, snap models.MarketSnapshot, tier string) (models.AIAnalysis, error) {
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

	prompt := buildPrompt(snap)
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
