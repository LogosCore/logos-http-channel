package httpserver

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	coreProfile "github.com/logoscore/logos-golang-channel-core/pkg/profile"
)

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find module root (go.mod)")
		}
		dir = parent
	}
}

func loadExampleProfiles(t *testing.T, files ...string) []coreProfile.Profile {
	t.Helper()
	all := make([]coreProfile.Profile, 0, len(files))
	for _, file := range files {
		path := filepath.Join(moduleRoot(t), "examples", "profiles", file)
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read profile example %s: %v", file, err)
		}
		profiles, err := coreProfile.ParseYAMLProfiles(b)
		if err != nil {
			t.Fatalf("parse profile example %s: %v", file, err)
		}
		all = append(all, profiles...)
	}
	return all
}

func loadTestdataProfiles(t *testing.T, files ...string) []coreProfile.Profile {
	t.Helper()
	all := make([]coreProfile.Profile, 0, len(files))
	for _, file := range files {
		path := filepath.Join("testdata", file)
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read testdata profile %s: %v", file, err)
		}
		profiles, err := coreProfile.ParseYAMLProfiles(b)
		if err != nil {
			t.Fatalf("parse testdata profile %s: %v", file, err)
		}
		all = append(all, profiles...)
	}
	return all
}

// --- Default profile (kept from before) ---

func TestDefaultProfile_CompositeBase64Body(t *testing.T) {
	logosCore := newTestLogosCore(t)
	defer logosCore.Close()

	profiles := loadExampleProfiles(t, "default.yaml")
	srv := New(":0", "http-main", logosCore.URL(), profiles)
	ts := httptest.NewServer(srv.Handler)
	defer ts.Close()

	const testUUID = "550e8400-e29b-41d4-a716-446655440000" // 36 chars
	inRaw := base64.StdEncoding.EncodeToString([]byte(testUUID + "ciphertext"))
	resp, err := http.Post(ts.URL+"/sync", "text/plain", bytes.NewReader([]byte(inRaw)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("unexpected status: %d body: %s", resp.StatusCode, body)
	}

	in := logosCore.LastInbound()
	if in.ID != testUUID || in.EncryptedData != "ciphertext" {
		t.Fatalf("unexpected inbound to logosCore: %+v", in)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.StdEncoding.DecodeString(string(body))
	if err != nil {
		t.Fatalf("response not base64: %v", err)
	}
	if string(decoded) != "resp:ciphertext" {
		t.Fatalf("unexpected decoded outbound body: %s", string(decoded))
	}
}

// --- Core concept profiles from docs ---

func TestDocProfile_BodyBase64(t *testing.T) {
	logosCore := newTestLogosCore(t)
	defer logosCore.Close()
	profiles := loadExampleProfiles(t, "http-body-b64.yaml")

	srv := New(":0", "http-main", logosCore.URL(), profiles)
	ts := httptest.NewServer(srv.Handler)
	defer ts.Close()

	blobIn := base64.StdEncoding.EncodeToString([]byte("cipher"))
	payload := []byte(`{"sid":"agent-1","data":"` + blobIn + `"}`)
	resp, err := http.Post(ts.URL+"/sync", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("unexpected status: %d body: %s", resp.StatusCode, body)
	}

	in := logosCore.LastInbound()
	if in.ID != "agent-1" || in.EncryptedData != "cipher" {
		t.Fatalf("unexpected inbound to logosCore: %+v", in)
	}

	var out map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	expectedBlob := base64.StdEncoding.EncodeToString([]byte("resp:cipher"))
	if out["data"] != expectedBlob {
		t.Fatalf("unexpected outbound data: %s (want %s)", out["data"], expectedBlob)
	}
}

func TestDocProfile_HeaderBody(t *testing.T) {
	logosCore := newTestLogosCore(t)
	defer logosCore.Close()
	profiles := loadExampleProfiles(t, "http-header-body.yaml")

	srv := New(":0", "http-main", logosCore.URL(), profiles)
	ts := httptest.NewServer(srv.Handler)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/sync", bytes.NewReader([]byte(`{"data":"enc-payload"}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-ID", "h-1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("unexpected status: %d body: %s", resp.StatusCode, body)
	}

	in := logosCore.LastInbound()
	if in.ID != "h-1" || in.EncryptedData != "enc-payload" {
		t.Fatalf("unexpected inbound to logosCore: %+v", in)
	}

	var out map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out["data"] != "resp:enc-payload" {
		t.Fatalf("unexpected outbound data: %s", out["data"])
	}
}

func TestDocProfile_TransformChain(t *testing.T) {
	logosCore := newTestLogosCore(t)
	defer logosCore.Close()
	profiles := loadExampleProfiles(t, "http-chain-demo.yaml")

	srv := New(":0", "http-main", logosCore.URL(), profiles)
	ts := httptest.NewServer(srv.Handler)
	defer ts.Close()

	// Outbound encode order for id: 1) prefix "agent:" → "agent:abc123", 2) base64url encode
	idEncoded := base64.RawURLEncoding.EncodeToString([]byte("agent:abc123"))
	blobEncoded := base64.StdEncoding.EncodeToString([]byte("secret"))

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/sync", bytes.NewReader([]byte(`{"blob":"`+blobEncoded+`"}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Session", idEncoded)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("unexpected status: %d body: %s", resp.StatusCode, body)
	}

	in := logosCore.LastInbound()
	if in.ID != "abc123" || in.EncryptedData != "secret" {
		t.Fatalf("unexpected inbound to logosCore: %+v", in)
	}

	var out map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	expectedBlob := base64.StdEncoding.EncodeToString([]byte("resp:secret"))
	if out["blob"] != expectedBlob {
		t.Fatalf("unexpected outbound blob: %s (want %s)", out["blob"], expectedBlob)
	}
}

