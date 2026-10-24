// Package management declares what core may manage on this channel (live
// limits, timeouts and log level; drain/resume/check_core actions; request
// counters) and binds it to the client handler. Core drives it over the
// module's rpc_queue via logos-golang-channel-core/v2 pkg/mgmt.
package management

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/logoscore/logos-golang-channel-core/v2/pkg/lifecycle"
	"github.com/logoscore/logos-golang-channel-core/v2/pkg/mgmt"
	protocol "github.com/logoscore/logos-golang-protocol/protocol"
	"github.com/logoscore/logos-http-channel/internal/config"
	httpserver "github.com/logoscore/logos-http-channel/internal/transport/http/httpserver"
)

// Property bounds.
const (
	minBodyBytes   = 17       // offset byte plus raw UUID
	maxBodyBytes   = 64 << 20 // 64 MiB
	defaultTimeout = "30s"
)

// Manifest builds the management manifest. Each property's default is the
// value this process runs with from its env config.
func Manifest(cfg config.Config) protocol.ManagementManifest {
	// Keep an env value above the cap valid rather than refusing to start.
	bodyMax := max(int64(maxBodyBytes), cfg.MaxBodyBytes)
	return protocol.ManagementManifest{
		SchemaVersion: protocol.ManagementSchemaV1,
		Properties: []protocol.PropertySpec{
			{FieldSpec: protocol.FieldSpec{
				Key: "max_body_bytes", Type: protocol.FieldBytes, Label: "Max request body",
				Description: "Largest accepted client request body. The core reply limit scales with it.",
				Group:       "Limits", Order: 1, Unit: "bytes",
				Default: cfg.MaxBodyBytes, Min: minBodyBytes, Max: bodyMax,
			}, Apply: protocol.ApplyLive},
			{FieldSpec: protocol.FieldSpec{
				Key: "core_sync_timeout", Type: protocol.FieldDuration, Label: "Core sync timeout",
				Description: "How long a client request waits for core's sync reply.",
				Group:       "Core", Order: 2,
				Default: defaultTimeout, Min: "1s", Max: "120s",
			}, Apply: protocol.ApplyLive},
			{FieldSpec: protocol.FieldSpec{
				Key: "log_level", Type: protocol.FieldEnum, Label: "Log level",
				Description: "info logs failures only; debug also logs every request outcome.",
				Group:       "Logging", Order: 3,
				Default: httpserver.LogInfo,
				Options: []protocol.EnumOption{{Value: httpserver.LogInfo, Label: "Info"}, {Value: httpserver.LogDebug, Label: "Debug"}},
			}, Apply: protocol.ApplyLive},
			{FieldSpec: protocol.FieldSpec{
				Key: "logos_sync_base_url", Type: protocol.FieldURL, Label: "Core sync base URL",
				Description: "Base URL of core's /api/channel/sync endpoint. Changing it requires a restart.",
				Group:       "Core", Order: 4,
				Default: cfg.LogosSyncBaseURL,
			}, Apply: protocol.ApplyRestart},
		},
		Actions: []protocol.ActionSpec{
			{
				Name: "drain", Label: "Drain", Risk: protocol.RiskHigh,
				Description: "Stop serving clients: every request gets a 404 and the heartbeat reports draining.",
				Confirm:     "Clients will receive 404 for every request until the channel is resumed.",
			},
			{Name: "resume", Label: "Resume", Risk: protocol.RiskLow, Description: "Serve clients again after a drain."},
			{
				Name: "check_core", Label: "Check core", Risk: protocol.RiskLow, TimeoutSeconds: 10,
				Description: "Probe core's sync endpoint and report reachability and latency.",
			},
		},
		Status: []protocol.StatusSpec{
			{Key: "requests_total", Type: protocol.FieldInt, Label: "Requests"},
			{Key: "succeeded_total", Type: protocol.FieldInt, Label: "Succeeded"},
			{Key: "failed_total", Type: protocol.FieldInt, Label: "Failed (404)"},
			{Key: "core_errors_total", Type: protocol.FieldInt, Label: "Core errors",
				Description: "Core unreachable, non-200, or invalid reply."},
			{Key: "rejected_by_core_total", Type: protocol.FieldInt, Label: "Rejected by core",
				Description: "Core answered 403: this instance was not registered."},
			{Key: "last_core_error", Type: protocol.FieldString, Label: "Last core error"},
			{Key: "last_core_error_at", Type: protocol.FieldString, Label: "Last core error at"},
			{Key: "draining", Type: protocol.FieldBool, Label: "Draining"},
		},
	}
}

