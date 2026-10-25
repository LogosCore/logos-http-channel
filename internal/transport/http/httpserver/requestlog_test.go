package httpserver

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
)

// captureLogs sends the default logger's output, at debug, to a buffer for
// the test and returns a reader for the records written.
func captureLogs(t *testing.T) func() []map[string]any {
	t.Helper()
	var mu sync.Mutex
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(lockedWriter{&mu, &buf}, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		var out []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			if line == "" {
				continue
			}
			var rec map[string]any
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				t.Fatalf("log line %q: %v", line, err)
			}
			out = append(out, rec)
		}
		return out
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l lockedWriter) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(b)
}

func TestRequestLogServed(t *testing.T) {
	records := captureLogs(t)
	var gotRequestID string
	core := newTestLogosCore(t)
	defer core.Close()
	spy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRequestID = r.Header.Get("X-Request-Id")
		core.server.Config.Handler.ServeHTTP(w, r)
	}))
	defer spy.Close()

	ts := httptest.NewServer(NewHandler("http-main", CoreConfig{SyncBaseURL: spy.URL}, RuntimeConfig{MaxBodyBytes: testMaxBody}))
	defer ts.Close()
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/upload/x?a=1&b=2", bytes.NewReader(buildPayload(3, []byte("abcdef"), testUUID)))
	req.Header.Set("User-Agent", "agent/1.0")
	req.Header.Add("X-Custom", "one")
	req.Header.Add("X-Custom", "two")
	req.Header.Set("Authorization", "Bearer s3cret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	recs := records()
	if len(recs) != 1 {
		t.Fatalf("want one line per request, got %v", recs)
	}
	rec := recs[0]
	for k, want := range map[string]any{
		"level": "DEBUG", "msg": "request served", "outcome": "ok",
		"method": "PUT", "path": "/upload/x", "query": "a=1&b=2", "proto": "HTTP/1.1",
		"status": float64(200), "core_status": float64(200),
		"body_bytes":     float64(len(buildPayload(3, []byte("abcdef"), testUUID))),
		"response_bytes": float64(len(respBody)), "client_ip": "127.0.0.1",
	} {
		if rec[k] != want {
			t.Errorf("%s = %v, want %v", k, rec[k], want)
		}
	}
	if rec["request_id"] == "" || rec["request_id"] != gotRequestID {
		t.Errorf("request_id = %v, core got X-Request-Id %q", rec["request_id"], gotRequestID)
	}
	for _, k := range []string{"duration_ms", "core_ms", "remote_addr", "host", "content_length"} {
		if _, ok := rec[k]; !ok {
			t.Errorf("missing %s", k)
		}
	}
	headers, _ := rec["headers"].(map[string]any)
	if headers["user-agent"] != "agent/1.0" || headers["x-custom"] != "one, two" || headers["authorization"] != "Bearer s3cret" {
		t.Errorf("headers = %v", headers)
	}
	if _, ok := rec["stage"]; ok {
		t.Error("stage on a served request")
	}
}

