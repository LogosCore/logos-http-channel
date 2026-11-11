package connection

import (
	"testing"

	protocol "github.com/logoscore/logos-golang-protocol/protocol"
)

func TestSchemaIsValid(t *testing.T) {
	if err := protocol.ValidateConnectionSchema(Schema()); err != nil {
		t.Fatal(err)
	}
}

func TestSchemaValues(t *testing.T) {
	s := Schema()
	good := map[string]any{
		"base_url":        "https://edge.example.com:8443",
		"extra_base_urls": []any{"https://edge-2.example.com", "http://10.0.0.7:8080"},
		"headers":         map[string]any{"X-Forwarded-Host": "cdn.example.com"},
		"tls_pin_sha256":  "aa11223344556677889900aabbccddeeff00112233445566778899aabbccddee",
	}
	if p := protocol.ValidateConnectionValues(s, protocol.KindReverseCall, good); p != nil {
		t.Fatalf("valid values: %v", p)
	}
	tooMany := make([]any, maxExtraBaseURLs+1)
	for i := range tooMany {
		tooMany[i] = "https://edge.example.com"
	}
	for key, v := range map[string]any{
		"extra_base_urls": []any{"https://edge-2.example.com/prefix"},
		"base_url":        "https://edge.example.com/prefix",
		"headers":         map[string]any{"Bad Name": "x"},
		"tls_pin_sha256":  "abc",
		"request_timeout": "500ms",
	} {
		vals := map[string]any{"base_url": "https://edge.example.com", key: v}
		if p := protocol.ValidateConnectionValues(s, protocol.KindReverseCall, vals); p[key] == "" {
			t.Errorf("%s = %v accepted", key, v)
		}
	}
	vals := map[string]any{"base_url": "https://edge.example.com", "extra_base_urls": tooMany}
	if p := protocol.ValidateConnectionValues(s, protocol.KindReverseCall, vals); p["extra_base_urls"] == "" {
		t.Errorf("%d additional base URLs accepted", len(tooMany))
	}
}

// Each kind spells out its fields: the reconnect back-off is the
// WebSocket's alone.
func TestSchemaKinds(t *testing.T) {
	s := Schema()
	vals := map[string]any{"base_url": "https://edge.example.com", "ws_reconnect_backoff": "30s"}
	if p := protocol.ValidateConnectionValues(s, protocol.KindReversePersistent, vals); p != nil {
		t.Fatalf("reverse-persistent: %v", p)
	}
	if p := protocol.ValidateConnectionValues(s, protocol.KindReverseCall, vals); p["ws_reconnect_backoff"] == "" {
		t.Fatal("reverse-call took the WebSocket's back-off")
	}
}

// A minion that reads a minion config reads its connection by these
// numbers: each stays its field's, the same in every kind.
func TestFieldNumbers(t *testing.T) {
	want := map[string]int{
		"base_url": 1, "extra_base_urls": 2, "host_header": 3, "sni": 4, "proxy": 5, "tls_verify": 6, "tls_pin_sha256": 7,
		"user_agent": 8, "headers": 9, "request_timeout": 10, "checkin_interval": 11, "checkin_jitter": 12,
		"ws_reconnect_backoff": 13,
	}
	s := Schema()
	for kind, k := range s.Kinds {
		if !s.Numbered(kind) {
			t.Errorf("%s: fields without numbers", kind)
		}
		for _, f := range k.Fields {
			if f.Num != want[f.Key] {
				t.Errorf("%s: %s is %d, want %d", kind, f.Key, f.Num, want[f.Key])
			}
		}
	}
}
