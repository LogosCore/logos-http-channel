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

	"github.com/logoscore/logos-golang-channel-core/v2/pkg/lifecycle"
	"github.com/logoscore/logos-http-channel/internal/config"
	"github.com/logoscore/logos-http-channel/internal/registration"
	httpserver "github.com/logoscore/logos-http-channel/internal/transport/http/httpserver"
)

// version is the module build version reported at registration; set with
// -ldflags "-X main.version=...".
var version = "dev"

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

	handler := httpserver.NewHandler(cfg.ChannelID, cfg.LogosSyncBaseURL, httpserver.RuntimeConfig{
		MaxBodyBytes: cfg.MaxBodyBytes,
	})
	srv := httpserver.New(cfg.Listen, handler)
	health := httpserver.NewHealth(cfg.HealthListen)
	for _, s := range []*http.Server{srv, health} {
		go func() {
			if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatal(err)
			}
		}()
	}
	log.Printf("logos-http-channel listening on %s, health on %s (channel_id=%s, logos=%s, max_body=%d)",
		srv.Addr, health.Addr, cfg.ChannelID, cfg.LogosSyncBaseURL, cfg.MaxBodyBytes)

	// Registration outlives ctx: in-flight requests must drain while core still
	// treats this instance as registered, so it is stopped after the servers.
	regCtx, stopReg := context.WithCancel(context.Background())
	regDone := make(chan struct{})
	go func() {
		defer close(regDone)
		registration.Run(regCtx, cfg.RabbitMQURL, lifecycle.Identity{
			ModuleType: "channel",
			ModuleName: "http",
			Instance:   cfg.ChannelID,
			Version:    version,
			RPCQueue:   "logos.channel.rpc." + cfg.ChannelID,
		})
	}()

	<-ctx.Done()
	log.Printf("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, s := range []*http.Server{srv, health} {
		if err := s.Shutdown(shutdownCtx); err != nil {
			log.Printf("shutdown %s: %v", s.Addr, err)
		}
	}
	stopReg()
	<-regDone
}
