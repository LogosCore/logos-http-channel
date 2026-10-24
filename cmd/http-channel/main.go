package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"

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

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv := httpserver.New(cfg.Listen, cfg.ChannelID, cfg.LogosSyncBaseURL)
	go func() {
		log.Printf("logos-http-channel listening on %s (channel_id=%s, logos=%s)", srv.Addr, cfg.ChannelID, cfg.LogosSyncBaseURL)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()

	<-ctx.Done()
	log.Printf("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}
