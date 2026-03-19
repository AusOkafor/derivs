package config

import (
	"fmt"
	"os"
	"strconv"
)

// DefaultSymbols is the full list of supported symbols (Bybit, Binance, OKX).
var DefaultSymbols = []string{
	"BTC", "ETH", "SOL", "ARB", "DOGE", "AVAX",
	"WLD", "SUI", "OP", "INJ", "TIA", "PENDLE",
	"XRP", "BNB", "LINK", "TON",
}

type Config struct {
	Port                    string
	AnthropicAPIKey         string
	AllowedOrigins          string
	CacheTTLSeconds         int
	TelegramBotToken        string
	SupabaseURL             string
	SupabaseServiceKey      string
	StripeSecretKey         string
	StripeWebhookSecret     string
	StripePriceIDBasic      string
	StripePriceIDPro        string
	LemonSqueezyAPIKey      string
	LemonSqueezyWebhookSecret string
	LemonSqueezyVariantBasic  string
	LemonSqueezyVariantPro    string
	LemonSqueezyStoreID       string
	AdminSecret             string
}

func Load() *Config {
	cfg := &Config{
		Port:                getEnvOrDefault("PORT", "8080"),
		AnthropicAPIKey:     os.Getenv("ANTHROPIC_API_KEY"),
		AllowedOrigins:     getEnvOrDefault("ALLOWED_ORIGINS", "https://derivlens.io"),
		CacheTTLSeconds:    getEnvAsIntOrDefault("CACHE_TTL_SECONDS", 30),
		TelegramBotToken:   os.Getenv("TELEGRAM_BOT_TOKEN"),
		SupabaseURL:        os.Getenv("SUPABASE_URL"),
		SupabaseServiceKey: os.Getenv("SUPABASE_SERVICE_KEY"),
		StripeSecretKey:             os.Getenv("STRIPE_SECRET_KEY"),
		StripeWebhookSecret:          os.Getenv("STRIPE_WEBHOOK_SECRET"),
		StripePriceIDBasic:           os.Getenv("STRIPE_PRICE_ID_BASIC"),
		StripePriceIDPro:             os.Getenv("STRIPE_PRICE_ID_PRO"),
		LemonSqueezyAPIKey:           os.Getenv("LEMONSQUEEZY_API_KEY"),
		LemonSqueezyWebhookSecret:    os.Getenv("LEMONSQUEEZY_WEBHOOK_SECRET"),
		LemonSqueezyVariantBasic:    os.Getenv("LEMONSQUEEZY_VARIANT_BASIC"),
		LemonSqueezyVariantPro:      os.Getenv("LEMONSQUEEZY_VARIANT_PRO"),
		LemonSqueezyStoreID:         os.Getenv("LEMONSQUEEZY_STORE_ID"),
		AdminSecret:                 os.Getenv("ADMIN_SECRET"),
	}

	required := map[string]string{
		"ANTHROPIC_API_KEY":    cfg.AnthropicAPIKey,
		"TELEGRAM_BOT_TOKEN":   cfg.TelegramBotToken,
		"SUPABASE_URL":         cfg.SupabaseURL,
		"SUPABASE_SERVICE_KEY": cfg.SupabaseServiceKey,
	}
	for key, val := range required {
		if val == "" {
			panic(fmt.Sprintf("config: required environment variable %q is not set", key))
		}
	}

	return cfg
}

func (c *Config) Addr() string {
	return ":" + c.Port
}

func getEnvOrDefault(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

func getEnvAsIntOrDefault(key string, defaultVal int) int {
	v := os.Getenv(key)
	if v == "" {
		return defaultVal
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		panic(fmt.Sprintf("config: environment variable %q must be an integer, got %q", key, v))
	}
	return n
}
