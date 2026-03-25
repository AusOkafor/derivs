package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/getsentry/sentry-go"
	"derivs-backend/internal/analysis"
	"derivs-backend/internal/billing"
	"derivs-backend/internal/models"
	"derivs-backend/internal/signals"
	"derivs-backend/internal/snooze"
	"derivs-backend/internal/supabase"
)

// snoozeGlobal is a package-level alias for cleaner handler code.
var snoozeGlobal = snooze.Global

// snoozeParseDuration wraps snooze.ParseDuration.
func snoozeParseDuration(s string) (time.Duration, bool) { return snooze.ParseDuration(s) }

// snoozeFormatRemaining wraps snooze.FormatRemaining.
func snoozeFormatRemaining(t time.Time) string { return snooze.FormatRemaining(t) }

func validateUsername(u string) error {
	if len(u) == 0 || len(u) > 32 {
		return fmt.Errorf("invalid username length")
	}
	for _, c := range u {
		if !unicode.IsLetter(c) && !unicode.IsDigit(c) && c != '_' {
			return fmt.Errorf("invalid username character: %c", c)
		}
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}

// GetSnapshot handles GET /api/snapshot?symbol=BTC&username=johndoe
// username is optional — if provided, fetches tier from Supabase and only runs AI for pro tier.
// When username is provided, cache is bypassed to serve tier-specific AI.
func (h *Handler) GetSnapshot(w http.ResponseWriter, r *http.Request) {
	symbol := r.URL.Query().Get("symbol")
	if symbol == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "symbol query parameter is required"})
		return
	}
	username := r.URL.Query().Get("username")
	if username != "" {
		if err := validateUsername(username); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid username"})
			return
		}
	}

	tier := ""
	var userAPIKey, preferredModel string
	if username != "" {
		var err error
		tier, _, err = h.db.GetSubscriberTier(r.Context(), username)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			log.Printf("GetSnapshot: GetSubscriberTier(%s): %v", username, err)
			sentry.CaptureException(err)
		}
		if tier == "" {
			tier = "free"
		}
		settings, _ := h.db.GetUserSettings(r.Context(), username)
		if settings != nil {
			userAPIKey = settings.AnthropicAPIKey
			preferredModel = settings.PreferredModel
		}
	}

	// Bypass cache when username provided (tier-specific AI)
	if username == "" {
		if cached, ok := h.cache.Get(symbol); ok {
			if cached.Snapshot.Symbol != symbol {
				log.Printf("GetSnapshot: cache symbol mismatch: requested %s, got %s", symbol, cached.Snapshot.Symbol)
			}
			// Merge fresh RecentLiquidations (cache may have been populated by GetTickers without it)
			if h.liqFeed != nil {
				recent := h.liqFeed.GetRecent(symbol)
				burst, burstSize := h.liqFeed.GetBurst(symbol)
				cached.Snapshot.RecentLiquidations = &models.RecentLiquidations{
					TotalLongUSD:  recent.TotalLong,
					TotalShortUSD: recent.TotalShort,
					BurstDetected: burst,
					BurstSizeUSD:  burstSize,
					Window:        "5m",
				}
			}
			// Attach market Fear & Greed if missing (e.g. from older cache)
			if cached.FearGreed.MarketFearGreed == nil {
				if marketFG, err := h.calc.GetMarketIndex(); err == nil {
					cached.FearGreed.MarketFearGreed = &models.MarketFearGreed{
						Value: marketFG.Value,
						Label: marketFG.Label,
					}
				}
			}
			w.Header().Set("X-Cache", "HIT")
			writeJSON(w, http.StatusOK, cached)
			return
		}
	}

	ctx := r.Context()

	snap, err := h.aggregator.FetchSnapshot(ctx, symbol)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		sentry.CaptureException(err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if snap.Symbol != symbol {
		log.Printf("GetSnapshot: snapshot symbol mismatch: requested %s, got %s", symbol, snap.Symbol)
	}
	h.cache.RecordPrice(symbol, snap.LiquidationMap.CurrentPrice)

	if h.liqFeed != nil {
		recent := h.liqFeed.GetRecent(symbol)
		burst, burstSize := h.liqFeed.GetBurst(symbol)
		snap.RecentLiquidations = &models.RecentLiquidations{
			TotalLongUSD:  recent.TotalLong,
			TotalShortUSD: recent.TotalShort,
			BurstDetected: burst,
			BurstSizeUSD:  burstSize,
			Window:        "5m",
		}
	}

	engine := signals.New()
	momentum := h.cache.GetPriceMomentum(symbol)
	sigs := engine.Analyze(snap, momentum)

	// Check AI cache before calling Claude — pro users share one result per symbol
	// refreshed every 5 minutes, so rapid page refreshes don't burn tokens.
	const aiCacheTTL = 5 * time.Minute
	var ai models.AIAnalysis
	if tier == "pro" {
		h.aiCacheMu.Lock()
		entry, hit := h.aiCache[symbol]
		h.aiCacheMu.Unlock()
		if hit && time.Now().Before(entry.expiresAt) {
			ai = entry.result
		}
	}
	if ai.GeneratedAt.IsZero() {
		var err error
		ai, err = h.analyzer.Analyze(ctx, snap, sigs, tier, userAPIKey, preferredModel)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			sentry.CaptureException(err)
			ai = models.AIAnalysis{Symbol: symbol, Summary: "Analysis temporarily unavailable", Sentiment: "neutral", Confidence: 0, GeneratedAt: time.Now().UTC()}
		}
		if tier == "pro" && ai.Summary != "Analysis temporarily unavailable" {
			h.aiCacheMu.Lock()
			h.aiCache[symbol] = aiCacheEntry{result: ai, expiresAt: time.Now().Add(aiCacheTTL)}
			h.aiCacheMu.Unlock()
		}
	}
	if ai.GeneratedAt.IsZero() {
		ai = models.AIAnalysis{
			Symbol:      symbol,
			Summary:     "Upgrade to Pro to unlock AI-powered market analysis.",
			Sentiment:   "neutral",
			Confidence:  0,
			GeneratedAt: time.Now().UTC(),
		}
	}

	fg := h.calc.Calculate(snap)
	if marketFG, err := h.calc.GetMarketIndex(); err == nil {
		fg.MarketFearGreed = &models.MarketFearGreed{
			Value: marketFG.Value,
			Label: marketFG.Label,
		}
	}

	rawAlerts := h.detector.Analyze(snap, sigs)
	result := models.SnapshotWithAnalysis{
		Snapshot:  snap,
		Analysis:  ai,
		Alerts:    h.worker.ProcessAlerts(rawAlerts),
		FearGreed: fg,
		Signals:   sigs,
	}

	if username == "" {
		h.cache.Set(symbol, result)
	}

	w.Header().Set("X-Cache", "MISS")
	writeJSON(w, http.StatusOK, result)
}

// WaitlistRequest is the request body for POST /api/waitlist
type WaitlistRequest struct {
	Email    string `json:"email"`
	Tier     string `json:"tier"`
	Username string `json:"username"`
}

