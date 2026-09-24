package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/joho/godotenv"
	coreProfile "github.com/logoscore/logos-golang-channel-core/pkg/profile"
)

type Config struct {
	ChannelID        string
	Listen           string
	LogosSyncBaseURL string
	ProfilesDir      string
	RabbitMQHost     string
	RabbitMQPort     string
	RabbitMQPassword string
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// LoadFromEnv reads runtime configuration from environment variables.
// If envFilePath exists, it is loaded as a fallback source (.env style).
// Existing process environment values are not overridden by the .env file.
func LoadFromEnv(envFilePath string) (Config, error) {
	if envFilePath != "" {
		_ = godotenv.Load(envFilePath)
	}

	cfg := Config{
		ChannelID:        envOrDefault("CHANNEL_ID", "http-main"),
		Listen:           envOrDefault("LISTEN_ADDR", ":8080"),
		LogosSyncBaseURL: envOrDefault("LOGOS_SYNC_BASE_URL", "http://localhost:9000"),
		ProfilesDir:      envOrDefault("PROFILES_DIR", "profiles"),
		RabbitMQHost:     envOrDefault("RABBITMQ_HOST", ""),
		RabbitMQPort:     envOrDefault("RABBITMQ_PORT", "5672"),
		RabbitMQPassword: envOrDefault("RABBITMQ_PASSWORD", ""),
	}

	if cfg.LogosSyncBaseURL == "" {
		return Config{}, fmt.Errorf("LOGOS_SYNC_BASE_URL is required")
	}
	if cfg.ProfilesDir == "" {
		return Config{}, fmt.Errorf("PROFILES_DIR is required")
	}

	return cfg, nil
}

func (c Config) AMQPURL() string {
	if c.RabbitMQHost == "" {
		return ""
	}
	return fmt.Sprintf("amqp://logos:%s@%s:%s/", c.RabbitMQPassword, c.RabbitMQHost, c.RabbitMQPort)
}

func EnsureProfilesDir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create profiles dir: %w", err)
	}
	defaultPath := filepath.Join(dir, "default.yaml")
	if _, err := os.Stat(defaultPath); err == nil {
		return nil
	}
	examplePath := filepath.Join("examples", "profiles", "default.yaml")
	b, err := os.ReadFile(examplePath)
	if err != nil {
		return fmt.Errorf("read default example profile: %w", err)
	}
	if err := os.WriteFile(defaultPath, b, 0o644); err != nil {
		return fmt.Errorf("write default profile: %w", err)
	}
	return nil
}

func LoadProfiles(dir string) ([]coreProfile.Profile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read profiles dir: %w", err)
	}
	var all []coreProfile.Profile
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if len(name) < 5 || (name[len(name)-5:] != ".yaml" && name[len(name)-4:] != ".yml") {
			continue
		}
		b, err := os.ReadFile(dir + "/" + name)
		if err != nil {
			return nil, fmt.Errorf("read profile %s: %w", name, err)
		}
		profiles, err := coreProfile.ParseYAMLProfiles(b)
		if err != nil {
			return nil, fmt.Errorf("parse profile %s: %w", name, err)
		}
		all = append(all, profiles...)
	}
	if len(all) == 0 {
		return all, nil
	}
	if err := coreProfile.ValidateSet(all); err != nil {
		return nil, fmt.Errorf("validate profile set: %w", err)
	}
	return all, nil
}
