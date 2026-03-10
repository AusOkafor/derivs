package config

import (
	"fmt"
	"os"
	"strconv"
)

type Config struct {
	Port               string
	AnthropicAPIKey    string
	AllowedOrigins     string
	CacheTTLSeconds    int
	TelegramBotToken   string
	SupabaseURL        string
	SupabaseServiceKey string
}

func Load() *Config {
	cfg := &Config{
		Port:               getEnvOrDefault("PORT", "8080"),
		AnthropicAPIKey:    os.Getenv("ANTHROPIC_API_KEY"),
		AllowedOrigins:     getEnvOrDefault("ALLOWED_ORIGINS", "*"),
		CacheTTLSeconds:    getEnvAsIntOrDefault("CACHE_TTL_SECONDS", 30),
		TelegramBotToken:   os.Getenv("TELEGRAM_BOT_TOKEN"),
		SupabaseURL:        os.Getenv("SUPABASE_URL"),
		SupabaseServiceKey: os.Getenv("SUPABASE_SERVICE_KEY"),
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
