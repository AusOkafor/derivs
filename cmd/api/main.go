package main

import (
	"context"
	"log"
	"net/http"

	"github.com/joho/godotenv"
	"derivs-backend/internal/aggregator"
	"derivs-backend/internal/alerts"
	"derivs-backend/internal/analysis"
	"derivs-backend/internal/billing"
	"derivs-backend/internal/cache"
	"derivs-backend/internal/config"
	"derivs-backend/internal/feargreed"
	"derivs-backend/internal/handlers"
	"derivs-backend/internal/notify"
	"derivs-backend/internal/supabase"
	"derivs-backend/internal/worker"
	"golang.org/x/net/websocket"
)

func main() {
	_ = godotenv.Load() // no-op in production where env vars are set directly

	cfg := config.Load()

	agg := aggregator.New(cfg)
	az := analysis.New(cfg.AnthropicAPIKey)
	c := cache.New(cfg.CacheTTLSeconds)
	detector := alerts.New()
	calc := feargreed.New()

	tg := notify.NewTelegram(cfg.TelegramBotToken)
	sb := supabase.New(cfg.SupabaseURL, cfg.SupabaseServiceKey)
	wrk := worker.New(agg, detector, tg, sb, calc)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go wrk.Start(ctx)

	var billingClient *billing.StripeClient
	if cfg.StripeSecretKey != "" && cfg.StripeProPriceID != "" && cfg.StripeWebhookSecret != "" {
		billingClient = billing.New(cfg.StripeSecretKey, cfg.StripeProPriceID, cfg.StripeWebhookSecret)
	}

	h := handlers.New(agg, az, c, detector, calc, sb, tg, billingClient)
	hub := handlers.NewHub(h)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", h.Health)
	mux.HandleFunc("/api/snapshot", h.GetSnapshot)
	mux.HandleFunc("/api/history", h.GetHistory)
	mux.HandleFunc("/api/alerts", h.GetAlerts)
	mux.HandleFunc("/api/alerts/history", h.GetAlertHistory)
	mux.HandleFunc("/api/tickers", h.GetTickers)
	mux.HandleFunc("/api/subscribe", h.Subscribe)
	mux.HandleFunc("/api/unsubscribe", h.Unsubscribe)
	mux.HandleFunc("/api/billing/checkout", h.CreateCheckout)
	mux.HandleFunc("/api/billing/webhook", h.StripeWebhook)
	mux.HandleFunc("/api/billing/status", h.GetBillingStatus)
	mux.HandleFunc("/api/webhook/telegram", h.TelegramWebhook)
	mux.Handle("/ws", websocket.Handler(hub.ServeWS))

	log.Printf("derivlens: listening on %s", cfg.Addr())
	if err := http.ListenAndServe(cfg.Addr(), corsMiddleware(cfg.AllowedOrigins, mux)); err != nil {
		log.Fatalf("derivlens: server error: %v", err)
	}
}

func corsMiddleware(allowedOrigins string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", allowedOrigins)
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}
