package httpserver

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func buildPayload(offset byte, encData []byte, rawUUID []byte) []byte {
	buf := make([]byte, 0, 1+len(encData)+uuidLen)
	buf = append(buf, offset)
	buf = append(buf, encData[:int(offset)]...)
	buf = append(buf, rawUUID...)
	buf = append(buf, encData[int(offset):]...)
	return buf
}

var testUUID = []byte{
	0x55, 0x0e, 0x84, 0x00, 0xe2, 0x9b, 0x41, 0xd4,
	0xa7, 0x16, 0x44, 0x66, 0x55, 0x44, 0x00, 0x00,
}

const testUUIDStr = "550e8400-e29b-41d4-a716-446655440000"

const testMaxBody = 1 << 20

func assertPlain404(t *testing.T, resp *http.Response, what string) {
	t.Helper()
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound || string(body) != "404 page not found\n" {
		t.Fatalf("%s: status = %d, body = %q, want plain 404", what, resp.StatusCode, body)
	}
}

func TestUnpack_Valid(t *testing.T) {
	encData := []byte("hello-encrypted")
	payload := buildPayload(5, encData, testUUID)

	id, decoded, err := unpack(payload)
	if err != nil {
		t.Fatal(err)
	}
	if id != testUUIDStr {
		t.Fatalf("id = %s, want %s", id, testUUIDStr)
	}
	if string(decoded) != "hello-encrypted" {
		t.Fatalf("encrypted_data = %s, want hello-encrypted", string(decoded))
	}
}

func TestUnpack_ZeroOffset(t *testing.T) {
	encData := []byte("payload")
	payload := buildPayload(0, encData, testUUID)

	id, decoded, err := unpack(payload)
	if err != nil {
		t.Fatal(err)
	}
	if id != testUUIDStr {
		t.Fatalf("id = %s, want %s", id, testUUIDStr)
	}
	if string(decoded) != "payload" {
		t.Fatalf("encrypted_data = %s, want payload", string(decoded))
	}
}

func TestUnpack_MaxOffset(t *testing.T) {
	encData := make([]byte, 300)
	for i := range encData {
		encData[i] = byte(i % 256)
	}
	payload := buildPayload(255, encData, testUUID)

	id, decoded, err := unpack(payload)
	if err != nil {
		t.Fatal(err)
	}
	if id != testUUIDStr {
		t.Fatalf("id = %s, want %s", id, testUUIDStr)
	}
	if !bytes.Equal(decoded, encData) {
		t.Fatalf("encrypted_data length = %d, want %d", len(decoded), len(encData))
	}
}

func TestUnpack_NoEncryptedData(t *testing.T) {
	payload := append([]byte{0}, testUUID...)

	id, enc, err := unpack(payload)
	if err != nil {
		t.Fatal(err)
	}
	if id != testUUIDStr {
		t.Fatalf("id = %s, want %s", id, testUUIDStr)
	}
	if len(enc) != 0 {
		t.Fatalf("encrypted_data = %q, want empty", enc)
	}
}

func TestUnpack_TooShort(t *testing.T) {
	_, _, err := unpack([]byte{0x00, 0x01, 0x02})
	if err == nil {
		t.Fatal("expected error for short body")
	}
}

func TestUnpack_InvalidOffset(t *testing.T) {
	payload := make([]byte, 20)
	payload[0] = 200
	_, _, err := unpack(payload)
	if err == nil {
		t.Fatal("expected error for invalid offset")
	}
}

func TestPackUnpack_RoundTrip(t *testing.T) {
	encData := []byte("round-trip-test-data")

	for i := 0; i < 50; i++ {
		packed, err := pack(testUUIDStr, encData)
		if err != nil {
			t.Fatal(err)
		}

		gotID, gotEnc, err := unpack(packed)
		if err != nil {
			t.Fatal(err)
		}
		if gotID != testUUIDStr {
			t.Fatalf("iteration %d: id = %s, want %s", i, gotID, testUUIDStr)
		}
		if string(gotEnc) != string(encData) {
			t.Fatalf("iteration %d: data = %s, want %s", i, string(gotEnc), string(encData))
		}
	}
}

func TestIntegration_PostSync(t *testing.T) {
	core := newTestLogosCore(t)
	defer core.Close()

	srv := New(":0", NewHandler("http-main", CoreConfig{SyncBaseURL: core.URL()}, RuntimeConfig{MaxBodyBytes: testMaxBody}))
	ts := httptest.NewServer(srv.Handler)
	defer ts.Close()

	encData := []byte("secret-payload")
	payload := buildPayload(7, encData, testUUID)

	resp, err := http.Post(ts.URL+"/any/path/works", "application/octet-stream", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}

	in := core.LastInbound()
	if in.BuildID != testUUIDStr || in.ChannelInstance != "http-main" || in.PeerAddress != "127.0.0.1" {
		t.Fatalf("core received %+v, want minion %s from http-main, from 127.0.0.1", in, testUUIDStr)
	}
	if string(in.Data) != "secret-payload" {
		t.Fatalf("core received data = %s, want secret-payload", string(in.Data))
	}

	respBody, _ := io.ReadAll(resp.Body)
	gotID, gotEnc, err := unpack(respBody)
	if err != nil {
		t.Fatal(err)
	}
	if gotID != testUUIDStr {
		t.Fatalf("response id = %s, want %s", gotID, testUUIDStr)
	}
	if string(gotEnc) != "resp:secret-payload" {
		t.Fatalf("response data = %s, want resp:secret-payload", string(gotEnc))
	}
}

