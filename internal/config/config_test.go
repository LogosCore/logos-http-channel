package config

import "testing"

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