func TestDocProfile_CompositeLength(t *testing.T) {
	logosCore := newTestLogosCore(t)
	defer logosCore.Close()
	profiles := loadExampleProfiles(t, "http-composite-length.yaml")

	srv := New(":0", "http-main", logosCore.URL(), profiles)
	ts := httptest.NewServer(srv.Handler)
	defer ts.Close()

	// 16-byte id + encrypted_data
	id := "0123456789abcdef" // exactly 16 bytes
	raw := id + "encrypted-payload"
	encoded := base64.StdEncoding.EncodeToString([]byte(raw))

	resp, err := http.Post(ts.URL+"/sync", "text/plain", bytes.NewReader([]byte(encoded)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("unexpected status: %d body: %s", resp.StatusCode, body)
	}

	in := logosCore.LastInbound()
	if in.ID != id || in.EncryptedData != "encrypted-payload" {
		t.Fatalf("unexpected inbound to logosCore: %+v", in)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.StdEncoding.DecodeString(string(body))
	if err != nil {
		t.Fatalf("response not base64: %v", err)
	}
	if string(decoded) != "resp:encrypted-payload" {
		t.Fatalf("unexpected decoded outbound body: %s", string(decoded))
	}
}

func TestDocProfile_CompositeDelim(t *testing.T) {
	logosCore := newTestLogosCore(t)
	defer logosCore.Close()
	profiles := loadExampleProfiles(t, "http-composite-delim.yaml")

	srv := New(":0", "http-main", logosCore.URL(), profiles)
	ts := httptest.NewServer(srv.Handler)
	defer ts.Close()

	raw := "myid||encrypted-data"
	encoded := base64.StdEncoding.EncodeToString([]byte(raw))

	resp, err := http.Post(ts.URL+"/sync", "text/plain", bytes.NewReader([]byte(encoded)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("unexpected status: %d body: %s", resp.StatusCode, body)
	}

	in := logosCore.LastInbound()
	if in.ID != "myid" || in.EncryptedData != "encrypted-data" {
		t.Fatalf("unexpected inbound to logosCore: %+v", in)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.StdEncoding.DecodeString(string(body))
	if err != nil {
		t.Fatalf("response not base64: %v", err)
	}
	if string(decoded) != "resp:encrypted-data" {
		t.Fatalf("unexpected decoded outbound body: %s", string(decoded))
	}
}

func TestDocProfile_WithNoise(t *testing.T) {
	logosCore := newTestLogosCore(t)
	defer logosCore.Close()
	profiles := loadExampleProfiles(t, "http-with-noise.yaml")

	srv := New(":0", "http-main", logosCore.URL(), profiles)
	ts := httptest.NewServer(srv.Handler)
	defer ts.Close()

	blobIn := base64.StdEncoding.EncodeToString([]byte("cipher"))
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/sync", bytes.NewReader([]byte(`{"data":"`+blobIn+`"}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-ID", "noise-agent")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("unexpected status: %d body: %s", resp.StatusCode, body)
	}

	in := logosCore.LastInbound()
	if in.ID != "noise-agent" || in.EncryptedData != "cipher" {
		t.Fatalf("unexpected inbound to logosCore: %+v", in)
	}

	// Outbound noise: X-Cache-Status should be one of HIT, MISS, EXPIRED
	cacheStatus := resp.Header.Get("X-Cache-Status")
	if cacheStatus != "HIT" && cacheStatus != "MISS" && cacheStatus != "EXPIRED" {
		t.Fatalf("unexpected X-Cache-Status noise header: %q", cacheStatus)
	}
}

// --- HTTP catalog profiles from docs ---

func TestDocProfile_JsonApi(t *testing.T) {
	logosCore := newTestLogosCore(t)
	defer logosCore.Close()
	profiles := loadExampleProfiles(t, "http-json-api.yaml")

	srv := New(":0", "http-main", logosCore.URL(), profiles)
	ts := httptest.NewServer(srv.Handler)
	defer ts.Close()

	blobIn := base64.StdEncoding.EncodeToString([]byte("cipher"))
	payload := []byte(`{"request_id":"r1","payload":"` + blobIn + `"}`)
	resp, err := http.Post(ts.URL+"/sync", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("unexpected status: %d body: %s", resp.StatusCode, body)
	}

	in := logosCore.LastInbound()
	if in.ID != "r1" || in.EncryptedData != "cipher" {
		t.Fatalf("unexpected inbound to logosCore: %+v", in)
	}

	var out map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	expectedBlob := base64.StdEncoding.EncodeToString([]byte("resp:cipher"))
	if out["payload"] != expectedBlob {
		t.Fatalf("unexpected outbound payload: %s (want %s)", out["payload"], expectedBlob)
	}
}

func TestDocProfile_CdnBlend(t *testing.T) {
	logosCore := newTestLogosCore(t)
	defer logosCore.Close()
	profiles := loadExampleProfiles(t, "http-cdn-blend.yaml")

	srv := New(":0", "http-main", logosCore.URL(), profiles)
	ts := httptest.NewServer(srv.Handler)
	defer ts.Close()

	idEncoded := base64.RawURLEncoding.EncodeToString([]byte("cdn-agent"))
	blobIn := base64.StdEncoding.EncodeToString([]byte("cipher"))

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/sync", bytes.NewReader([]byte(`{"data":"`+blobIn+`"}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-ID", idEncoded)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("unexpected status: %d body: %s", resp.StatusCode, body)
	}

	in := logosCore.LastInbound()
	if in.ID != "cdn-agent" || in.EncryptedData != "cipher" {
		t.Fatalf("unexpected inbound to logosCore: %+v", in)
	}

	var out map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	expectedBlob := base64.StdEncoding.EncodeToString([]byte("resp:cipher"))
	if out["data"] != expectedBlob {
		t.Fatalf("unexpected outbound data: %s (want %s)", out["data"], expectedBlob)
	}
}