// JoinWaitlist handles POST /api/waitlist
func (h *Handler) JoinWaitlist(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req WaitlistRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	if req.Email == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "email required"})
		return
	}
	if req.Tier == "" {
		req.Tier = "pro"
	}

	if err := h.db.AddToWaitlist(r.Context(), req.Email, req.Tier, req.Username); err != nil {
		if strings.Contains(err.Error(), "duplicate") {
			writeJSON(w, http.StatusOK, map[string]string{
				"status":  "already_registered",
				"message": "You're already on the waitlist!",
			})
			return
		}
		log.Printf("JoinWaitlist: AddToWaitlist: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to join waitlist"})
		return
	}

	usernameDisplay := "(not provided)"
	if req.Username != "" {
		usernameDisplay = "@" + strings.TrimPrefix(req.Username, "@")
	}
	go func() {
		_ = h.notifier.SendToAdmin(fmt.Sprintf(
			"🎯 New Waitlist Signup!\n\nEmail: %s\nTier: %s\nUsername: %s",
			req.Email, req.Tier, usernameDisplay,
		))
	}()

	writeJSON(w, http.StatusOK, map[string]string{
		"status":  "success",
		"message": "You're on the waitlist!",
	})
}

// Health handles GET /api/health
func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {
	exchangeStatus := map[string]string{
		"bybit":   h.aggregator.ExchangeStatus("bybit"),
		"binance": h.aggregator.ExchangeStatus("binance"),
		"okx":     h.aggregator.ExchangeStatus("okx"),
	}
	status := "ok"
	for _, s := range exchangeStatus {
		if s != "ok" {
			status = "degraded"
			break
		}
	}
	health := map[string]interface{}{
		"status":          status,
		"uptime":          time.Since(h.startTime).Round(time.Second).String(),
		"timestamp":       time.Now().UTC(),
		"worker_running":  h.worker != nil && h.worker.IsRunning(),
		"ai_enabled":      analysis.IsAIEnabled(),
		"cache_size":      h.cache.Size(),
		"exchange_status": exchangeStatus,
		"supabase":        h.db.Ping(),
		"last_fetch":      h.cache.LastFetchTime(),
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(health) //nolint:errcheck
}

// GetHistory handles GET /api/history?symbol=BTC
// Returns HistoricalData with the last 200 funding rate points (~66 days at 8h intervals) and OI candles.
func (h *Handler) GetHistory(w http.ResponseWriter, r *http.Request) {
	symbol := r.URL.Query().Get("symbol")
	if symbol == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "symbol query parameter is required"})
		return
	}

	ctx := r.Context()

	var fundingHistory []models.FundingRatePoint
	var oiHistory []models.OICandle
	var wg sync.WaitGroup
	var fundingErr, oiErr error

	wg.Add(2)
	go func() {
		defer wg.Done()
		fundingHistory, fundingErr = h.aggregator.FetchFundingHistory(ctx, symbol, 200)
	}()
	go func() {
		defer wg.Done()
		oiHistory, oiErr = h.aggregator.FetchOIHistory(ctx, symbol, 48)
	}()
	wg.Wait()

	if fundingErr != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": fundingErr.Error()})
		return
	}
	if oiErr != nil {
		log.Printf("GetHistory: FetchOIHistory: %v", oiErr)
		oiHistory = nil
	}

	writeJSON(w, http.StatusOK, models.HistoricalData{
		Symbol:         symbol,
		FundingHistory: fundingHistory,
		OIHistory:      oiHistory,
		Timestamp:      time.Now().UTC(),
	})
}

// GetAlertHistory handles GET /api/alerts/history?symbol=BTC&limit=50
// symbol is optional — if empty, returns all symbols
// limit defaults to 50, max 200
// Returns []AlertHistoryEntry sorted by triggered_at DESC
func (h *Handler) GetAlertHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	symbol := r.URL.Query().Get("symbol")
	limitStr := r.URL.Query().Get("limit")
	limit := 50
	if limitStr != "" {
		if n, err := strconv.Atoi(limitStr); err == nil && n > 0 {
			limit = n
			if limit > 200 {
				limit = 200
			}
		}
	}
	entries, err := h.db.GetAlertHistory(r.Context(), symbol, limit)
	if err != nil {
		log.Printf("GetAlertHistory: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to get alert history"})
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

// GetAlerts handles GET /api/alerts?symbol=BTC
// Fetches a fresh snapshot and runs alert detection. Does not use the cache
// so that callers always get current anomaly detection.
func (h *Handler) GetAlerts(w http.ResponseWriter, r *http.Request) {
	symbol := r.URL.Query().Get("symbol")
	if symbol == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "symbol query parameter is required"})
		return
	}

	ctx := r.Context()

	snap, err := h.aggregator.FetchSnapshot(ctx, symbol)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		sentry.CaptureException(err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	h.cache.RecordPrice(symbol, snap.LiquidationMap.CurrentPrice)

	engine := signals.New()
	momentum := h.cache.GetPriceMomentum(symbol)
	sigs := engine.Analyze(snap, momentum)
	rawAlerts := h.detector.Analyze(snap, sigs)
	writeJSON(w, http.StatusOK, h.worker.ProcessAlerts(rawAlerts))
}

// GetTickers handles GET /api/tickers?symbols=BTC,ETH,SOL,ARB,DOGE,AVAX
// Fetches snapshot for each symbol, runs signal engine, and returns []models.TickerResult.
func (h *Handler) GetTickers(w http.ResponseWriter, r *http.Request) {
	symbolsParam := r.URL.Query().Get("symbols")
	if symbolsParam == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "symbols query parameter is required"})
		return
	}

	symbols := strings.Split(symbolsParam, ",")
	ctx := r.Context()

	results := make([]models.TickerResult, len(symbols))
	var mu sync.Mutex
	var wg sync.WaitGroup
	engine := signals.New()

	for i, sym := range symbols {
		wg.Add(1)
		go func(idx int, symbol string) {
			defer wg.Done()
			symbol = strings.TrimSpace(symbol)
			snap, err := h.aggregator.FetchSnapshot(ctx, symbol)
			if err != nil {
				if errors.Is(err, context.Canceled) {
					return
				}
				sentry.CaptureException(err)
				log.Printf("tickers FetchSnapshot %s: %v", symbol, err)
				return
			}
			h.cache.RecordPrice(symbol, snap.LiquidationMap.CurrentPrice)
			if h.liqFeed != nil {
				recent := h.liqFeed.GetRecent(symbol)
				burst, burstSize := h.liqFeed.GetBurst(symbol)
				snap.RecentLiquidations = &models.RecentLiquidations{
					TotalLongUSD:  recent.TotalLong,
					TotalShortUSD: recent.TotalShort,
					BurstDetected: burst,
					BurstSizeUSD:  burstSize,
					Window:        "5m",
				}
			}
			price, change24h, tickErr := h.aggregator.FetchTicker(ctx, symbol)
			if tickErr != nil {
				price = snap.LiquidationMap.CurrentPrice
			}
			momentum := h.cache.GetPriceMomentum(symbol)
			sigs := engine.Analyze(snap, momentum)
			fg := h.calc.Calculate(snap)
			if marketFG, err := h.calc.GetMarketIndex(); err == nil {
				fg.MarketFearGreed = &models.MarketFearGreed{
					Value: marketFG.Value,
					Label: marketFG.Label,
				}
			}
			// Populate cache so Size() reflects usage
			rawAlerts := h.detector.Analyze(snap, sigs)
			h.cache.Set(symbol, models.SnapshotWithAnalysis{
				Snapshot:  snap,
				Analysis:  models.AIAnalysis{},
				Alerts:    h.worker.ProcessAlerts(rawAlerts),
				FearGreed: fg,
				Signals:   sigs,
			})
			mu.Lock()
			results[idx] = models.TickerResult{
				Symbol:    symbol,
				Snapshot:  snap,
				Signals:   sigs,
				FearGreed: fg,
				Price:     price,
				Change24h: change24h,
				Timestamp: time.Now().UTC(),
			}
			mu.Unlock()
		}(i, sym)
	}

	wg.Wait()

	tickers := make([]models.TickerResult, 0, len(results))
	for _, t := range results {
		if t.Symbol != "" {
			tickers = append(tickers, t)
		}
	}

	writeJSON(w, http.StatusOK, tickers)
}

