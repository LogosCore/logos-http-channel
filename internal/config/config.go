package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"

	"github.com/joho/godotenv"
)

type Config struct {
	ChannelID        string
	Listen           string
	LogosSyncBaseURL string
	HealthListen     string
	MaxBodyBytes     int64
	// RabbitMQURL is the AMQP URL for registering with core's control plane.
	RabbitMQURL string
}

// minBodyBytes is the smallest valid message: offset byte plus raw UUID.
const minBodyBytes = 17

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
		LogosSyncBaseURL: envOrDefault("LOGOS_SYNC_BASE_URL", "http://localhost:8002"),
		HealthListen:     envOrDefault("HEALTH_LISTEN_ADDR", ":8081"),
	}

	maxBody, err := strconv.ParseInt(envOrDefault("MAX_BODY_BYTES", "1048576"), 10, 64)
	if err != nil || maxBody < minBodyBytes {
		return Config{}, fmt.Errorf("MAX_BODY_BYTES must be an integer >= %d", minBodyBytes)
	}
	cfg.MaxBodyBytes = maxBody

	vhost := envOrDefault("RABBITMQ_VHOST", "/")
	cfg.RabbitMQURL = (&url.URL{
		Scheme: "amqp",
		User:   url.UserPassword(envOrDefault("RABBITMQ_USER", "guest"), envOrDefault("RABBITMQ_PASSWORD", "guest")),
		Host:   net.JoinHostPort(envOrDefault("RABBITMQ_HOST", "localhost"), envOrDefault("RABBITMQ_PORT", "5672")),
		// amqp091 takes the vhost from the path minus its leading "/", so the
		// default "/" vhost becomes the path "//".
		Path: "/" + vhost,
	}).String()

	if cfg.LogosSyncBaseURL == "" {
		return Config{}, fmt.Errorf("LOGOS_SYNC_BASE_URL is required")
	}

	return cfg, nil
}