func TestDocProfile_CookieId(t *testing.T) {
	logosCore := newTestLogosCore(t)
	defer logosCore.Close()
	profiles := loadExampleProfiles(t, "http-cookie-id.yaml")

	srv := New(":0", "http-main", logosCore.URL(), profiles)
	ts := httptest.NewServer(srv.Handler)
	defer ts.Close()

	idEncoded := base64.RawURLEncoding.EncodeToString([]byte("cookie-agent"))
	blobIn := base64.StdEncoding.EncodeToString([]byte("cipher"))

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/sync", bytes.NewReader([]byte(`{"form_data":"`+blobIn+`"}`)))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "session_token", Value: idEncoded})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("unexpected status: %d body: %s", resp.StatusCode, body)
	}

	in := logosCore.LastInbound()
	if in.ID != "cookie-agent" || in.EncryptedData != "cipher" {
		t.Fatalf("unexpected inbound to logosCore: %+v", in)
	}

	var out map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	expectedBlob := base64.StdEncoding.EncodeToString([]byte("resp:cipher"))
	if out["form_data"] != expectedBlob {
		t.Fatalf("unexpected outbound form_data: %s (want %s)", out["form_data"], expectedBlob)
	}
}

func TestDocProfile_GetBeacon(t *testing.T) {
	logosCore := newTestLogosCore(t)
	defer logosCore.Close()
	profiles := loadExampleProfiles(t, "http-get-beacon.yaml")

	srv := New(":0", "http-main", logosCore.URL(), profiles)
	ts := httptest.NewServer(srv.Handler)
	defer ts.Close()

	// Profile 103: id uses base64url, encrypted_data_in uses base64url then url_encode.
	// Outbound transform chain: base64url → url_encode.
	// Inbound reversal: url_decode → base64url_decode.
	// Go's net/http auto-URL-decodes query values, so we need to url_encode
	// the value so that after Go decodes it, the transform's url_decode still
	// sees a url-encoded string.
	idEncoded := base64.RawURLEncoding.EncodeToString([]byte("beacon-agent"))
	blobB64 := base64.RawURLEncoding.EncodeToString([]byte("beacon-data"))
	blobUrlEncoded := url.QueryEscape(blobB64)

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/sync?uid="+idEncoded+"&q="+url.QueryEscape(blobUrlEncoded), bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("unexpected status: %d body: %s", resp.StatusCode, body)
	}

	in := logosCore.LastInbound()
	if in.ID != "beacon-agent" || in.EncryptedData != "beacon-data" {
		t.Fatalf("unexpected inbound to logosCore: %+v", in)
	}
}