func TestRequestLogFailures(t *testing.T) {
	records := captureLogs(t)
	core := newTestLogosCore(t)
	defer core.Close()
	h := NewHandler("http-main", CoreConfig{SyncBaseURL: core.URL()}, RuntimeConfig{MaxBodyBytes: testMaxBody})
	ts := httptest.NewServer(h)
	defer ts.Close()

	post(t, ts.URL+"/short", []byte("x")).Body.Close()
	post(t, ts.URL+"/big", make([]byte, testMaxBody+1)).Body.Close()
	h.SetDraining(true)
	post(t, ts.URL+"/drained", buildPayload(0, []byte("x"), testUUID)).Body.Close()
	h.SetDraining(false)

	down := httptest.NewServer(http.NotFoundHandler())
	downURL := down.URL
	down.Close()
	hDown := NewHandler("http-main", CoreConfig{SyncBaseURL: downURL}, RuntimeConfig{MaxBodyBytes: testMaxBody})
	tsDown := httptest.NewServer(hDown)
	defer tsDown.Close()
	post(t, tsDown.URL+"/core-down", buildPayload(0, []byte("x"), testUUID)).Body.Close()

	recs := records()
	if len(recs) != 4 {
		t.Fatalf("records = %v", recs)
	}
	want := []struct{ level, msg, outcome, stage string }{
		{"INFO", "request failed", "bad_request", "unpack"},
		{"WARN", "request failed", "too_large", "read body"},
		{"DEBUG", "request refused", "draining", ""},
		{"WARN", "request failed", "core_error", "core sync"},
	}
	for i, w := range want {
		r := recs[i]
		if r["level"] != w.level || r["msg"] != w.msg || r["outcome"] != w.outcome || r["status"] != float64(404) {
			t.Errorf("record %d = %v, want %+v", i, r, w)
		}
		if stage, _ := r["stage"].(string); stage != w.stage {
			t.Errorf("record %d stage = %q, want %q", i, stage, w.stage)
		}
	}
	if recs[3]["core_status"] != float64(0) || recs[3]["err"] == nil {
		t.Errorf("core-down record = %v", recs[3])
	}
	if st := h.Stats(); st.TooLargeTotal != 1 {
		t.Errorf("too large total = %d, want 1", st.TooLargeTotal)
	}
	if _, ok := recs[0]["core_ms"]; ok {
		t.Error("core_ms logged although core was not called")
	}
}

func TestClientIP(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	cases := []struct {
		name, remote, xff, real, want string
	}{
		{"untrusted peer ignores headers", "203.0.113.5:4000", "1.2.3.4", "5.6.7.8", "203.0.113.5"},
		{"trusted peer, rightmost untrusted hop", "10.0.0.2:4000", "1.2.3.4, 198.51.100.9, 10.0.0.7", "", "198.51.100.9"},
		{"trusted peer, real ip", "10.0.0.2:4000", "", "198.51.100.1", "198.51.100.1"},
		{"trusted peer, no headers", "10.0.0.2:4000", "", "", "10.0.0.2"},
		{"ipv6 peer", "[2001:db8::1]:4000", "", "", "2001:db8::1"},
		{"hop with port", "10.0.0.2:4000", "198.51.100.9:5555", "", "198.51.100.9"},
		{"empty hops skipped", "10.0.0.2:4000", "198.51.100.9, ,", "", "198.51.100.9"},
		{"all hops trusted", "10.0.0.2:4000", "10.0.0.5, 10.0.0.7", "", "10.0.0.5"},
		{"real ip ignored when xff present", "10.0.0.2:4000", "198.51.100.9", "6.6.6.6", "198.51.100.9"},
		{"unparsable real ip", "10.0.0.2:4000", "", "nonsense", "10.0.0.2"},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = c.remote
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		if c.real != "" {
			r.Header.Set("X-Real-IP", c.real)
		}
		if got := clientIP(r, trusted); got != c.want {
			t.Errorf("%s: client_ip = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestRequestLogSkippedWhenDisabled(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	core := newTestLogosCore(t)
	defer core.Close()
	ts := httptest.NewServer(NewHandler("http-main", CoreConfig{SyncBaseURL: core.URL()}, RuntimeConfig{MaxBodyBytes: testMaxBody}))
	defer ts.Close()
	post(t, ts.URL, buildPayload(2, []byte("abcd"), testUUID)).Body.Close()
	if buf.Len() != 0 {
		t.Errorf("served request logged at info level: %s", buf.String())
	}
}

func TestUnpackInPlace(t *testing.T) {
	body := buildPayload(3, []byte("abcdef"), testUUID) // "abc" before the UUID, "def" after
	id, enc, err := unpack(body)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := base64.StdEncoding.DecodeString(enc)
	if string(raw) != "abcdef" || id != formatUUID(testUUID) {
		t.Errorf("unpack = %q %q", id, raw)
	}
}
