package config

import (
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

func TestLoadFromEnv_Defaults(t *testing.T) {
	cfg, err := LoadFromEnv("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxBodyBytes != 1<<20 {
		t.Fatalf("MaxBodyBytes = %d, want %d", cfg.MaxBodyBytes, 1<<20)
	}
	if cfg.HealthListen != ":8081" {
		t.Fatalf("HealthListen = %s, want :8081", cfg.HealthListen)
	}
	if cfg.CoreSyncTimeout != 30*time.Second {
		t.Fatalf("CoreSyncTimeout = %s, want 30s", cfg.CoreSyncTimeout)
	}
}

func TestLoadFromEnv_CoreSyncTimeout(t *testing.T) {
	t.Setenv("CORE_SYNC_TIMEOUT", "5s")
	cfg, err := LoadFromEnv("")
	if err != nil || cfg.CoreSyncTimeout != 5*time.Second {
		t.Fatalf("CoreSyncTimeout = %s (%v), want 5s", cfg.CoreSyncTimeout, err)
	}
	for _, v := range []string{"abc", "0s", "-1s", "30"} {
		t.Setenv("CORE_SYNC_TIMEOUT", v)
		if _, err := LoadFromEnv(""); err == nil {
			t.Fatalf("CORE_SYNC_TIMEOUT=%s: expected error", v)
		}
	}
}

func TestLoadFromEnv_MaxBodyBytes(t *testing.T) {
	t.Setenv("MAX_BODY_BYTES", "4096")
	cfg, err := LoadFromEnv("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxBodyBytes != 4096 {
		t.Fatalf("MaxBodyBytes = %d, want 4096", cfg.MaxBodyBytes)
	}
}

func TestLoadFromEnv_InvalidMaxBodyBytes(t *testing.T) {
	for _, v := range []string{"abc", "-1", "16"} {
		t.Setenv("MAX_BODY_BYTES", v)
		if _, err := LoadFromEnv(""); err == nil {
			t.Fatalf("MAX_BODY_BYTES=%s: expected error", v)
		}
	}
}

func TestLoadFromEnv_RabbitMQURL(t *testing.T) {
	cases := []struct {
		vhost, password, want string
	}{
		{"", "guest", "/"},
		{"/", "p@ss/word", "/"},
		{"logos", "guest", "logos"},
	}
	for _, c := range cases {
		t.Setenv("RABBITMQ_VHOST", c.vhost)
		t.Setenv("RABBITMQ_PASSWORD", c.password)
		cfg, err := LoadFromEnv("")
		if err != nil {
			t.Fatal(err)
		}
		uri, err := amqp.ParseURI(cfg.RabbitMQURL)
		if err != nil {
			t.Fatalf("vhost %q: parse %s: %v", c.vhost, cfg.RabbitMQURL, err)
		}
		if uri.Vhost != c.want || uri.Password != c.password || uri.Host != "localhost" || uri.Port != 5672 {
			t.Fatalf("vhost %q: got %+v, want vhost %q password %q", c.vhost, uri, c.want, c.password)
		}
	}
}

func TestLoadFromEnv_LogFormatAndTrustedProxies(t *testing.T) {
	cfg, err := LoadFromEnv("")
	if err != nil || cfg.LogFormat != "json" || len(cfg.TrustedProxies) != 0 {
		t.Fatalf("defaults: %+v (%v)", cfg, err)
	}
	t.Setenv("LOG_FORMAT", "Text")
	t.Setenv("TRUSTED_PROXIES", "10.0.0.0/8, 192.168.1.7 ,::1")
	cfg, err = LoadFromEnv("")
	if err != nil || cfg.LogFormat != "text" || len(cfg.TrustedProxies) != 3 || cfg.TrustedProxies[1].String() != "192.168.1.7/32" {
		t.Fatalf("got %+v (%v)", cfg, err)
	}
	for _, v := range [][2]string{{"LOG_FORMAT", "xml"}, {"TRUSTED_PROXIES", "not-an-ip"}} {
		t.Setenv("LOG_FORMAT", "json")
		t.Setenv("TRUSTED_PROXIES", "")
		t.Setenv(v[0], v[1])
		if _, err := LoadFromEnv(""); err == nil {
			t.Errorf("%s=%s: expected error", v[0], v[1])
		}
	}
}
