package config

import (
	"fmt"
	"os"

	"github.com/joho/godotenv"
)

type Config struct {
	ChannelID        string
	Listen           string
	LogosSyncBaseURL string
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func LoadFromEnv(envFilePath string) (Config, error) {
	if envFilePath != "" {
		_ = godotenv.Load(envFilePath)
	}

	cfg := Config{
		ChannelID:        envOrDefault("CHANNEL_ID", "http-main"),
		Listen:           envOrDefault("LISTEN_ADDR", ":8080"),
		LogosSyncBaseURL: envOrDefault("LOGOS_SYNC_BASE_URL", "http://localhost:9000"),
	}

	if cfg.LogosSyncBaseURL == "" {
		return Config{}, fmt.Errorf("LOGOS_SYNC_BASE_URL is required")
	}

	return cfg, nil
}
