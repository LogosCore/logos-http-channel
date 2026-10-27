package management

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	protocol "github.com/logoscore/logos-golang-protocol/protocol"
	"github.com/logoscore/logos-http-channel/internal/config"
	httpserver "github.com/logoscore/logos-http-channel/internal/transport/http/httpserver"
)

func testConfig(syncURL string) config.Config {
	return config.Config{ChannelID: "http-1", LogosSyncBaseURL: syncURL, MaxBodyBytes: 1 << 20}
}

func TestManifestValid(t *testing.T) {
	m := Manifest(testConfig("http://core:8002"))
	if err := protocol.ValidateManifest(m); err != nil {
		t.Fatalf("ValidateManifest: %v", err)
	}
	if m.Properties[0].Default != int64(1<<20) {
		t.Errorf("default not taken from env config: %+v", m.Properties)
	}
	// How the channel reaches core is env-only.
	for _, p := range m.Properties {
		if p.Key == "core_sync_timeout" || p.Key == "logos_sync_base_url" {
			t.Errorf("core connection setting %q is manageable", p.Key)
		}
	}
	// An env body cap above the declared max still yields a valid manifest.
	big := testConfig("http://core:8002")
	big.MaxBodyBytes = 128 << 20
	if err := protocol.ValidateManifest(Manifest(big)); err != nil {
		t.Errorf("large env cap: %v", err)
	}
}

func newServer(t *testing.T, syncBaseURL string) (*httpserver.Handler, func(protocol.Envelope) protocol.ReplyEnvelope) {
	t.Helper()
	h, _, handle := newServerWithLevel(t, syncBaseURL)
	return h, handle
}

func newServerWithLevel(t *testing.T, syncBaseURL string) (*httpserver.Handler, *slog.LevelVar, func(protocol.Envelope) protocol.ReplyEnvelope) {
	t.Helper()
	cfg := testConfig(syncBaseURL)
	h := httpserver.NewHandler(cfg.ChannelID, httpserver.CoreConfig{SyncBaseURL: cfg.LogosSyncBaseURL}, httpserver.RuntimeConfig{MaxBodyBytes: cfg.MaxBodyBytes})
	var level slog.LevelVar
	s, err := New(cfg, h, &level)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h, &level, func(req protocol.Envelope) protocol.ReplyEnvelope { return s.Handle(context.Background(), req) }
}

func envelope(t *testing.T, typ string, payload any) protocol.Envelope {
	t.Helper()
	raw, _ := json.Marshal(payload)
	id := protocol.NewID()
	return protocol.Envelope{MessageID: id, CorrelationID: id, Type: typ, Version: protocol.EnvelopeVersionV1, Payload: raw}
}

func TestApplyUpdatesHandlerLive(t *testing.T) {
	h, level, handle := newServerWithLevel(t, "http://core:8002")
	// The startup apply (register reply), with nothing managed yet.
	handle(envelope(t, protocol.TypeMgmtConfigApply, protocol.ConfigApplyRequest{}))
	rep := handle(envelope(t, protocol.TypeMgmtConfigApply, protocol.ConfigApplyRequest{
		ManagementConfig: protocol.ManagementConfig{Revision: 1, Values: map[string]any{
			"max_body_bytes": 4096,
			"log_level":      "debug",
		}},
	}))
	var out protocol.ConfigApplyReply
	if err := json.Unmarshal(rep.Payload, &out); err != nil || rep.Status != protocol.StatusOK {
		t.Fatalf("reply = %+v (%v)", rep, err)
	}
	if len(out.RestartRequired) != 0 || len(out.Rejected) != 0 {
		t.Errorf("reply = %+v", out)
	}
	want := httpserver.RuntimeConfig{MaxBodyBytes: 4096, WebSocket: httpserver.DefaultWebSocketConfig}
	if got := h.Config(); got != want || level.Level() != slog.LevelDebug {
		t.Errorf("config = %+v level = %s, want %+v debug", got, level.Level(), want)
	}

	// Out-of-range values are rejected and the previous value kept.
	rep = handle(envelope(t, protocol.TypeMgmtConfigApply, protocol.ConfigApplyRequest{
		ManagementConfig: protocol.ManagementConfig{Revision: 2, Values: map[string]any{"max_body_bytes": 5, "log_level": "trace"}},
	}))
	_ = json.Unmarshal(rep.Payload, &out)
	if len(out.Rejected) != 2 || h.Config().MaxBodyBytes != 4096 || level.Level() != slog.LevelDebug {
		t.Errorf("rejected = %+v config = %+v", out.Rejected, h.Config())
	}
}