// New builds the management server for h from cfg.
func New(cfg config.Config, h *httpserver.Handler) (*mgmt.Server, error) {
	s, err := mgmt.New(Manifest(cfg), mgmt.WithSource(protocol.Source{
		Service: protocol.ServiceChannel, Instance: cfg.ChannelID,
	}))
	if err != nil {
		return nil, err
	}
	s.OnApply(func(ctx context.Context, live map[string]any) error { return apply(ctx, h, live) })
	s.Status(func() map[string]any { return status(h) })
	s.Action("drain", func(_ context.Context, _ map[string]any, actor *protocol.Actor) (any, error) {
		was := h.Draining()
		h.SetDraining(true)
		log.Printf("management: drain by %s", actorName(actor))
		return map[string]any{"draining": true, "was_draining": was}, nil
	})
	s.Action("resume", func(_ context.Context, _ map[string]any, actor *protocol.Actor) (any, error) {
		was := h.Draining()
		h.SetDraining(false)
		log.Printf("management: resume by %s", actorName(actor))
		return map[string]any{"draining": false, "was_draining": was}, nil
	})
	s.Action("check_core", func(ctx context.Context, _ map[string]any, _ *protocol.Actor) (any, error) {
		return CheckCore(ctx, h.Client(), h.SyncEndpoint()), nil
	})
	return s, nil
}

// LifecycleStatus reports draining in heartbeats while h is drained.
func LifecycleStatus(h *httpserver.Handler) lifecycle.Option {
	return lifecycle.WithStatus(func() lifecycle.Status {
		if h.Draining() {
			return lifecycle.StatusDraining
		}
		return lifecycle.StatusHealthy
	})
}

// apply maps live property values onto the handler's runtime config.
func apply(ctx context.Context, h *httpserver.Handler, live map[string]any) error {
	prev := h.Config()
	next := prev
	if n, ok := protocol.AsInt64(live["max_body_bytes"]); ok {
		next.MaxBodyBytes = n
	}
	if v := live["core_sync_timeout"]; v != nil {
		d, err := protocol.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("core_sync_timeout: %w", err)
		}
		next.CoreSyncTimeout = d
	}
	if lvl, ok := live["log_level"].(string); ok {
		next.LogLevel = lvl
	}
	if next == prev {
		return nil
	}
	h.SetConfig(next)
	log.Printf("management: config applied by %s (max_body=%d core_sync_timeout=%s log_level=%s)",
		actorName(mgmt.ActorFromContext(ctx)), next.MaxBodyBytes, next.CoreSyncTimeout, next.LogLevel)
	return nil
}

func status(h *httpserver.Handler) map[string]any {
	st := h.Stats()
	out := map[string]any{
		"requests_total":         st.RequestsTotal,
		"succeeded_total":        st.SucceededTotal,
		"failed_total":           st.FailedTotal,
		"core_errors_total":      st.CoreErrorsTotal,
		"rejected_by_core_total": st.RejectedByCore,
		"draining":               st.Draining,
	}
	if st.LastCoreError != "" {
		out["last_core_error"] = st.LastCoreError
		out["last_core_error_at"] = st.LastCoreErrorAt.Format(time.RFC3339)
	}
	return out
}

// CoreCheck is the check_core result.
type CoreCheck struct {
	Reachable bool   `json:"reachable"`
	Status    int    `json:"status"`
	LatencyMS int64  `json:"latency_ms"`
	Error     string `json:"error,omitempty"`
}

// CheckCore POSTs an empty JSON object to core's sync URL. Core answers 400
// for the invalid envelope, which proves it is reachable; only a transport
// error or a 5xx counts as unreachable.
func CheckCore(ctx context.Context, client *http.Client, syncURL string) CoreCheck {
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, syncURL, bytes.NewReader([]byte("{}")))
	if err != nil {
		return CoreCheck{Error: err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return CoreCheck{LatencyMS: latency, Error: err.Error()}
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	return CoreCheck{Reachable: resp.StatusCode < 500, Status: resp.StatusCode, LatencyMS: latency}
}

func actorName(a *protocol.Actor) string {
	switch {
	case a == nil:
		return "core"
	case a.Name != "":
		return fmt.Sprintf("%s %s (%s)", a.Type, a.Name, a.ID)
	default:
		return a.Type + " " + a.ID
	}
}
