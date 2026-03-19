package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/joho/godotenv"
	"derivs-backend/internal/aggregator"
	"derivs-backend/internal/alerts"
	"derivs-backend/internal/analysis"
	"derivs-backend/internal/billing"
	"derivs-backend/internal/cache"
	"derivs-backend/internal/config"
	"derivs-backend/internal/feargreed"
	"derivs-backend/internal/handlers"
	"derivs-backend/internal/liquidations"
	"derivs-backend/internal/models"
	"derivs-backend/internal/notify"
	"derivs-backend/internal/supabase"
	"derivs-backend/internal/worker"
	"golang.org/x/net/websocket"
)

func main() {
	_ = godotenv.Load() // load before Sentry so APP_ENV is available from .env
	err := sentry.Init(sentry.ClientOptions{
		Dsn:              os.Getenv("SENTRY_DSN"),
		TracesSampleRate: 0.1,
		Environment:      os.Getenv("APP_ENV"),
	})
	if err != nil {
		log.Printf("sentry init failed: %v", err)
	}
	defer sentry.Flush(2 * time.Second)

	cfg := config.Load()

	agg := aggregator.New(cfg)
	az := analysis.New(cfg.AnthropicAPIKey)
	c := cache.New(cfg.CacheTTLSeconds)
	detector := alerts.New()
	calc := feargreed.New()

	tg := notify.NewTelegram(cfg.TelegramBotToken)
	sb := supabase.New(cfg.SupabaseURL, cfg.SupabaseServiceKey)
	wrk := worker.New(agg, detector, tg, sb, calc)
	alerts.SetOnHighAlert(func(a models.Alert, snap models.MarketSnapshot, sigs models.MarketSignals) {
		if a.Severity == "low" {
			return // don't post low alerts to public channel
		}
		if a.ClusterSize == 0 {
			log.Printf("[channel] skipping regime alert for public channel: %s", a.Symbol)
			return
		}
		if !alerts.IsSafeToSend(a) {
			return
		}
		// Only post cluster alerts with $500K+ to public channel
		if a.ClusterSize < 500_000 {
			return
		}
		if err := tg.PostTopAlert(a, snap, sigs); err != nil {
			log.Printf("alerts: PostTopAlert: %v", err)
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go wrk.Start(ctx)

	liqFeed := liquidations.NewFeed(config.DefaultSymbols)
	go liqFeed.Start(ctx)

	var billingClient *billing.StripeClient
	if cfg.StripeSecretKey != "" && cfg.StripeWebhookSecret != "" && (cfg.StripePriceIDBasic != "" || cfg.StripePriceIDPro != "") {
		billingClient = billing.New(cfg.StripeSecretKey, cfg.StripeWebhookSecret)
	}

	var lsClient *billing.LemonSqueezyClient
	if cfg.LemonSqueezyAPIKey != "" && cfg.LemonSqueezyStoreID != "" {
		lsClient = billing.NewLemonSqueezyClient(
			cfg.LemonSqueezyAPIKey,
			cfg.LemonSqueezyWebhookSecret,
			cfg.LemonSqueezyVariantBasic,
			cfg.LemonSqueezyVariantPro,
			cfg.LemonSqueezyStoreID,
			"https://derivlens.io",
		)
	}

	h := handlers.New(agg, az, c, detector, calc, sb, tg, billingClient, lsClient, cfg.AdminSecret, cfg.StripePriceIDBasic, cfg.StripePriceIDPro, wrk, liqFeed)
	hub := handlers.NewHub(h)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", h.Health)
	mux.HandleFunc("/api/market/status", h.MarketStatus)
	mux.HandleFunc("/api/waitlist", h.JoinWaitlist)
	mux.HandleFunc("/api/snapshot", h.GetSnapshot)
	mux.HandleFunc("/api/history", h.GetHistory)
	mux.HandleFunc("/api/alerts", h.GetAlerts)
	mux.HandleFunc("/api/alerts/history", h.GetAlertHistory)
	mux.HandleFunc("/api/alerts/custom", h.CustomPriceAlerts)
	mux.HandleFunc("/api/tickers", h.GetTickers)
	mux.HandleFunc("/api/subscribe", h.Subscribe)
	mux.HandleFunc("/api/unsubscribe", h.Unsubscribe)
	mux.HandleFunc("/api/billing/checkout", h.CreateCheckout)
	mux.HandleFunc("/api/billing/portal", h.CreatePortal)
	mux.HandleFunc("/api/billing/webhook", h.StripeWebhook)
	mux.HandleFunc("/api/billing/lemonsqueezy/webhook", h.LemonSqueezyWebhook)
	mux.HandleFunc("/api/billing/lemonsqueezy/checkout", h.LemonSqueezyCheckout)
	mux.HandleFunc("/api/billing/status", h.GetBillingStatus)
	mux.HandleFunc("/api/settings", h.Settings)
	mux.HandleFunc("/api/webhook/telegram", h.TelegramWebhook)
	mux.HandleFunc("/api/admin/ai/pause", h.PauseAI)
	mux.HandleFunc("/api/admin/ai/resume", h.ResumeAI)
	mux.HandleFunc("/api/admin/ai/status", h.AIStatus)
	mux.Handle("/ws", websocket.Handler(hub.ServeWS))

	log.Printf("derivlens: listening on %s", cfg.Addr())
	if err := http.ListenAndServe(cfg.Addr(), corsMiddleware(cfg.AllowedOrigins, mux)); err != nil {
		log.Fatalf("derivlens: server error: %v", err)
	}
}

func corsMiddleware(allowedOrigins string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", allowedOrigins)
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Admin-Key")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}