// ─── Subscription endpoints ───────────────────────────────────────────────────

type subscribeRequest struct {
	TelegramUser struct {
		ID        int64  `json:"id"`
		FirstName string `json:"first_name"`
		Username  string `json:"username"`
		PhotoURL  string `json:"photo_url"`
		AuthDate  int64  `json:"auth_date"`
		Hash      string `json:"hash"`
	} `json:"telegram_user"`
	Symbols []string        `json:"symbols"`
	Rules   json.RawMessage `json:"rules"`
}

// Subscribe handles POST /api/subscribe.
func (h *Handler) Subscribe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	var req subscribeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	if req.TelegramUser.Hash == "" || len(req.Symbols) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "telegram_user and symbols are required"})
		return
	}

	ctx := r.Context()

	isManual := req.TelegramUser.Hash == "manual"

	if !isManual {
		if req.TelegramUser.ID == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "telegram_user and symbols are required"})
			return
		}
		// Build data map for hash verification (all fields except hash, as strings)
		data := map[string]string{
			"auth_date":  strconv.FormatInt(req.TelegramUser.AuthDate, 10),
			"first_name": req.TelegramUser.FirstName,
			"hash":       req.TelegramUser.Hash,
			"id":         strconv.FormatInt(req.TelegramUser.ID, 10),
		}
		if req.TelegramUser.Username != "" {
			data["username"] = req.TelegramUser.Username
		}
		if req.TelegramUser.PhotoURL != "" {
			data["photo_url"] = req.TelegramUser.PhotoURL
		}
		if !h.notifier.VerifyAuth(data) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid telegram auth"})
			return
		}
	}

	// Use username from Telegram; for manual, FirstName holds the entered username
	telegramUsername := req.TelegramUser.Username
	if telegramUsername == "" {
		telegramUsername = req.TelegramUser.FirstName
	}
	if telegramUsername == "" && !isManual {
		telegramUsername = "user_" + strconv.FormatInt(req.TelegramUser.ID, 10)
	}
	if telegramUsername == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "telegram username is required"})
		return
	}

	chatID := req.TelegramUser.ID
	if isManual {
		chatID = 0 // default: rely on /start webhook to populate later
		existingChatID, err := h.db.GetSubscriberChatID(ctx, telegramUsername)
		if err == nil && existingChatID != 0 {
			chatID = existingChatID // preserve the real chat_id
		}
	}

	sub := supabase.Subscriber{
		TelegramUsername: telegramUsername,
		ChatID:           chatID,
		Symbols:          req.Symbols,
		Rules:            req.Rules,
		Active:           true,
	}

	if err := h.db.CreateSubscriber(ctx, sub); err != nil {
		if strings.Contains(err.Error(), "409") || strings.Contains(err.Error(), "duplicate") || strings.Contains(err.Error(), "unique") {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "already subscribed"})
			return
		}
		log.Printf("subscribe: CreateSubscriber: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to create subscription"})
		return
	}

	// Notify admin of new subscriber
	go func() {
		tier, _, _ := h.db.GetSubscriberTier(context.Background(), telegramUsername)
		if tier == "" {
			tier = "free"
		}
		msg := fmt.Sprintf(
			"🎉 New Subscriber!\n\n"+
				"Username: @%s\n"+
				"Tier: %s\n"+
				"Symbols: %v\n"+
				"Time: %s UTC",
			telegramUsername,
			tier,
			req.Symbols,
			time.Now().UTC().Format("2006-01-02 15:04:05"),
		)
		if err := h.notifier.SendToAdmin(msg); err != nil {
			log.Printf("subscribe: SendToAdmin: %v", err)
		}
	}()

	if isManual {
		// Manual signup: no chat_id yet — user must send /start to bot
		writeJSON(w, http.StatusCreated, map[string]string{
			"status":  "pending",
			"message": fmt.Sprintf("Almost done! Send /start to @derivlens_alerts_bot to activate your alerts, %s.", req.TelegramUser.FirstName),
		})
		return
	}

	// Widget signup: send welcome message immediately
	welcomeMsg := fmt.Sprintf(
		"✅ <b>DerivLens Alerts Activated!</b>\nHello %s! You'll receive alerts for: %s\nPowered by DerivLens 🚀",
		req.TelegramUser.FirstName,
		strings.Join(req.Symbols, ", "),
	)
	if err := h.notifier.SendMessage(context.Background(), req.TelegramUser.ID, welcomeMsg); err != nil {
		log.Printf("subscribe: SendMessage: %v", err)
	}

	writeJSON(w, http.StatusCreated, map[string]string{
		"status":  "activated",
		"message": "Alerts activated! Check your Telegram for confirmation.",
	})
}

