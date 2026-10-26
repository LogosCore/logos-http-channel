// Package management declares what core may manage on this channel (body
// limit and log level; drain/resume/check_core actions, each available only
// when it changes something; request counters) and
// binds it to the client handler. How the channel reaches core (sync URL,
// timeout) is env-only and deliberately not manageable. Core drives it over the
// module's rpc_queue via logos-golang-module-core pkg/mgmt.
package management

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/logoscore/logos-golang-module-core/pkg/mgmt"
	protocol "github.com/logoscore/logos-golang-protocol/protocol"
	"github.com/logoscore/logos-http-channel/internal/config"
	httpserver "github.com/logoscore/logos-http-channel/internal/transport/http/httpserver"
)

// defaultWS are the WebSocket defaults the manifest declares.
var defaultWS = httpserver.DefaultWebSocketConfig

// logLevels maps the log_level values onto slog levels, most verbose first.
// Each description says what the channel logs at that level.
var logLevels = []struct {
	value, label, description string
	level                     slog.Level
}{
	{"debug", "Debug", "Every request outcome, plus everything below.", slog.LevelDebug},
	{"info", "Info", "Rejected client requests, startup and config changes, plus core problems.", slog.LevelInfo},
	{"warn", "Warn", "Core problems only: sync failed, rejected or bad reply.", slog.LevelWarn},
	{"error", "Error", "Errors only (the channel has none yet).", slog.LevelError},
}

const defaultLogLevel = "info"

// logLevelOptions are the log_level enum options.
func logLevelOptions() []protocol.EnumOption {
	out := make([]protocol.EnumOption, len(logLevels))
	for i, l := range logLevels {
		out[i] = protocol.EnumOption{Value: l.value, Label: l.label, Description: l.description}
	}
	return out
}

// slogLevel returns the slog level for a log_level value; unknown values
// (never valid against the manifest) fall back to info.
func slogLevel(v any) slog.Level {
	for _, l := range logLevels {
		if l.value == v {
			return l.level
		}
	}
	return slog.LevelInfo
}

// Property bounds.
const (
	minBodyBytes = 17       // offset byte plus raw UUID
	maxBodyBytes = 64 << 20 // 64 MiB
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
				Description: "Largest accepted request body or WebSocket message. Minions choose their own message size, so lowering it can break minions already deployed: watch Too large.",
				Group:       "Limits", Order: 1, Unit: "bytes",
				Default: cfg.MaxBodyBytes, Min: minBodyBytes, Max: bodyMax,
			}, Apply: protocol.ApplyLive},
			{FieldSpec: protocol.FieldSpec{
				Key: "log_level", Type: protocol.FieldEnum, Label: "Log level",
				Description: "Level of the channel's own output (stderr). The Logs tab has its own level.",
				Group:       "Logging", Order: 2,
				Default: defaultLogLevel,
				Options: logLevelOptions(),
			}, Apply: protocol.ApplyLive},
			{FieldSpec: protocol.FieldSpec{
				Key: "ws_max_connections", Type: protocol.FieldInt, Label: "Max WebSocket connections",
				Description: "Open WebSocket connections allowed at once; further handshakes get a 404.",
				Group:       "WebSocket", Order: 3,
				Default: defaultWS.MaxConnections, Min: 1, Max: 100000,
			}, Apply: protocol.ApplyLive},
			{FieldSpec: protocol.FieldSpec{
				Key: "ws_max_inflight", Type: protocol.FieldInt, Label: "Messages in flight per connection",
				Description: "Messages of one connection relayed to core at once. Messages with the same minion UUID are always relayed in order. Applies to new connections.",
				Group:       "WebSocket", Order: 4,
				Default: defaultWS.MaxInflight, Min: 1, Max: 64,
			}, Apply: protocol.ApplyLive},
			{FieldSpec: protocol.FieldSpec{
				Key: "ws_ping_interval", Type: protocol.FieldDuration, Label: "Ping interval",
				Description: "How often the channel pings each connection (WebSocket control frames, never sent to core). Keep it below proxy idle timeouts. Applies to new connections.",
				Group:       "WebSocket", Order: 5,
				Default: defaultWS.PingInterval.String(), Min: "5s", Max: "5m",
			}, Apply: protocol.ApplyLive},
			{FieldSpec: protocol.FieldSpec{
				Key: "ws_idle_timeout", Type: protocol.FieldDuration, Label: "Idle timeout",
				Description: "A connection that sends no message for this long is closed. Applies to new connections.",
				Group:       "WebSocket", Order: 6,
				Default: defaultWS.IdleTimeout.String(), Min: "10s", Max: "24h",
			}, Apply: protocol.ApplyLive},
		},
		Actions: []protocol.ActionSpec{
			{
				Name: "drain", Label: "Drain", Risk: protocol.RiskHigh,
				Description: "Stop serving clients: every request and WebSocket handshake gets a 404 until resumed, and open WebSockets are closed. Shown in the Draining status value.",
				Confirm:     "Clients will receive 404 for every request until the channel is resumed.",
				AvailableWhen: []protocol.ActionCondition{
					{Status: "draining", Op: protocol.OpEq, Value: false, Message: "Already draining"},
				},
			},
			{
				Name: "resume", Label: "Resume", Risk: protocol.RiskLow, Description: "Serve clients again after a drain.",
				AvailableWhen: []protocol.ActionCondition{
					{Status: "draining", Op: protocol.OpEq, Value: true, Message: "Not draining"},
				},
			},
			{
				Name: "close_connections", Label: "Close WebSockets", Risk: protocol.RiskHigh,
				Description: "Close every open WebSocket connection after its in-flight messages are answered. Minions may reconnect at once.",
				Confirm:     "All connected minions will be disconnected.",
				AvailableWhen: []protocol.ActionCondition{
					{Status: "ws_connections_open", Op: protocol.OpGt, Value: 0, Message: "No WebSocket connection is open"},
				},
			},
			{
				Name: "check_core", Label: "Check core", Risk: protocol.RiskLow, TimeoutSeconds: 10,
				Description: "Probe core's sync endpoint and report reachability and latency.",
			},
		},
		Status: []protocol.StatusSpec{
			{Key: "requests_total", Type: protocol.FieldInt, Label: "HTTP requests",
				Description: "Plain requests and WebSocket handshakes."},
			{Key: "succeeded_total", Type: protocol.FieldInt, Label: "HTTP succeeded"},
			{Key: "failed_total", Type: protocol.FieldInt, Label: "HTTP failed (404)"},
			{Key: "ws_connections_open", Type: protocol.FieldInt, Label: "WebSocket connections"},
			{Key: "ws_connections_total", Type: protocol.FieldInt, Label: "WebSocket connections opened"},
			{Key: "ws_messages_total", Type: protocol.FieldInt, Label: "WebSocket messages"},
			{Key: "ws_messages_failed_total", Type: protocol.FieldInt, Label: "WebSocket messages failed",
				Description: "Each one closed its connection."},
			{Key: "core_errors_total", Type: protocol.FieldInt, Label: "Core errors",
				Description: "Core unreachable, non-200, or invalid reply (both transports)."},
			{Key: "too_large_total", Type: protocol.FieldInt, Label: "Too large",
				Description: "Requests and WebSocket messages over the max request body (both transports). Minions sending them fail until the limit is raised."},
			{Key: "rejected_by_core_total", Type: protocol.FieldInt, Label: "Rejected by core",
				Description: "Core answered 403: this instance was not registered."},
			{Key: "last_core_error", Type: protocol.FieldString, Label: "Last core error"},
			{Key: "last_core_error_at", Type: protocol.FieldString, Label: "Last core error at"},
			// Draining needs an operator's eye: clients get 404 for everything.
			{Key: "draining", Type: protocol.FieldBool, Label: "Draining", Alert: true},
		},
	}
}

