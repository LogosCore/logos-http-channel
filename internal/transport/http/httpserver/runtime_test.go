package httpserver

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func post(t *testing.T, url string, body []byte) *http.Response {
	t.Helper()
	resp, err := http.Post(url, "application/octet-stream", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestHandler_BodyCapChangesLive(t *testing.T) {
	core := newTestLogosCore(t)
	defer core.Close()
	h := NewHandler("http-main", CoreConfig{SyncBaseURL: core.URL()}, RuntimeConfig{MaxBodyBytes: testMaxBody})
	ts := httptest.NewServer(h)
	defer ts.Close()

	payload := buildPayload(0, make([]byte, 100), testUUID)
	if resp := post(t, ts.URL, payload); resp.StatusCode != http.StatusOK {
		t.Fatalf("before: status = %d, want 200", resp.StatusCode)
	}

	cfg := h.Config()
	cfg.MaxBodyBytes = 64
	h.SetConfig(cfg)
	assertPlain404(t, post(t, ts.URL, payload), "after lowering cap")

	cfg.MaxBodyBytes = testMaxBody
	h.SetConfig(cfg)
	if resp := post(t, ts.URL, payload); resp.StatusCode != http.StatusOK {
		t.Fatalf("after raising cap: status = %d, want 200", resp.StatusCode)
	}
}

func TestHandler_CoreTimeout(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
		http.Error(w, "late", http.StatusInternalServerError)
	}))
	defer slow.Close()
	if d := NewHandler("http-main", CoreConfig{SyncBaseURL: slow.URL}, RuntimeConfig{}); d.syncTimeout != 30*time.Second || d.Config().LogLevel != LogInfo {
		t.Fatalf("defaults: timeout=%s config=%+v", d.syncTimeout, d.Config())
	}
	h := NewHandler("http-main", CoreConfig{SyncBaseURL: slow.URL, SyncTimeout: 50 * time.Millisecond},
		RuntimeConfig{MaxBodyBytes: testMaxBody, LogLevel: LogDebug})
	ts := httptest.NewServer(h)
	defer ts.Close()

	start := time.Now()
	assertPlain404(t, post(t, ts.URL, buildPayload(0, []byte("x"), testUUID)), "timeout")
	if d := time.Since(start); d > time.Second {
		t.Errorf("request took %v, want the 50ms core timeout", d)
	}
	st := h.Stats()
	if st.CoreErrorsTotal != 1 || st.LastCoreError == "" || st.LastCoreErrorAt.IsZero() {
		t.Errorf("stats = %+v, want one core error recorded", st)
	}
}

func TestHandler_Counters(t *testing.T) {
	core := newTestLogosCore(t)
	defer core.Close()
	forbidden := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not registered", http.StatusForbidden)
	}))
	defer forbidden.Close()

	h := NewHandler("http-main", CoreConfig{SyncBaseURL: core.URL()}, RuntimeConfig{MaxBodyBytes: testMaxBody})
	ts := httptest.NewServer(h)
	defer ts.Close()
	post(t, ts.URL, buildPayload(0, []byte("ok"), testUUID)).Body.Close()
	assertPlain404(t, post(t, ts.URL, []byte{1, 2}), "malformed")

	st := h.Stats()
	if st.RequestsTotal != 2 || st.SucceededTotal != 1 || st.FailedTotal != 1 || st.CoreErrorsTotal != 0 {
		t.Fatalf("stats = %+v", st)
	}

	h403 := NewHandler("http-main", CoreConfig{SyncBaseURL: forbidden.URL}, RuntimeConfig{MaxBodyBytes: testMaxBody})
	ts403 := httptest.NewServer(h403)
	defer ts403.Close()
	assertPlain404(t, post(t, ts403.URL, buildPayload(0, []byte("x"), testUUID)), "403")
	st = h403.Stats()
	if st.RejectedByCore != 1 || st.FailedTotal != 1 || st.CoreErrorsTotal != 0 || st.LastCoreError != "" {
		t.Fatalf("403 stats = %+v, want rejected_by_core only", st)
	}
}

func TestHandler_DrainResume(t *testing.T) {
	core := newTestLogosCore(t)
	defer core.Close()
	h := NewHandler("http-main", CoreConfig{SyncBaseURL: core.URL()}, RuntimeConfig{MaxBodyBytes: testMaxBody})
	ts := httptest.NewServer(h)
	defer ts.Close()
	payload := buildPayload(0, []byte("x"), testUUID)

	h.SetDraining(true)
	assertPlain404(t, post(t, ts.URL, payload), "draining")
	if st := h.Stats(); !st.Draining || st.FailedTotal != 1 {
		t.Fatalf("stats = %+v, want draining with one failure", st)
	}

	h.SetDraining(false)
	if resp := post(t, ts.URL, payload); resp.StatusCode != http.StatusOK {
		t.Fatalf("after resume: status = %d", resp.StatusCode)
	}
}
