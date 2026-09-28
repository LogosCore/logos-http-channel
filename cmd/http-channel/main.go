package main

import (
	"flag"
	"log"

	"github.com/logoscore/logos-http-channel/internal/config"
	httpserver "github.com/logoscore/logos-http-channel/internal/transport/http/httpserver"
)

func main() {
	var envFile string
	flag.StringVar(&envFile, "config", ".env", "path to .env fallback file")
	flag.Parse()

	cfg, err := config.LoadFromEnv(envFile)
	if err != nil {
		log.Fatalf("config load failed: %v", err)
	}

	srv := httpserver.New(cfg.Listen, cfg.ChannelID, cfg.LogosSyncBaseURL)
	log.Printf("logos-http-channel listening on %s (channel_id=%s, logos=%s)", srv.Addr, cfg.ChannelID, cfg.LogosSyncBaseURL)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
