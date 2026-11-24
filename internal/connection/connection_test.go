package connection

import (
	"testing"

	protocol "github.com/logoscore/logos-golang-protocol/protocol"
)

func TestSchemaIsValid(t *testing.T) {
	if err := protocol.ValidateConnectionSchema(Schema()); err != nil {
		t.Fatal(err)
	}
	// Each kind's connections told apart by their mode and base URL.
	for kind, k := range Schema().Kinds {
		if len(k.Summary) != 2 || k.Summary[0] != "mode" || k.Summary[1] != "base_url" {
			t.Errorf("%s: summary %v", kind, k.Summary)
		}
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

// Factory-based, the factory decides the addresses: the base URL and the
// additional ones apply only with url-based, the default, in every kind.
func TestModes(t *testing.T) {
	s := Schema()
	for _, kind := range s.SupportedKinds() {
		if p := protocol.ValidateConnectionValues(s, kind, map[string]any{}); p["base_url"] != "value is required" {
			t.Errorf("%s: url-based by default, without a base URL: %v", kind, p)
		}
		factory := map[string]any{"mode": ModeFactoryBased}
		if p := protocol.ValidateConnectionValues(s, kind, factory); p != nil {
			t.Errorf("%s: factory-based, no addresses: %v", kind, p)
		}
		factory["base_url"], factory["extra_base_urls"] = "https://edge.example.com", []any{"https://edge-2.example.com"}
		p := protocol.ValidateConnectionValues(s, kind, factory)
		if p["base_url"] != "only when mode is url-based" || p["extra_base_urls"] != "only when mode is url-based" || len(p) != 2 {
			t.Errorf("%s: factory-based, with addresses: %v", kind, p)
		}
		// What a minion is given: no address of the endpoint's.
		got := protocol.ResolveConnectionValues(s, kind, map[string]any{"mode": ModeFactoryBased})
		if got["mode"] != ModeFactoryBased || got["base_url"] != nil || got["extra_base_urls"] != nil || got["checkin_interval"] != "60s" {
			t.Errorf("%s: factory-based, resolved: %v", kind, got)
		}
		if got := protocol.ResolveConnectionValues(s, kind, map[string]any{"base_url": "https://edge.example.com"}); got["mode"] != ModeURLBased ||
			got["base_url"] != "https://edge.example.com" {
			t.Errorf("%s: url-based, resolved: %v", kind, got)
		}
	}
}

// The mode shows first, before the addresses it decides on.
func TestModeFirst(t *testing.T) {
	for kind, k := range Schema().Kinds {
		first := k.Fields[0]
		for _, f := range k.Fields[1:] {
			if f.Order <= first.Order {
				t.Errorf("%s: %s (order %d) before the mode", kind, f.Key, f.Order)
			}
		}
		if first.Key != "mode" || first.Group != "Address" {
			t.Errorf("%s: %s first, in %q", kind, first.Key, first.Group)
		}
	}
}

// A minion that reads a minion config reads its connection by these
// numbers: each stays its field's, the same in every kind.
func TestFieldNumbers(t *testing.T) {
	want := map[string]int{
		"base_url": 1, "extra_base_urls": 2, "host_header": 3, "sni": 4, "proxy": 5, "tls_verify": 6, "tls_pin_sha256": 7,
		"user_agent": 8, "headers": 9, "request_timeout": 10, "checkin_interval": 11, "checkin_jitter": 12,
		"ws_reconnect_backoff": 13, "mode": 14,
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
