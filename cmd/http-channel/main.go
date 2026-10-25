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
	"github.com/logoscore/logos-golang-channel-core/v2/pkg/module"
	"github.com/logoscore/logos-http-channel/internal/config"
	"github.com/logoscore/logos-http-channel/internal/management"
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

	handler := httpserver.NewHandler(cfg.ChannelID, httpserver.CoreConfig{
		SyncBaseURL: cfg.LogosSyncBaseURL,
		SyncTimeout: cfg.CoreSyncTimeout,
	}, httpserver.RuntimeConfig{
		MaxBodyBytes: cfg.MaxBodyBytes,
	})
	srv := httpserver.New(cfg.Listen, handler)
	// Without management (e.g. an env value the manifest rejects) the channel
	// still serves and registers; core just cannot manage it.
	mgmtSrv, err := management.New(cfg, handler)
	if err != nil {
		log.Printf("management disabled: %v", err)
	}
	health := httpserver.NewHealth(cfg.HealthListen)
	for _, s := range []*http.Server{srv, health} {
		go func() {
			if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatal(err)
			}
		}()
	}
	log.Printf("logos-http-channel listening on %s, health on %s (channel_id=%s, logos=%s, core_sync_timeout=%s, max_body=%d)",
		srv.Addr, health.Addr, cfg.ChannelID, cfg.LogosSyncBaseURL, cfg.CoreSyncTimeout, cfg.MaxBodyBytes)

	// The control-plane session outlives ctx: in-flight requests must drain
	// while core still treats this instance as registered, so it is stopped
	// after the servers.
	regCtx, stopReg := context.WithCancel(context.Background())
	regDone := make(chan struct{})
	go func() {
		defer close(regDone)
		err := module.Run(regCtx, module.Config{
			AMQPURL: cfg.RabbitMQURL,
			Identity: lifecycle.Identity{
				ModuleType: "channel",
				ModuleName: "http",
				Instance:   cfg.ChannelID,
				Version:    version,
			},
			Management: mgmtSrv,
		})
		if err != nil {
			log.Fatalf("control plane: %v", err)
		}
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