// Unsubscribe handles DELETE /api/unsubscribe.
func (h *Handler) Unsubscribe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	var req struct {
		TelegramUsername string `json:"telegram_username"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.TelegramUsername == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "telegram_username is required"})
		return
	}

	if err := h.db.DeleteSubscriber(r.Context(), req.TelegramUsername); err != nil {
		log.Printf("unsubscribe: DeleteSubscriber(%s): %v", req.TelegramUsername, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to unsubscribe"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "unsubscribed"})
}

// ─── Settings ──────────────────────────────────────────────────────────────────

// Settings routes GET and POST /api/settings to the appropriate handler.
func (h *Handler) Settings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.GetSettings(w, r)
	case http.MethodPost:
		h.SaveSettings(w, r)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

// GetSettings handles GET /api/settings?username=xxx
func (h *Handler) GetSettings(w http.ResponseWriter, r *http.Request) {
	username := r.URL.Query().Get("username")
	if username == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "username query parameter is required"})
		return
	}
	if err := validateUsername(username); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid username"})
		return
	}
	settings, err := h.db.GetUserSettings(r.Context(), username)
	if err != nil {
		log.Printf("GetSettings: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to get settings"})
		return
	}
	if settings == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"username":             username,
			"anthropic_api_key_set": false,
			"preferred_model":      "claude-haiku-4-5-20251001",
		})
		return
	}
	// Don't return the raw API key to the client; indicate if one is set
	hasKey := settings.AnthropicAPIKey != ""
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"username":             settings.Username,
		"anthropic_api_key_set": hasKey,
		"preferred_model":      settings.PreferredModel,
	})
}

// SaveSettings handles POST /api/settings
func (h *Handler) SaveSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username        string `json:"username"`
		AnthropicAPIKey string `json:"anthropic_api_key"`
		PreferredModel  string `json:"preferred_model"`
		ClearAPIKey     bool   `json:"clear_api_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Username == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "username is required"})
		return
	}
	settings := supabase.UserSettings{
		Username:       req.Username,
		PreferredModel: req.PreferredModel,
	}
	if settings.PreferredModel == "" {
		settings.PreferredModel = "claude-haiku-4-5-20251001"
	}
	if req.ClearAPIKey {
		settings.AnthropicAPIKey = ""
	} else if req.AnthropicAPIKey != "" {
		settings.AnthropicAPIKey = req.AnthropicAPIKey
	} else {
		// Preserve existing key when not provided
		existing, _ := h.db.GetUserSettings(r.Context(), req.Username)
		if existing != nil {
			settings.AnthropicAPIKey = existing.AnthropicAPIKey
		}
	}
	if err := h.db.SaveUserSettings(r.Context(), settings); err != nil {
		log.Printf("SaveSettings: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save settings"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}

// ─── Billing ───────────────────────────────────────────────────────────────────

// CreateCheckout handles POST /api/billing/checkout.
// Body: {"telegram_username": "johndoe", "plan": "basic"|"pro"}
// Creates Stripe checkout session, returns {"checkout_url": "https://checkout.stripe.com/..."}
func (h *Handler) CreateCheckout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	var req struct {
		TelegramUsername string `json:"telegram_username"`
		Plan             string `json:"plan"` // "basic" or "pro"
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.TelegramUsername == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "telegram_username is required"})
		return
	}

	if h.billing == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "billing not configured"})
		return
	}

	priceID := h.stripePriceIDPro
	plan := "pro"
	if req.Plan == "basic" && h.stripePriceIDBasic != "" {
		priceID = h.stripePriceIDBasic
		plan = "basic"
	}
	if priceID == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "billing not configured for selected plan"})
		return
	}

	url, err := h.billing.CreateCheckoutSession(req.TelegramUsername, priceID, plan)
	if err != nil {
		log.Printf("billing: CreateCheckoutSession: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"checkout_url": url})
}

// LemonSqueezyCheckout handles GET /api/billing/lemonsqueezy/checkout?username=johndoe&tier=basic|pro
// Creates Lemon Squeezy checkout, returns {"checkout_url": "https://..."}
func (h *Handler) LemonSqueezyCheckout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	username := strings.TrimPrefix(strings.TrimSpace(r.URL.Query().Get("username")), "@")
	tier := r.URL.Query().Get("tier")
	if username == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "username query parameter is required"})
		return
	}
	if err := validateUsername(username); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid username"})
		return
	}
	if tier == "" {
		tier = "pro"
	}
	if tier != "basic" && tier != "pro" {
		tier = "pro"
	}

	if h.lemonSqueezy == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Lemon Squeezy not configured"})
		return
	}

	url, err := h.lemonSqueezy.CreateCheckout(username, tier)
	if err != nil {
		log.Printf("billing: LemonSqueezy CreateCheckout: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"checkout_url": url})
}

// LemonSqueezyWebhook handles POST /api/billing/lemonsqueezy/webhook.
func (h *Handler) LemonSqueezyWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	if h.lemonSqueezy == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}

	payload, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("billing: Lemon Squeezy webhook read body: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	sigHeader := r.Header.Get("X-Signature")
	if sigHeader == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	err = h.lemonSqueezy.HandleWebhook(payload, sigHeader, func(u billing.WebhookUpdate) {
		switch u.EventType {
		case "order_created", "subscription_created":
			if u.TelegramUsername != "" {
				if err := h.db.UpdateSubscriberTier(ctx, u.TelegramUsername, u.Tier, u.CustomerID, u.SubscriptionID, u.Status); err != nil {
					sentry.CaptureException(err)
					log.Printf("billing: Lemon Squeezy UpdateSubscriberTier(%s): %v", u.TelegramUsername, err)
				}
			}
		case "subscription_updated":
			if err := h.db.UpdateSubscriberTierByStripeID(ctx, u.CustomerID, u.SubscriptionID, "", u.Status); err != nil {
				sentry.CaptureException(err)
				log.Printf("billing: Lemon Squeezy UpdateSubscriberTierByStripeID: %v", err)
			}
		case "subscription_expired":
			if err := h.db.UpdateSubscriberTierByStripeID(ctx, u.CustomerID, u.SubscriptionID, "free", u.Status); err != nil {
				sentry.CaptureException(err)
				log.Printf("billing: Lemon Squeezy UpdateSubscriberTierByStripeID: %v", err)
			}
		}
	})

	if err != nil {
		log.Printf("billing: Lemon Squeezy webhook: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	w.WriteHeader(http.StatusOK)
}

// StripeWebhook handles POST /api/billing/webhook.
// Stripe webhook handler — verifies signature, processes events, updates Supabase.
func (h *Handler) StripeWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	if h.billing == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}

	payload, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("billing: webhook read body: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	sigHeader := r.Header.Get("Stripe-Signature")
	if sigHeader == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	err = h.billing.HandleWebhook(payload, sigHeader, func(u billing.WebhookUpdate) {
		switch u.EventType {
		case "checkout.session.completed":
			if u.TelegramUsername != "" {
				if err := h.db.UpdateSubscriberTier(ctx, u.TelegramUsername, u.Tier, u.CustomerID, u.SubscriptionID, u.Status); err != nil {
					sentry.CaptureException(err)
					log.Printf("billing: UpdateSubscriberTier(%s): %v", u.TelegramUsername, err)
				}
			}
		case "customer.subscription.deleted":
			if err := h.db.UpdateSubscriberTierByStripeID(ctx, u.CustomerID, u.SubscriptionID, u.Tier, u.Status); err != nil {
				sentry.CaptureException(err)
				log.Printf("billing: UpdateSubscriberTierByStripeID: %v", err)
			}
		case "customer.subscription.updated":
			if err := h.db.UpdateSubscriberTierByStripeID(ctx, u.CustomerID, u.SubscriptionID, "", u.Status); err != nil {
				sentry.CaptureException(err)
				log.Printf("billing: UpdateSubscriberTierByStripeID: %v", err)
			}
		}
	})

	if err != nil {
		log.Printf("billing: webhook: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	w.WriteHeader(http.StatusOK)
}

// CreatePortal handles POST /api/billing/portal.
// Body: {"username": "austinokwy"}
// Returns {"url": "https://billing.stripe.com/..."}
func (h *Handler) CreatePortal(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	var req struct {
		Username string `json:"username"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Username == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "username is required"})
		return
	}
	username := strings.TrimPrefix(strings.TrimSpace(req.Username), "@")

	if h.billing == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "billing not configured"})
		return
	}

	customerID, err := h.db.GetSubscriberStripeCustomerID(r.Context(), username)
	if err != nil {
		sentry.CaptureException(err)
		log.Printf("billing: GetSubscriberStripeCustomerID(%s): %v", username, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to get customer"})
		return
	}
	if customerID == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no active subscription found"})
		return
	}

	url, err := h.billing.CreatePortalSession(customerID, "https://derivlens.io/dashboard")
	if err != nil {
		log.Printf("billing: CreatePortalSession: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"url": url})
}

// GetBillingStatus handles GET /api/billing/status?username=johndoe.
// Returns {"tier": "free"|"pro", "status": "active"|"inactive"}
func (h *Handler) GetBillingStatus(w http.ResponseWriter, r *http.Request) {
	username := r.URL.Query().Get("username")
	if username == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "username query parameter is required"})
		return
	}
	if err := validateUsername(username); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid username"})
		return
	}

	tier, status, err := h.db.GetSubscriberTier(r.Context(), username)
	if err != nil {
		sentry.CaptureException(err)
		log.Printf("billing: GetSubscriberTier(%s): %v", username, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to get tier"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"tier": tier, "status": status})
}

// ─── Telegram webhook ─────────────────────────────────────────────────────────

// telegramUpdate is the minimal subset of a Telegram Update object we care about.
type telegramUpdate struct {
	Message struct {
		Chat struct {
			ID int64 `json:"id"`
		} `json:"chat"`
		From struct {
			Username string `json:"username"`
		} `json:"from"`
		Text string `json:"text"`
	} `json:"message"`
	CallbackQuery struct {
		ID   string `json:"id"`
		From struct {
			Username string `json:"username"`
		} `json:"from"`
		Message struct {
			Chat struct {
				ID int64 `json:"id"`
			} `json:"chat"`
		} `json:"message"`
		Data string `json:"data"` // e.g. "snooze:BTC:1h"
	} `json:"callback_query"`
}

// TelegramWebhook handles POST /api/webhook/telegram.
// Telegram requires a 200 response for every update, even on errors.
func (h *Handler) TelegramWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusOK)
		return
	}

	var update telegramUpdate
	if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
		log.Printf("telegram webhook: decode: %v", err)
		w.WriteHeader(http.StatusOK)
		return
	}

	ctx := r.Context()

	// Handle inline button callbacks (snooze button pressed on an alert message)
	if cq := update.CallbackQuery; cq.ID != "" {
		h.handleSnoozeCallback(ctx, cq.ID, cq.Message.Chat.ID, cq.From.Username, cq.Data)
		w.WriteHeader(http.StatusOK)
		return
	}

	msg := update.Message
	chatID := msg.Chat.ID
	username := msg.From.Username
	text := strings.TrimSpace(msg.Text)

	switch {
	case text == "/start":
		if username != "" {
			if err := h.db.UpdateChatID(ctx, username, chatID); err != nil {
				log.Printf("telegram webhook: UpdateChatID(%s, %d): %v", username, chatID, err)
			}
		}
		dashboardLink := fmt.Sprintf("https://derivlens.io/dashboard?u=%s", username)
		if username == "" {
			dashboardLink = fmt.Sprintf("https://derivlens.io/dashboard?uid=%d", chatID)
		}
		welcomeMsg := fmt.Sprintf(
			"👋 Welcome to DerivLens!\n\n📊 <b>Your dashboard:</b>\n<a href=\"%s\">%s</a>\n\n"+
				"⭐ Bookmark this link — it keeps your account connected on any device.\n\n"+
				"Your alerts are now active. You'll receive notifications here automatically.\n\n"+
				"💤 <b>Snooze commands:</b>\n"+
				"/snooze BTC 1h — pause BTC alerts for 1h\n"+
				"/snooze all 4h — pause all alerts for 4h\n"+
				"/unsnooze BTC — resume BTC alerts\n"+
				"/snoozes — list active snoozes",
			dashboardLink, dashboardLink,
		)
		if err := h.notifier.SendMessage(ctx, chatID, welcomeMsg); err != nil {
			log.Printf("telegram webhook: SendMessage(%d): %v", chatID, err)
		}

	case strings.HasPrefix(text, "/snooze "):
		h.handleSnoozeCommand(ctx, chatID, username, strings.TrimPrefix(text, "/snooze "))

	case strings.HasPrefix(text, "/unsnooze"):
		h.handleUnsnoozeCommand(ctx, chatID, username, strings.TrimPrefix(text, "/unsnooze"))

	case text == "/snoozes":
		h.handleSnoozeListCommand(ctx, chatID, username)
	}

	w.WriteHeader(http.StatusOK)
}

// handleSnoozeCommand parses "/snooze BTC 1h" args and applies the snooze.
func (h *Handler) handleSnoozeCommand(ctx context.Context, chatID int64, username, args string) {
	parts := strings.Fields(args)
	if len(parts) < 2 {
		h.notifier.SendMessage(ctx, chatID, "Usage: /snooze BTC 1h\nDurations: 30m · 1h · 4h · 24h\nUse 'all' to snooze every symbol.") //nolint:errcheck
		return
	}
	symbol := strings.ToUpper(parts[0])
	d, ok := snoozeParseDuration(parts[1])
	if !ok {
		h.notifier.SendMessage(ctx, chatID, "Valid durations: 30m · 1h · 4h · 24h") //nolint:errcheck
		return
	}
	subID := h.resolveSubscriberID(ctx, chatID, username)
	if subID == "" {
		h.notifier.SendMessage(ctx, chatID, "You're not subscribed. Send /start to register.") //nolint:errcheck
		return
	}
	snoozeGlobal.Snooze(subID, symbol, d)
	label := symbol
	if symbol == "ALL" {
		label = "all symbols"
	}
	h.notifier.SendMessage(ctx, chatID, fmt.Sprintf("😴 %s alerts snoozed for %s.\nSend /unsnooze %s to resume early.", label, parts[1], symbol)) //nolint:errcheck
}

// handleUnsnoozeCommand cancels a snooze for a symbol.
func (h *Handler) handleUnsnoozeCommand(ctx context.Context, chatID int64, username, args string) {
	symbol := strings.ToUpper(strings.TrimSpace(args))
	if symbol == "" {
		h.notifier.SendMessage(ctx, chatID, "Usage: /unsnooze BTC") //nolint:errcheck
		return
	}
	subID := h.resolveSubscriberID(ctx, chatID, username)
	if subID == "" {
		h.notifier.SendMessage(ctx, chatID, "You're not subscribed.") //nolint:errcheck
		return
	}
	snoozeGlobal.Unsnooze(subID, symbol)
	h.notifier.SendMessage(ctx, chatID, fmt.Sprintf("✅ %s alerts resumed.", symbol)) //nolint:errcheck
}

// handleSnoozeListCommand shows all active snoozes.
func (h *Handler) handleSnoozeListCommand(ctx context.Context, chatID int64, username string) {
	subID := h.resolveSubscriberID(ctx, chatID, username)
	if subID == "" {
		h.notifier.SendMessage(ctx, chatID, "You're not subscribed.") //nolint:errcheck
		return
	}
	active := snoozeGlobal.List(subID)
	if len(active) == 0 {
		h.notifier.SendMessage(ctx, chatID, "No active snoozes. All alerts are enabled.") //nolint:errcheck
		return
	}
	var sb strings.Builder
	sb.WriteString("😴 <b>Active snoozes:</b>\n")
	for sym, exp := range active {
		sb.WriteString(fmt.Sprintf("• %s — %s remaining\n", sym, snoozeFormatRemaining(exp)))
	}
	sb.WriteString("\nSend /unsnooze SYMBOL to resume early.")
	h.notifier.SendMessage(ctx, chatID, sb.String()) //nolint:errcheck
}

// handleSnoozeCallback handles the inline "😴 Snooze 1h" button press.
func (h *Handler) handleSnoozeCallback(ctx context.Context, callbackQueryID string, chatID int64, username, data string) {
	// data format: "snooze:BTC:1h"
	parts := strings.Split(data, ":")
	if len(parts) != 3 || parts[0] != "snooze" {
		return
	}
	symbol := strings.ToUpper(parts[1])
	d, ok := snoozeParseDuration(parts[2])
	if !ok {
		return
	}
	subID := h.resolveSubscriberID(ctx, chatID, username)
	if subID == "" {
		h.notifier.AnswerCallbackQuery(callbackQueryID, "Subscribe first to use snooze.") //nolint:errcheck
		return
	}
	snoozeGlobal.Snooze(subID, symbol, d)
	h.notifier.AnswerCallbackQuery(callbackQueryID, fmt.Sprintf("😴 %s alerts snoozed for %s", symbol, parts[2])) //nolint:errcheck
}

// resolveSubscriberID returns the subscriber UUID by username (preferred) or chat_id fallback.
func (h *Handler) resolveSubscriberID(ctx context.Context, chatID int64, username string) string {
	if username != "" {
		id, _, err := h.db.GetSubscriberIDByUsername(ctx, username)
		if err == nil {
			return id
		}
	}
	id, err := h.db.GetSubscriberIDByChatID(ctx, chatID)
	if err != nil {
		return ""
	}
	return id
}

// ─── Market status ────────────────────────────────────────────────────────────

// MarketStatus handles GET /api/market/status
// Returns current market activity level based on time since last alert.
func (h *Handler) MarketStatus(w http.ResponseWriter, r *http.Request) {
	lastAlert := h.worker.GetLastAlertTime()
	since := time.Since(lastAlert)

	status := "volatile"
	message := "Market is active — alerts firing normally"

	if lastAlert.IsZero() || since > 2*time.Hour {
		status = "quiet"
		message = "Market is ranging quietly — no significant setups detected. This is normal during low volatility periods."
	} else if since > 30*time.Minute {
		status = "active"
		message = "Market is calm — monitoring for setups"
	}

	sinceMinutes := "0"
	if !lastAlert.IsZero() {
		sinceMinutes = fmt.Sprintf("%.0f", since.Minutes())
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"status":         status,
		"last_alert_at":  lastAlert.Format(time.RFC3339),
		"message":        message,
		"since_minutes":  sinceMinutes,
	})
}

// ─── Admin AI toggle ─────────────────────────────────────────────────────────

func (h *Handler) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if h.adminSecret == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "admin not configured"})
		return false
	}
	key := r.Header.Get("X-Admin-Key")
	if key != h.adminSecret {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return false
	}
	return true
}

// PauseAI handles POST /api/admin/ai/pause — disables AI analysis.
func (h *Handler) PauseAI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if !h.requireAdmin(w, r) {
		return
	}
	analysis.SetAIEnabled(false)
	writeJSON(w, http.StatusOK, map[string]bool{"ai_enabled": false})
}

// ResumeAI handles POST /api/admin/ai/resume — enables AI analysis.
func (h *Handler) ResumeAI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if !h.requireAdmin(w, r) {
		return
	}
	analysis.SetAIEnabled(true)
	writeJSON(w, http.StatusOK, map[string]bool{"ai_enabled": true})
}

// AIStatus handles GET /api/admin/ai/status — returns {"ai_enabled": true/false}.
func (h *Handler) AIStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if !h.requireAdmin(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ai_enabled": analysis.IsAIEnabled()})
}

// TriggerPost handles POST /api/admin/poster/trigger — fires a post generation immediately.
func (h *Handler) TriggerPost(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if !h.requireAdmin(w, r) {
		return
	}
	// Use context.Background() — r.Context() cancels as soon as the response
	// is sent, which would kill the 16 FetchSnapshot calls mid-flight.
	go h.worker.TriggerPost(context.Background())
	writeJSON(w, http.StatusOK, map[string]string{"status": "post generation triggered"})
}

// ─── Custom price alert limits by tier ───────────────────────────────────────

const (
	customAlertLimitBasic = 5
	customAlertLimitPro   = 20
)

func customAlertLimit(tier string) int {
	if tier == "pro" {
		return customAlertLimitPro
	}
	return customAlertLimitBasic
}

// CustomPriceAlerts handles GET / POST / DELETE /api/alerts/custom
//
//	GET    ?username=X            → list active alerts for user
//	POST   body {username, symbol, target_price, direction, note}
//	DELETE ?username=X&id=Y      → delete one alert owned by user
func (h *Handler) CustomPriceAlerts(w http.ResponseWriter, r *http.Request) {
	username := r.URL.Query().Get("username")
	if err := validateUsername(username); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "username required"})
		return
	}

	subscriberID, tier, err := h.db.GetSubscriberIDByUsername(r.Context(), username)
	if err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "subscriber not found — subscribe via Telegram first"})
		return
	}
	if tier == "free" || tier == "" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "custom alerts require Basic or Pro plan"})
		return
	}

	switch r.Method {
	case http.MethodGet:
		alerts, err := h.db.GetCustomPriceAlerts(r.Context(), subscriberID)
		if err != nil {
			log.Printf("CustomPriceAlerts GET: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to fetch alerts"})
			return
		}
		writeJSON(w, http.StatusOK, alerts)

	case http.MethodPost:
		var body struct {
			Symbol      string  `json:"symbol"`
			TargetPrice float64 `json:"target_price"`
			Direction   string  `json:"direction"`
			Note        string  `json:"note"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
			return
		}
		if body.Symbol == "" || body.TargetPrice <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "symbol and target_price required"})
			return
		}
		if body.Direction != "above" && body.Direction != "below" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "direction must be 'above' or 'below'"})
			return
		}
		if len(body.Note) > 100 {
			body.Note = body.Note[:100]
		}

		count, err := h.db.CountCustomPriceAlerts(r.Context(), subscriberID)
		if err != nil {
			log.Printf("CustomPriceAlerts count: %v", err)
		}
		limit := customAlertLimit(tier)
		if count >= limit {
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": fmt.Sprintf("limit of %d active custom alerts reached for %s plan", limit, tier),
			})
			return
		}

		if err := h.db.CreateCustomPriceAlert(r.Context(), subscriberID, strings.ToUpper(body.Symbol), body.Direction, body.Note, body.TargetPrice); err != nil {
			log.Printf("CustomPriceAlerts create: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to create alert"})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{"status": "created"})

	case http.MethodDelete:
		id := r.URL.Query().Get("id")
		if id == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id required"})
			return
		}
		if err := h.db.DeleteCustomPriceAlert(r.Context(), id, subscriberID); err != nil {
			log.Printf("CustomPriceAlerts delete: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to delete alert"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})

	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

