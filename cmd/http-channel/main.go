package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/logoscore/logos-golang-channel-core/v2/pkg/lifecycle"
	"github.com/logoscore/logos-golang-channel-core/v2/pkg/logstream"
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

	// Logs go to stderr at the managed log_level, and to core's live log tail
	// (which can ask for debug on its own) through the hub.
	var level slog.LevelVar
	logs := logstream.New(cfg.ChannelID)
	slog.SetDefault(slog.New(logs.Handler(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: &level}))))

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
	mgmtSrv, err := management.New(cfg, handler, &level)
	if err != nil {
		slog.Warn("management disabled", "err", err)
	}
	health := httpserver.NewHealth(cfg.HealthListen)
	for _, s := range []*http.Server{srv, health} {
		go func() {
			if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatal(err)
			}
		}()
	}
	slog.Info("logos-http-channel listening", "addr", srv.Addr, "health", health.Addr, "channel_id", cfg.ChannelID,
		"logos", cfg.LogosSyncBaseURL, "core_sync_timeout", cfg.CoreSyncTimeout, "max_body_bytes", cfg.MaxBodyBytes)

	// The control-plane session outlives ctx: in-flight requests must drain
	// while core still treats this instance as registered, so it is stopped
	// after the servers.
	regCtx, stopReg := context.WithCancel(context.Background())
	regDone := make(chan struct{})
	go func() {
		defer close(regDone)
		mc := module.Config{
			AMQPURL: cfg.RabbitMQURL,
			Identity: lifecycle.Identity{
				ModuleType: "channel",
				ModuleName: "http",
				Instance:   cfg.ChannelID,
				Version:    version,
			},
		}
		// The live log tail is served on the management queue.
		if mgmtSrv != nil {
			mc.Management, mc.Logs = mgmtSrv, logs
		}
		err := module.Run(regCtx, mc)
		if err != nil {
			log.Fatalf("control plane: %v", err)
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, s := range []*http.Server{srv, health} {
		if err := s.Shutdown(shutdownCtx); err != nil {
			slog.Warn("shutdown", "addr", s.Addr, "err", err)
		}
	}
	stopReg()
	<-regDone
}
