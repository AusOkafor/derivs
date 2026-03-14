package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/getsentry/sentry-go"
	"derivs-backend/internal/analysis"
	"derivs-backend/internal/billing"
	"derivs-backend/internal/models"
	"derivs-backend/internal/signals"
	"derivs-backend/internal/supabase"
)

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
		// Allow test key override for Settings page "Test Connection"
		if override := r.Header.Get("X-API-Key-Override"); override != "" {
			userAPIKey = override
		}
	}

	// Bypass cache when username provided (tier-specific AI)
	if username == "" {
		if cached, ok := h.cache.Get(symbol); ok {
			if cached.Snapshot.Symbol != symbol {
				log.Printf("GetSnapshot: cache symbol mismatch: requested %s, got %s", symbol, cached.Snapshot.Symbol)
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

	engine := signals.New()
	momentum := h.cache.GetPriceMomentum(symbol)
	sigs := engine.Analyze(snap, momentum)

	ai, err := h.analyzer.Analyze(ctx, snap, sigs, tier, userAPIKey, preferredModel)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		sentry.CaptureException(err)
		ai = models.AIAnalysis{Symbol: symbol, Summary: "Analysis temporarily unavailable", Sentiment: "neutral", Confidence: 0, GeneratedAt: time.Now().UTC()}
	}

	result := models.SnapshotWithAnalysis{
		Snapshot:  snap,
		Analysis:  ai,
		Alerts:    h.detector.Analyze(snap, sigs),
		FearGreed: h.calc.Calculate(snap),
		Signals:   sigs,
	}

	if username == "" {
		h.cache.Set(symbol, result)
	}

	w.Header().Set("X-Cache", "MISS")
	writeJSON(w, http.StatusOK, result)
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
// Returns HistoricalData with the last 48 hourly funding rate points and OI candles.
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
		fundingHistory, fundingErr = h.aggregator.FetchFundingHistory(ctx, symbol, 48)
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
	writeJSON(w, http.StatusOK, h.detector.Analyze(snap, sigs))
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
			price, change24h, tickErr := h.aggregator.FetchTicker(ctx, symbol)
			if tickErr != nil {
				price = snap.LiquidationMap.CurrentPrice
			}
			momentum := h.cache.GetPriceMomentum(symbol)
			sigs := engine.Analyze(snap, momentum)
			fg := h.calc.Calculate(snap)
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
}

// TelegramWebhook handles POST /api/webhook/telegram.
// Telegram requires a 200 response for every update, even on errors.
func (h *Handler) TelegramWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusOK) // always 200 to Telegram
		return
	}

	var update telegramUpdate
	if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
		log.Printf("telegram webhook: decode: %v", err)
		w.WriteHeader(http.StatusOK)
		return
	}

	msg := update.Message
	if msg.Text != "/start" || msg.From.Username == "" {
		w.WriteHeader(http.StatusOK)
		return
	}

	ctx := r.Context()

	if err := h.db.UpdateChatID(ctx, msg.From.Username, msg.Chat.ID); err != nil {
		log.Printf("telegram webhook: UpdateChatID(%s, %d): %v", msg.From.Username, msg.Chat.ID, err)
		w.WriteHeader(http.StatusOK)
		return
	}

	welcome := "✅ <b>DerivLens Alerts Activated!</b>\nYou'll receive alerts for your subscribed symbols.\nPowered by DerivLens 🚀"
	if err := h.notifier.SendMessage(ctx, msg.Chat.ID, welcome); err != nil {
		log.Printf("telegram webhook: SendMessage(%d): %v", msg.Chat.ID, err)
	}

	w.WriteHeader(http.StatusOK)
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