func TestLogLevels(t *testing.T) {
	_, level, handle := newServerWithLevel(t, "http://core:8002")
	handle(envelope(t, protocol.TypeMgmtConfigApply, protocol.ConfigApplyRequest{}))
	for i, c := range []struct {
		value string
		want  slog.Level
	}{
		{"warn", slog.LevelWarn},
		{"error", slog.LevelError},
		{"debug", slog.LevelDebug},
		{"info", slog.LevelInfo},
	} {
		rep := handle(envelope(t, protocol.TypeMgmtConfigApply, protocol.ConfigApplyRequest{
			ManagementConfig: protocol.ManagementConfig{Revision: int64(i + 1), Values: map[string]any{"log_level": c.value}},
		}))
		var out protocol.ConfigApplyReply
		if err := json.Unmarshal(rep.Payload, &out); err != nil || len(out.Rejected) != 0 {
			t.Fatalf("%s: reply = %+v (%v)", c.value, rep, err)
		}
		if level.Level() != c.want {
			t.Errorf("log_level=%s: level = %s, want %s", c.value, level.Level(), c.want)
		}
	}
	// Absent from core's values: back to the default.
	handle(envelope(t, protocol.TypeMgmtConfigApply, protocol.ConfigApplyRequest{
		ManagementConfig: protocol.ManagementConfig{Revision: 10, Values: map[string]any{"log_level": "warn"}},
	}))
	handle(envelope(t, protocol.TypeMgmtConfigApply, protocol.ConfigApplyRequest{
		ManagementConfig: protocol.ManagementConfig{Revision: 11},
	}))
	if level.Level() != slog.LevelInfo {
		t.Errorf("cleared log_level: level = %s, want INFO", level.Level())
	}
}

func TestDrainResumeAndStatus(t *testing.T) {
	h, handle := newServer(t, "http://core:8002")
	invoke := func(action string) protocol.ReplyEnvelope {
		return handle(envelope(t, protocol.TypeMgmtActionInvoke, protocol.ActionInvokeRequest{
			InvocationID: protocol.NewID(), Action: action,
		}))
	}
	// Nothing to resume, and no WebSocket to close.
	refused(t, invoke("resume"), "Not draining")
	refused(t, invoke("close_connections"), "No WebSocket connection is open")
	if rep := invoke("drain"); rep.Status != protocol.StatusOK || !h.Draining() {
		t.Fatalf("drain: %+v draining=%v", rep.Error, h.Draining())
	}
	refused(t, invoke("drain"), "Already draining")

	ts := httptest.NewServer(h)
	defer ts.Close()
	resp, err := http.Post(ts.URL, "application/octet-stream", bytes.NewReader(make([]byte, 17)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("draining status = %d, want 404", resp.StatusCode)
	}

	var st protocol.StateGetReply
	_ = json.Unmarshal(handle(envelope(t, protocol.TypeMgmtStateGet, nil)).Payload, &st)
	if st.Status["draining"] != true || st.Status["failed_total"] != float64(1) || st.Status["requests_total"] != float64(1) {
		t.Errorf("status = %+v", st.Status)
	}
	if _, ok := st.Status["last_core_error_at"]; ok {
		t.Error("last_core_error_at reported without a core error")
	}

	if rep := invoke("resume"); rep.Status != protocol.StatusOK || h.Draining() {
		t.Fatalf("resume: %+v draining=%v", rep.Error, h.Draining())
	}
}

// refused checks that an action was unavailable for reason.
func refused(t *testing.T, rep protocol.ReplyEnvelope, reason string) {
	t.Helper()
	if rep.Error == nil || rep.Error.Code != protocol.CodeActionUnavailable || rep.Error.Message != reason {
		t.Fatalf("reply %+v, want %s %q", rep.Error, protocol.CodeActionUnavailable, reason)
	}
}

func TestCheckCore(t *testing.T) {
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/channel/sync" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "invalid envelope", http.StatusBadRequest)
	}))
	defer core.Close()

	_, handle := newServer(t, core.URL)
	rep := handle(envelope(t, protocol.TypeMgmtActionInvoke, protocol.ActionInvokeRequest{InvocationID: "c1", Action: "check_core"}))
	var out struct {
		Result CoreCheck `json:"result"`
	}
	if err := json.Unmarshal(rep.Payload, &out); err != nil || rep.Status != protocol.StatusOK {
		t.Fatalf("reply = %+v (%v)", rep, err)
	}
	if !out.Result.Reachable || out.Result.Status != http.StatusBadRequest {
		t.Errorf("check = %+v, want reachable with 400", out.Result)
	}

	down := httptest.NewServer(http.NotFoundHandler())
	url := down.URL
	down.Close()
	if c := CheckCore(context.Background(), http.DefaultClient, url+"/api/channel/sync"); c.Reachable || c.Error == "" {
		t.Errorf("down check = %+v, want unreachable", c)
	}
}