func TestAnyMethod_Works(t *testing.T) {
	core := newTestLogosCore(t)
	defer core.Close()

	srv := New(":0", NewHandler("http-main", CoreConfig{SyncBaseURL: core.URL()}, RuntimeConfig{MaxBodyBytes: testMaxBody}))
	ts := httptest.NewServer(srv.Handler)
	defer ts.Close()

	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch, http.MethodOptions} {
		req, _ := http.NewRequest(method, ts.URL+"/test", bytes.NewReader(buildPayload(2, []byte("test"), testUUID)))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", method, resp.StatusCode)
		}
		if id, _, err := unpack(body); err != nil || id != testUUIDStr {
			t.Fatalf("%s: unpack response: id = %s, err = %v", method, id, err)
		}
	}
}

func TestAnyPath_Works(t *testing.T) {
	core := newTestLogosCore(t)
	defer core.Close()

	srv := New(":0", NewHandler("http-main", CoreConfig{SyncBaseURL: core.URL()}, RuntimeConfig{MaxBodyBytes: testMaxBody}))
	ts := httptest.NewServer(srv.Handler)
	defer ts.Close()

	encData := []byte("test")
	paths := []string{"/", "/sync", "/api/v1/data", "/wp-json/posts", "/pixel.gif", "/graphql", "/healthz", "//double//slash", "/a/../b"}
	for _, path := range paths {
		payload := buildPayload(2, encData, testUUID)
		resp, err := http.Post(ts.URL+path, "application/octet-stream", bytes.NewReader(payload))
		if err != nil {
			t.Fatalf("path %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("path %s: status = %d, want 200", path, resp.StatusCode)
		}
	}
}

func TestEmptyBody_NotFound(t *testing.T) {
	core := newTestLogosCore(t)
	defer core.Close()

	srv := New(":0", NewHandler("http-main", CoreConfig{SyncBaseURL: core.URL()}, RuntimeConfig{MaxBodyBytes: testMaxBody}))
	ts := httptest.NewServer(srv.Handler)
	defer ts.Close()

	for _, method := range []string{http.MethodGet, http.MethodPost} {
		req, _ := http.NewRequest(method, ts.URL+"/test", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		assertPlain404(t, resp, method)
	}
}

func TestOversizedBody_NotFound(t *testing.T) {
	core := newTestLogosCore(t)
	defer core.Close()

	const limit = 64
	srv := New(":0", NewHandler("http-main", CoreConfig{SyncBaseURL: core.URL()}, RuntimeConfig{MaxBodyBytes: limit}))
	ts := httptest.NewServer(srv.Handler)
	defer ts.Close()

	atLimit := buildPayload(0, make([]byte, limit-1-uuidLen), testUUID)
	resp, err := http.Post(ts.URL+"/test", "application/octet-stream", bytes.NewReader(atLimit))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("at limit: status = %d, want 200", resp.StatusCode)
	}

	overLimit := buildPayload(0, make([]byte, limit-uuidLen), testUUID)
	resp, err = http.Post(ts.URL+"/test", "application/octet-stream", bytes.NewReader(overLimit))
	if err != nil {
		t.Fatal(err)
	}
	assertPlain404(t, resp, "over limit")
}

func TestCoreFailure_NotFound(t *testing.T) {
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	garbage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"not-a-uuid"}`))
	}))
	down := httptest.NewServer(http.NotFoundHandler())
	downURL := down.URL
	down.Close()
	defer failing.Close()
	defer garbage.Close()

	for name, coreURL := range map[string]string{"core 500": failing.URL, "bad reply": garbage.URL, "core down": downURL} {
		srv := New(":0", NewHandler("http-main", CoreConfig{SyncBaseURL: coreURL}, RuntimeConfig{MaxBodyBytes: testMaxBody}))
		ts := httptest.NewServer(srv.Handler)
		resp, err := http.Post(ts.URL+"/test", "application/octet-stream", bytes.NewReader(buildPayload(2, []byte("test"), testUUID)))
		if err != nil {
			t.Fatal(err)
		}
		assertPlain404(t, resp, name)
		ts.Close()
	}
}

func TestHealthz_InternalServer(t *testing.T) {
	ts := httptest.NewServer(NewHealth(":0").Handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestHealthz_PublicServerTreatsAsMessage(t *testing.T) {
	core := newTestLogosCore(t)
	defer core.Close()

	srv := New(":0", NewHandler("http-main", CoreConfig{SyncBaseURL: core.URL()}, RuntimeConfig{MaxBodyBytes: testMaxBody}))
	ts := httptest.NewServer(srv.Handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	assertPlain404(t, resp, "GET /healthz without body")
}
