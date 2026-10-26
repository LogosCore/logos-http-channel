package config

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

type Config struct {
	ChannelID        string
	Listen           string
	LogosSyncBaseURL string
	HealthListen     string
	MaxBodyBytes     int64
	// CoreSyncTimeout bounds each client request's wait for core's sync reply.
	CoreSyncTimeout time.Duration
	// RabbitMQURL is the AMQP URL for registering with core's control plane.
	RabbitMQURL string
	// LogFormat is the stderr log format: "json" (default) or "text".
	LogFormat string
	// TrustedProxies are the peers whose X-Forwarded-For / X-Real-IP is
	// believed when logging the client IP.
	TrustedProxies []netip.Prefix
}

// Body size bounds: the smallest valid message (offset byte plus raw UUID),
// and a ceiling that keeps the reply limit (2× plus overhead) far from
// overflowing and a single request's memory sane.
const (
	minBodyBytes = 17
	maxBodyBytes = 1 << 30
)

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
		LogosSyncBaseURL: envOrDefault("LOGOS_SYNC_BASE_URL", "http://localhost:8002"),
	}

	var err error
	if cfg.Listen, err = listenAddr("LISTEN_ADDR", ":8080"); err != nil {
		return Config{}, err
	}
	if cfg.HealthListen, err = listenAddr("HEALTH_LISTEN_ADDR", ":8081"); err != nil {
		return Config{}, err
	}

	maxBody, err := strconv.ParseInt(envOrDefault("MAX_BODY_BYTES", "1048576"), 10, 64)
	if err != nil || maxBody < minBodyBytes || maxBody > maxBodyBytes {
		return Config{}, fmt.Errorf("MAX_BODY_BYTES must be an integer from %d to %d", minBodyBytes, int64(maxBodyBytes))
	}
	cfg.MaxBodyBytes = maxBody

	timeout, err := time.ParseDuration(envOrDefault("CORE_SYNC_TIMEOUT", "30s"))
	if err != nil || timeout <= 0 {
		return Config{}, fmt.Errorf("CORE_SYNC_TIMEOUT must be a positive duration (e.g. 30s)")
	}
	cfg.CoreSyncTimeout = timeout

	vhost := envOrDefault("RABBITMQ_VHOST", "/")
	cfg.RabbitMQURL = (&url.URL{
		Scheme: "amqp",
		User:   url.UserPassword(envOrDefault("RABBITMQ_USER", "guest"), envOrDefault("RABBITMQ_PASSWORD", "guest")),
		Host:   net.JoinHostPort(envOrDefault("RABBITMQ_HOST", "localhost"), envOrDefault("RABBITMQ_PORT", "5672")),
		// amqp091 takes the vhost from the path minus its leading "/", so the
		// default "/" vhost becomes the path "//".
		Path: "/" + vhost,
	}).String()

	cfg.LogFormat = strings.ToLower(envOrDefault("LOG_FORMAT", "json"))
	if cfg.LogFormat != "json" && cfg.LogFormat != "text" {
		return Config{}, fmt.Errorf("LOG_FORMAT must be json or text")
	}

	proxies, err := parsePrefixes(os.Getenv("TRUSTED_PROXIES"))
	if err != nil {
		return Config{}, fmt.Errorf("TRUSTED_PROXIES: %w", err)
	}
	cfg.TrustedProxies = proxies

	if cfg.LogosSyncBaseURL == "" {
		return Config{}, fmt.Errorf("LOGOS_SYNC_BASE_URL is required")
	}

	return cfg, nil
}

// listenAddr reads a listen address: host:port, :port, or a bare port, which
// means all interfaces (":port"). Anything else fails here rather than when
// the server starts listening.
func listenAddr(key, fallback string) (string, error) {
	v := strings.TrimSpace(envOrDefault(key, fallback))
	if _, err := strconv.ParseUint(v, 10, 16); err == nil {
		v = ":" + v
	}
	_, port, err := net.SplitHostPort(v)
	if err == nil {
		_, err = strconv.ParseUint(port, 10, 16)
	}
	if err != nil {
		return "", fmt.Errorf("%s must be a port or host:port (e.g. :8080), got %q", key, v)
	}
	return v, nil
}

// parsePrefixes parses a comma-separated list of IPs and CIDRs.
func parsePrefixes(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, f := range strings.Split(s, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if p, err := netip.ParsePrefix(f); err == nil {
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(f)
		if err != nil {
			return nil, fmt.Errorf("%q is not an IP or CIDR", f)
		}
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	return out, nil
}
