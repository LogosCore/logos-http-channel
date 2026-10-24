package management

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

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
	if m.Properties[0].Default != int64(1<<20) || m.Properties[3].Default != "http://core:8002" {
		t.Errorf("defaults not taken from env config: %+v", m.Properties)
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
	cfg := testConfig(syncBaseURL)
	h := httpserver.NewHandler(cfg.ChannelID, cfg.LogosSyncBaseURL, httpserver.RuntimeConfig{MaxBodyBytes: cfg.MaxBodyBytes})
	s, err := New(cfg, h)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h, func(req protocol.Envelope) protocol.ReplyEnvelope { return s.Handle(context.Background(), req) }
}

func envelope(t *testing.T, typ string, payload any) protocol.Envelope {
	t.Helper()
	raw, _ := json.Marshal(payload)
	id := protocol.NewULID()
	return protocol.Envelope{MessageID: id, CorrelationID: id, Type: typ, Version: protocol.EnvelopeVersionV1, Payload: raw}
}

func TestStartupApplyAdoptsSyncURL(t *testing.T) {
	h, handle := newServer(t, "http://core:8002")
	rep := handle(envelope(t, protocol.TypeMgmtConfigApply, protocol.ConfigApplyRequest{
		ManagementConfig: protocol.ManagementConfig{Revision: 1, Values: map[string]any{
			"logos_sync_base_url": "http://elsewhere:9000",
		}},
	}))
	var out protocol.ConfigApplyReply
	if err := json.Unmarshal(rep.Payload, &out); err != nil || len(out.RestartRequired) != 0 {
		t.Fatalf("reply = %+v (%v)", rep, err)
	}
	if h.SyncEndpoint() != "http://elsewhere:9000/api/channel/sync" {
		t.Errorf("startup apply did not adopt the sync URL: %s", h.SyncEndpoint())
	}
}

func TestApplyUpdatesHandlerLive(t *testing.T) {
	h, handle := newServer(t, "http://core:8002")
	// The startup apply (register reply), with nothing managed yet.
	handle(envelope(t, protocol.TypeMgmtConfigApply, protocol.ConfigApplyRequest{}))
	rep := handle(envelope(t, protocol.TypeMgmtConfigApply, protocol.ConfigApplyRequest{
		ManagementConfig: protocol.ManagementConfig{Revision: 1, Values: map[string]any{
			"max_body_bytes":      4096,
			"core_sync_timeout":   "5s",
			"log_level":           "debug",
			"logos_sync_base_url": "http://elsewhere:9000",
		}},
	}))
	var out protocol.ConfigApplyReply
	if err := json.Unmarshal(rep.Payload, &out); err != nil || rep.Status != protocol.StatusOK {
		t.Fatalf("reply = %+v (%v)", rep, err)
	}
	if !slices.Equal(out.RestartRequired, []string{"logos_sync_base_url"}) || len(out.Rejected) != 0 {
		t.Errorf("reply = %+v", out)
	}
	want := httpserver.RuntimeConfig{SyncURL: "http://core:8002/api/channel/sync", MaxBodyBytes: 4096, CoreSyncTimeout: 5 * time.Second, LogLevel: "debug"}
	if got := h.Config(); got != want {
		t.Errorf("config = %+v, want %+v", got, want)
	}
	if h.SyncEndpoint() != "http://core:8002/api/channel/sync" {
		t.Errorf("sync URL changed live: %s", h.SyncEndpoint())
	}

	// Out-of-range values are rejected and the previous value kept.
	rep = handle(envelope(t, protocol.TypeMgmtConfigApply, protocol.ConfigApplyRequest{
		ManagementConfig: protocol.ManagementConfig{Revision: 2, Values: map[string]any{"max_body_bytes": 5, "core_sync_timeout": "5m"}},
	}))
	_ = json.Unmarshal(rep.Payload, &out)
	if len(out.Rejected) != 2 || h.Config().MaxBodyBytes != 4096 || h.Config().CoreSyncTimeout != 5*time.Second {
		t.Errorf("rejected = %+v config = %+v", out.Rejected, h.Config())
	}
}

func TestDrainResumeAndStatus(t *testing.T) {
	h, handle := newServer(t, "http://core:8002")
	invoke := func(action string) protocol.ReplyEnvelope {
		return handle(envelope(t, protocol.TypeMgmtActionInvoke, protocol.ActionInvokeRequest{
			InvocationID: protocol.NewULID(), Action: action,
		}))
	}
	if rep := invoke("drain"); rep.Status != protocol.StatusOK || !h.Draining() {
		t.Fatalf("drain: %+v draining=%v", rep.Error, h.Draining())
	}

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
