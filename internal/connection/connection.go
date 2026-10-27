// Package connection declares the minion connection schema: the parameters a
// minion is given to reach an HTTP channel. Core keeps it per channel kind;
// operators save values for it as channel endpoints (presets core uses to
// prefill a minion's connection settings). The channel never sees them.
//
// Every instance declares the same schema, so nothing in it may depend on one
// instance's settings.
//
// The channel accepts any method on any path, and plain requests and
// WebSockets alike, so none of those are parameters: a minion picks its own.
package connection

import (
	protocol "github.com/logoscore/logos-golang-protocol/protocol"
)

// Version is the parameter set's version. Raise it when minions built against
// the current set would read a new one wrongly (a field removed, renamed or
// given another meaning); a new optional field does not need it.
//
// 2: headers became a string_map (name → value) instead of "Name: value" lines.
const Version = 2

// baseURLPattern allows a scheme, a host and an optional port: the path is
// the minion's choice.
const baseURLPattern = `^https?://[^/?#\s]+$`

// maxExtraBaseURLs bounds the fallback addresses an endpoint lists.
const maxExtraBaseURLs = 16

// Header names are RFC 9110 tokens; values must not break the header line.
const (
	headerNamePattern  = `^[!#$%&'*+.^_|~0-9A-Za-z-]+$`
	headerValuePattern = `^[^\r\n]*$`
)

// Schema returns the connection schema.
func Schema() protocol.ConnectionSchema {
	return protocol.ConnectionSchema{
		SchemaVersion: protocol.ConnectionSchemaV1,
		Version:       Version,
		Fields: []protocol.FieldSpec{
			{
				Key: "base_url", Type: protocol.FieldURL, Label: "Base URL", Required: true,
				Description: "Where the minion connects: http(s)://host, with a port when it is not the scheme's default. No path: the channel accepts any path and the minion picks its own.",
				Group:       "Address", Order: 1, Placeholder: "https://edge.example.com",
				Pattern: baseURLPattern, MaxLength: 2048,
			},
			{
				Key: "extra_base_urls", Type: protocol.FieldStringList, Label: "Additional base URLs",
				Description: "More addresses of this channel, written like the base URL (e.g. other fronts in front of it). A minion falls back to them, in order, when the base URL cannot be reached; the other settings apply to each.",
				Group:       "Address", Order: 2, Placeholder: "https://edge-2.example.com",
				Pattern: baseURLPattern, MaxLength: 2048, MaxItems: maxExtraBaseURLs,
			},
			{
				Key: "host_header", Type: protocol.FieldString, Label: "Host header",
				Description: "Host header to send when it must differ from the host in the base URL (e.g. a CDN or reverse proxy in front).",
				Group:       "Address", Order: 3, MaxLength: 255,
			},
			{
				Key: "sni", Type: protocol.FieldString, Label: "TLS server name",
				Description: "Server name to send in the TLS handshake when it must differ from the host in the base URL.",
				Group:       "Address", Order: 4, MaxLength: 255,
			},
			{
				Key: "proxy", Type: protocol.FieldURL, Label: "Proxy",
				Description: "Outbound proxy the minion connects through, e.g. http://proxy.corp:3128.",
				Group:       "Address", Order: 5, MaxLength: 2048,
			},
			{
				Key: "tls_verify", Type: protocol.FieldBool, Label: "Verify TLS certificate",
				Description: "Check the server certificate against the system's trusted roots.",
				Group:       "TLS", Order: 6, Default: false,
			},
			{
				Key: "tls_pin_sha256", Type: protocol.FieldString, Label: "Certificate pin (SHA-256)",
				Description: "SHA-256 of the server certificate, as 64 hex characters. Authenticates the server even when verification is off.",
				Group:       "TLS", Order: 7, Pattern: `^[0-9a-fA-F]{64}$`,
			},
			{
				Key: "user_agent", Type: protocol.FieldString, Label: "User-Agent",
				Description: "User-Agent to send. Empty: the minion's built-in one.",
				Group:       "Requests", Order: 8, MaxLength: 512, Format: protocol.FormatUserAgent,
			},
			{
				Key: "headers", Type: protocol.FieldStringMap, Label: "Extra headers",
				Description: "Headers added to every request and WebSocket handshake, by name.",
				Group:       "Requests", Order: 9,
				KeyPattern: headerNamePattern, Pattern: headerValuePattern, MaxLength: 4096, MaxItems: 32,
			},
			{
				Key: "request_timeout", Type: protocol.FieldDuration, Label: "Request timeout",
				Description: "How long the minion waits for a reply. Keep it above the channel's wait for core (CORE_SYNC_TIMEOUT, 30s by default), or replies still on their way are dropped.",
				Group:       "Timing", Order: 10,
				Default: "40s", Min: "1s", Max: "10m",
			},
			{
				Key: "checkin_interval", Type: protocol.FieldDuration, Label: "Check-in interval",
				Description: "How often the minion checks in when it has nothing to send.",
				Group:       "Timing", Order: 11, Default: "60s", Min: "1s", Max: "168h",
			},
			{
				Key: "checkin_jitter", Type: protocol.FieldInt, Label: "Check-in jitter",
				Description: "Random spread around the check-in interval, in percent of it.",
				Group:       "Timing", Order: 12, Unit: "%", Default: 20, Min: 0, Max: 100,
			},
			{
				Key: "ws_reconnect_backoff", Type: protocol.FieldDuration, Label: "WebSocket reconnect back-off",
				Description: "Longest wait between WebSocket reconnect attempts; the wait starts at 1s and doubles up to this.",
				Group:       "Timing", Order: 13, Default: "60s", Min: "1s", Max: "1h",
			},
		},
	}
}