// ─── Discord webhook settings ─────────────────────────────────────────────────

// DiscordWebhook handles GET / POST / DELETE /api/settings/discord
//
//	GET    ?username=X                   → returns masked webhook URL
//	POST   ?username=X  body {url}       → sets webhook URL (validates discord.com domain)
//	DELETE ?username=X                   → removes webhook URL
func (h *Handler) DiscordWebhook(w http.ResponseWriter, r *http.Request) {
	username := r.URL.Query().Get("username")
	if err := validateUsername(username); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "username required"})
		return
	}

	switch r.Method {
	case http.MethodGet:
		hookURL, err := h.db.GetDiscordWebhook(r.Context(), username)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to fetch"})
			return
		}
		masked := ""
		if hookURL != "" {
			// Show only the first 40 chars so the token is not fully exposed
			if len(hookURL) > 40 {
				masked = hookURL[:40] + "…"
			} else {
				masked = hookURL
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"webhook_url": masked, "set": hookURL != ""})

	case http.MethodPost:
		var body struct {
			URL string `json:"url"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
			return
		}
		if !isValidDiscordWebhookURL(body.URL) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "URL must be a discord.com webhook URL"})
			return
		}
		if err := h.db.UpdateDiscordWebhook(r.Context(), username, body.URL); err != nil {
			log.Printf("DiscordWebhook set: %v", err)
			if strings.Contains(err.Error(), "subscriber not found") {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Subscribe via Telegram first before adding a Discord webhook"})
			} else {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save"})
			}
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})

	case http.MethodDelete:
		if err := h.db.UpdateDiscordWebhook(r.Context(), username, ""); err != nil {
			log.Printf("DiscordWebhook delete: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to remove"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "removed"})

	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

// ─── Alert performance stats ──────────────────────────────────────────────────

type severityStats struct {
	Count       int     `json:"count"`
	AvgAbs15m   float64 `json:"avg_abs_pct_15m"`
	AvgAbs1h    float64 `json:"avg_abs_pct_1h"`
	Effective1h int     `json:"effective_pct_1h"` // % where |outcome_1h| >= 1%
}

// setupStats holds aggregated outcome data for a single signal type.
// HasData is false when count < minSetupSamples — callers should show "—" instead of the metrics.
type setupStats struct {
	Count     int     `json:"count"`
	WinRate1h int     `json:"win_rate_1h"`   // % where |outcome_1h| >= 1%
	AvgAbs1h  float64 `json:"avg_abs_1h"`    // mean absolute 1h move
	HasData   bool    `json:"has_data"`
}

const minSetupSamples = 10

type alertPerformanceResponse struct {
	Period      string                `json:"period"`
	PeriodStart time.Time             `json:"period_start"`
	TotalAlerts int                   `json:"total_alerts"`
	High        severityStats         `json:"high"`
	Medium      severityStats         `json:"medium"`
	SetupStats  map[string]setupStats `json:"setup_stats"`
	UpdatedAt   time.Time             `json:"updated_at"`
}

// setupTypeFromMessage classifies an alert message into a setup type key.
func setupTypeFromMessage(msg string) string {
	m := strings.ToLower(msg)
	switch {
	case strings.Contains(m, "funding rate"):
		return "funding_spike"
	case strings.Contains(m, "oi up") || strings.Contains(m, "oi down") || strings.Contains(m, "open interest"):
		return "oi_divergence"
	case strings.Contains(m, "liquidation"):
		return "liquidation_cluster"
	default:
		return ""
	}
}

// v2Launch is when the improved alert engine was deployed.
// Performance is only measured from this date forward to exclude pre-improvement noise.
var v2Launch = time.Date(2026, 3, 18, 0, 0, 0, 0, time.UTC)

// perfCache avoids hitting Supabase on every landing page load.
var (
	perfCacheMu   sync.Mutex
	perfCacheData *alertPerformanceResponse
	perfCacheAt   time.Time
)

// AlertPerformance handles GET /api/alerts/performance.
// Returns aggregated signal accuracy stats for the last 30 days. Cached for 10 minutes.
func (h *Handler) AlertPerformance(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	perfCacheMu.Lock()
	if perfCacheData != nil && time.Since(perfCacheAt) < 10*time.Minute {
		cached := perfCacheData
		perfCacheMu.Unlock()
		writeJSON(w, http.StatusOK, cached)
		return
	}
	perfCacheMu.Unlock()

	since := time.Now().UTC().Add(-30 * 24 * time.Hour)
	if v2Launch.After(since) {
		since = v2Launch
	}
	entries, err := h.db.GetAlertOutcomes(r.Context(), since)
	if err != nil {
		log.Printf("AlertPerformance: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to fetch"})
		return
	}

	type bucket struct {
		count     int
		sum15m    float64
		sum1h     float64
		effective int
	}
	buckets := map[string]*bucket{}

	for _, e := range entries {
		sev := strings.ToLower(e.Severity)
		if buckets[sev] == nil {
			buckets[sev] = &bucket{}
		}
		b := buckets[sev]
		b.count++
		if e.OutcomePct15m != nil {
			b.sum15m += math.Abs(*e.OutcomePct15m)
		}
		if e.OutcomePct1h != nil {
			b.sum1h += math.Abs(*e.OutcomePct1h)
			if math.Abs(*e.OutcomePct1h) >= 1.0 {
				b.effective++
			}
		}
	}

	toStats := func(sev string) severityStats {
		b := buckets[sev]
		if b == nil || b.count == 0 {
			return severityStats{}
		}
		effPct := int(float64(b.effective) / float64(b.count) * 100)
		return severityStats{
			Count:       b.count,
			AvgAbs15m:   math.Round(b.sum15m/float64(b.count)*100) / 100,
			AvgAbs1h:    math.Round(b.sum1h/float64(b.count)*100) / 100,
			Effective1h: effPct,
		}
	}

	total := 0
	for _, b := range buckets {
		total += b.count
	}

	// ── Setup-type aggregation ────────────────────────────────────────────────
	type setupBucket struct {
		count     int
		sum1h     float64
		effective int
	}
	setupBuckets := map[string]*setupBucket{}
	for _, e := range entries {
		key := setupTypeFromMessage(e.Message)
		if key == "" {
			continue
		}
		if setupBuckets[key] == nil {
			setupBuckets[key] = &setupBucket{}
		}
		sb := setupBuckets[key]
		sb.count++
		if e.OutcomePct1h != nil {
			sb.sum1h += math.Abs(*e.OutcomePct1h)
			if math.Abs(*e.OutcomePct1h) >= 1.0 {
				sb.effective++
			}
		}
	}
	allSetups := []string{"funding_spike", "oi_divergence", "liquidation_cluster"}
	setupStatsMap := make(map[string]setupStats, len(allSetups))
	for _, key := range allSetups {
		sb := setupBuckets[key]
		if sb == nil || sb.count == 0 {
			setupStatsMap[key] = setupStats{}
			continue
		}
		hasData := sb.count >= minSetupSamples
		setupStatsMap[key] = setupStats{
			Count:     sb.count,
			WinRate1h: int(float64(sb.effective) / float64(sb.count) * 100),
			AvgAbs1h:  math.Round(sb.sum1h/float64(sb.count)*100) / 100,
			HasData:   hasData,
		}
	}

	periodLabel := "30d"
	if since.Equal(v2Launch) {
		periodLabel = "since " + v2Launch.Format("Jan 2, 2006")
	}
	result := &alertPerformanceResponse{
		Period:      periodLabel,
		PeriodStart: since,
		TotalAlerts: total,
		High:        toStats("high"),
		Medium:      toStats("medium"),
		SetupStats:  setupStatsMap,
		UpdatedAt:   time.Now().UTC(),
	}

	perfCacheMu.Lock()
	perfCacheData = result
	perfCacheAt = time.Now()
	perfCacheMu.Unlock()

	writeJSON(w, http.StatusOK, result)
}

func isValidDiscordWebhookURL(u string) bool {
	return strings.HasPrefix(u, "https://discord.com/api/webhooks/") ||
		strings.HasPrefix(u, "https://discordapp.com/api/webhooks/")
}

// ─── Dashboard snooze ─────────────────────────────────────────────────────────

// SnoozeHandler handles GET / POST / DELETE /api/snooze
// All snoozes are applied to "ALL" symbols — a global dashboard-level snooze.
//
//	GET    ?username=X                      → {"snoozed": bool, "remaining": "2h30m"}
//	POST   ?username=X  {"duration":"1h"}   → snooze all alerts for duration
//	DELETE ?username=X                      → unsnooze
func (h *Handler) SnoozeHandler(w http.ResponseWriter, r *http.Request) {
	username := r.URL.Query().Get("username")
	if err := validateUsername(username); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "username required"})
		return
	}

	subID := h.resolveSubscriberID(r.Context(), 0, username)
	if subID == "" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "subscriber not found"})
		return
	}

	switch r.Method {
	case http.MethodGet:
		active := snoozeGlobal.List(subID)
		exp, ok := active["ALL"]
		if !ok {
			writeJSON(w, http.StatusOK, map[string]interface{}{"snoozed": false})
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"snoozed":    true,
			"remaining":  snoozeFormatRemaining(exp),
			"expires_at": exp.Format(time.RFC3339),
		})

	case http.MethodPost:
		var body struct {
			Duration string `json:"duration"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
			return
		}
		d, ok := snoozeParseDuration(body.Duration)
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "valid durations: 1h, 4h, 24h"})
			return
		}
		snoozeGlobal.Snooze(subID, "ALL", d)
		writeJSON(w, http.StatusOK, map[string]string{"status": "snoozed", "duration": body.Duration})

	case http.MethodDelete:
		snoozeGlobal.Unsnooze(subID, "ALL")
		writeJSON(w, http.StatusOK, map[string]string{"status": "unsnoozed"})

	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

