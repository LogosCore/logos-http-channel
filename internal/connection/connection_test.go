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
	if p := protocol.ValidateConnectionValues(s, good); p != nil {
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
		if p := protocol.ValidateConnectionValues(s, vals); p[key] == "" {
			t.Errorf("%s = %v accepted", key, v)
		}
	}
	vals := map[string]any{"base_url": "https://edge.example.com", "extra_base_urls": tooMany}
	if p := protocol.ValidateConnectionValues(s, vals); p["extra_base_urls"] == "" {
		t.Errorf("%d additional base URLs accepted", len(tooMany))
	}
}