func TestDocProfile_MixedPlacement(t *testing.T) {
	logosCore := newTestLogosCore(t)
	defer logosCore.Close()
	profiles := loadExampleProfiles(t, "http-mixed-placement.yaml")

	srv := New(":0", "http-main", logosCore.URL(), profiles)
	ts := httptest.NewServer(srv.Handler)
	defer ts.Close()

	idEncoded := base64.RawURLEncoding.EncodeToString([]byte("mixed-agent"))
	blobIn := base64.StdEncoding.EncodeToString([]byte("cipher"))

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/sync?trace="+idEncoded, bytes.NewReader([]byte(`{"content":"`+blobIn+`"}`)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("unexpected status: %d body: %s", resp.StatusCode, body)
	}

	in := logosCore.LastInbound()
	if in.ID != "mixed-agent" || in.EncryptedData != "cipher" {
		t.Fatalf("unexpected inbound to logosCore: %+v", in)
	}

	var out map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	expectedBlob := base64.StdEncoding.EncodeToString([]byte("resp:cipher"))
	if out["content"] != expectedBlob {
		t.Fatalf("unexpected outbound content: %s (want %s)", out["content"], expectedBlob)
	}
}

func TestDocProfile_CompositePrefixed(t *testing.T) {
	logosCore := newTestLogosCore(t)
	defer logosCore.Close()
	profiles := loadExampleProfiles(t, "http-composite-prefixed.yaml")

	srv := New(":0", "http-main", logosCore.URL(), profiles)
	ts := httptest.NewServer(srv.Handler)
	defer ts.Close()

	// Outbound transform chain: 1) prefix "msg:", 2) base64.
	// So inbound: 1) base64 decode, 2) strip "msg:" prefix, 3) split by length_prefix (16 bytes).
	id := "0123456789abcdef" // 16 bytes
	raw := "msg:" + id + "composite-data"
	encoded := base64.StdEncoding.EncodeToString([]byte(raw))

	resp, err := http.Post(ts.URL+"/sync", "text/plain", bytes.NewReader([]byte(encoded)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("unexpected status: %d body: %s", resp.StatusCode, body)
	}

	in := logosCore.LastInbound()
	if in.ID != id || in.EncryptedData != "composite-data" {
		t.Fatalf("unexpected inbound to logosCore: %+v", in)
	}

	// Outbound: encrypted_data_out has transform prefix "msg:" then base64
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.StdEncoding.DecodeString(string(body))
	if err != nil {
		t.Fatalf("response not base64: %v", err)
	}
	if !strings.HasPrefix(string(decoded), "msg:") {
		t.Fatalf("expected msg: prefix in outbound, got: %s", string(decoded))
	}
	if string(decoded) != "msg:resp:composite-data" {
		t.Fatalf("unexpected decoded outbound body: %s", string(decoded))
	}
}

// --- Ambiguous profile validation (test-only fixtures) ---

func TestAmbiguousProfileSetRejected(t *testing.T) {
	a := loadTestdataProfiles(t, "ambiguous-a.yaml")
	b := loadTestdataProfiles(t, "ambiguous-b.yaml")
	f := loadTestdataProfiles(t, "ambiguous-fallback.yaml")
	combined := append(append(a, b...), f...)
	if err := coreProfile.ValidateSet(combined); err == nil {
		t.Fatal("expected ambiguous profile set to be rejected")
	}
}