// New builds the management server for h from cfg. log_level drives level,
// the level of the process's own log output.
func New(cfg config.Config, h *httpserver.Handler, level *slog.LevelVar) (*mgmt.Server, error) {
	s, err := mgmt.New(Manifest(cfg), mgmt.WithSource(protocol.Source{
		Service: protocol.ServiceChannel, Instance: cfg.ChannelID,
	}))
	if err != nil {
		return nil, err
	}
	s.OnApply(func(ctx context.Context, live map[string]any) error { return apply(ctx, h, level, live) })
	s.Status(func() map[string]any { return status(h) })
	s.Action("drain", func(_ context.Context, _ map[string]any, actor *protocol.Actor) (any, error) {
		was := h.Draining()
		h.SetDraining(true)
		slog.Info("management: drained", "by", actorName(actor))
		return map[string]any{"draining": true, "was_draining": was}, nil
	})
	s.Action("resume", func(_ context.Context, _ map[string]any, actor *protocol.Actor) (any, error) {
		was := h.Draining()
		h.SetDraining(false)
		slog.Info("management: resumed", "by", actorName(actor))
		return map[string]any{"draining": false, "was_draining": was}, nil
	})
	s.Action("close_connections", func(_ context.Context, _ map[string]any, actor *protocol.Actor) (any, error) {
		n := h.CloseWebSockets("closed_by_admin")
		slog.Info("management: websocket connections closed", "by", actorName(actor), "connections", n)
		return map[string]any{"closed": n}, nil
	})
	s.Action("check_core", func(ctx context.Context, _ map[string]any, _ *protocol.Actor) (any, error) {
		return CheckCore(ctx, h.Client(), h.SyncEndpoint()), nil
	})
	return s, nil
}

// apply maps live property values onto the handler's runtime config and the
// log level.
func apply(ctx context.Context, h *httpserver.Handler, level *slog.LevelVar, live map[string]any) error {
	prev := h.Config()
	next := prev
	if n, ok := protocol.AsInt64(live["max_body_bytes"]); ok {
		next.MaxBodyBytes = n
	}
	if n, ok := protocol.AsInt64(live["ws_max_connections"]); ok {
		next.WebSocket.MaxConnections = int(n)
	}
	if n, ok := protocol.AsInt64(live["ws_max_inflight"]); ok {
		next.WebSocket.MaxInflight = int(n)
	}
	for key, dst := range map[string]*time.Duration{
		"ws_ping_interval": &next.WebSocket.PingInterval,
		"ws_idle_timeout":  &next.WebSocket.IdleTimeout,
	} {
		if v := live[key]; v != nil {
			d, err := protocol.ParseDuration(v)
			if err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
			*dst = d
		}
	}
	lvl := slogLevel(live["log_level"])
	if next == prev && level.Level() == lvl {
		return nil
	}
	h.SetConfig(next)
	level.Set(lvl)
	ws := next.WebSocket
	slog.Info("management: config applied", "by", actorName(mgmt.ActorFromContext(ctx)),
		"max_body_bytes", next.MaxBodyBytes, "log_level", lvl.String(),
		"ws_max_connections", ws.MaxConnections, "ws_max_inflight", ws.MaxInflight,
		"ws_ping_interval", ws.PingInterval.String(), "ws_idle_timeout", ws.IdleTimeout.String())
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
		"too_large_total":        st.TooLargeTotal,
		"draining":               st.Draining,

		"ws_connections_open":      st.WSConnectionsOpen,
		"ws_connections_total":     st.WSConnectionsTotal,
		"ws_messages_total":        st.WSMessagesTotal,
		"ws_messages_failed_total": st.WSMessagesFailed,
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