// ─── Alert threshold settings ─────────────────────────────────────────────────

// thresholdKeys are the rule keys that support numeric thresholds.
var thresholdKeys = []string{"long_bias", "short_bias", "oi_divergence", "funding_spike"}

// ThresholdSettings handles GET / POST /api/settings/thresholds
//
//	GET  ?username=X            → {"long_bias_threshold": 65.0, ...}
//	POST ?username=X  {body}    → merge threshold values into subscriber rules
func (h *Handler) ThresholdSettings(w http.ResponseWriter, r *http.Request) {
	username := r.URL.Query().Get("username")
	if err := validateUsername(username); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "username required"})
		return
	}

	switch r.Method {
	case http.MethodGet:
		current, err := h.db.GetSubscriberRules(r.Context(), username)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to fetch rules"})
			return
		}
		var ruleMap map[string]interface{}
		if len(current) > 0 {
			json.Unmarshal(current, &ruleMap) //nolint:errcheck
		}
		out := make(map[string]interface{})
		for _, k := range thresholdKeys {
			key := k + "_threshold"
			if v, ok := ruleMap[key]; ok {
				out[key] = v
			}
		}
		writeJSON(w, http.StatusOK, out)

	case http.MethodPost:
		var incoming map[string]float64
		if err := json.NewDecoder(r.Body).Decode(&incoming); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
			return
		}
		current, err := h.db.GetSubscriberRules(r.Context(), username)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to fetch rules"})
			return
		}
		var ruleMap map[string]interface{}
		if len(current) > 0 {
			json.Unmarshal(current, &ruleMap) //nolint:errcheck
		}
		if ruleMap == nil {
			ruleMap = make(map[string]interface{})
		}
		allowed := make(map[string]bool)
		for _, k := range thresholdKeys {
			allowed[k+"_threshold"] = true
		}
		for k, v := range incoming {
			if allowed[k] {
				ruleMap[k] = v
			}
		}
		merged, _ := json.Marshal(ruleMap)
		if err := h.db.UpdateSubscriberRules(r.Context(), username, merged); err != nil {
			log.Printf("ThresholdSettings update: %v", err)
			if strings.Contains(err.Error(), "subscriber not found") {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Subscribe via Telegram first before saving thresholds"})
			} else {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to update rules"})
			}
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})

	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

// ─── Playbook status ──────────────────────────────────────────────────────────

// PlaybookStatus handles GET /api/playbook/status?symbol=BTC
// Returns the most recent playbook trigger state (forming / confirmed / idle).
func (h *Handler) PlaybookStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	symbol := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("symbol")))
	if symbol == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "symbol required"})
		return
	}
	state := h.worker.GetPlaybookState(symbol)
	writeJSON(w, http.StatusOK, state)
}

// ─── Klines proxy ─────────────────────────────────────────────────────────────

// Klines proxies GET /api/klines?symbol=BTC&interval=5m&limit=100 to Binance
// server-side, avoiding CORS issues with direct browser requests.
func (h *Handler) Klines(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	symbol := strings.ToUpper(r.URL.Query().Get("symbol"))
	if symbol == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "symbol required"})
		return
	}
	interval := r.URL.Query().Get("interval")
	if interval == "" {
		interval = "5m"
	}
	limit := r.URL.Query().Get("limit")
	if limit == "" {
		limit = "100"
	}

	url := fmt.Sprintf("https://api.binance.com/api/v3/klines?symbol=%sUSDT&interval=%s&limit=%s",
		symbol, interval, limit)

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, url, nil)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to build request"})
		return
	}
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "failed to fetch klines"})
		return
	}
	defer resp.Body.Close()

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=15")
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// PostSimulatorScore handles POST /api/simulator/score
func (h *Handler) PostSimulatorScore(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var row supabase.SimulatorScoreRow
	if err := json.NewDecoder(r.Body).Decode(&row); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	row.Username = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(row.Username)), "@")
	if row.Username == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "username required"})
		return
	}
	if row.DisplayName == "" {
		row.DisplayName = "@" + row.Username
	}
	if err := h.db.UpsertSimulatorScore(r.Context(), row); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save score"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "true"})
}

// GetSimulatorLeaderboard handles GET /api/simulator/leaderboard?limit=10
func (h *Handler) GetSimulatorLeaderboard(w http.ResponseWriter, r *http.Request) {
	limit := 10
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 50 {
			limit = n
		}
	}
	rows, err := h.db.GetSimulatorLeaderboard(r.Context(), limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to fetch leaderboard"})
		return
	}
	if rows == nil {
		rows = []supabase.SimulatorScoreRow{}
	}
	w.Header().Set("Cache-Control", "public, max-age=30")
	writeJSON(w, http.StatusOK, rows)
}

// GetSimulatorScenario handles GET /api/simulator/scenario?limit=20
// Returns recent resolved live scenarios captured from real playbook signals.
func (h *Handler) GetSimulatorScenario(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	limit := 20
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 100 {
			limit = n
		}
	}
	rows, err := h.db.GetSimulatorScenario(r.Context(), limit)
	if err != nil {
		log.Printf("GetSimulatorScenario: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to fetch scenarios"})
		return
	}
	if rows == nil {
		rows = []supabase.SimulatorScenarioRow{}
	}
	w.Header().Set("Cache-Control", "public, max-age=60")
	writeJSON(w, http.StatusOK, rows)
}
