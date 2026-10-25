# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What This Is

`logos-http-channel` is a Go module that implements HTTP transport for Logos minion communication. It receives HTTP requests (any method, any path) containing a binary payload, extracts `id` (raw UUIDv4) and `encrypted_data`, wraps them in a canonical envelope, and forwards to the Logos core sync endpoint. The response envelope is disassembled back into the same binary format.

## Commands

```bash
go run ./cmd/http-channel --config .env    # run the server
go test ./...                               # run all tests
go build -o http-channel ./cmd/http-channel # build binary
```

## Architecture

**Binary wire format** (request and response):

```
[offset:1 byte][enc_part1:offset bytes][id:16 bytes raw UUIDv4][enc_part2:remaining bytes]
```

- `offset` — position where the 16-byte UUID is embedded within the encrypted payload (0–255)
- `id` — raw UUIDv4 bytes (16 bytes), formatted as standard UUID string for the envelope
- `encrypted_data` — `enc_part1 + enc_part2` concatenated, base64-encoded for the envelope
- Response uses a random offset so the UUID position varies per response

**Entrypoint:** `cmd/http-channel/main.go` — loads config, builds the handler and its management server, starts the client server, the internal health server and the control-plane session (`channel-core/v2/pkg/module.Run`). On SIGINT/SIGTERM it drains the servers first, then deregisters.

**Internal packages:**

- `internal/config/` — environment-based config (`CHANNEL_ID`, `LISTEN_ADDR`, `LOGOS_SYNC_BASE_URL`, `CORE_SYNC_TIMEOUT`, `HEALTH_LISTEN_ADDR`, `MAX_BODY_BYTES`, `LOG_FORMAT`, `TRUSTED_PROXIES`, `RABBITMQ_*`)
- `internal/transport/http/httpserver/` — `Handler` is the client handler: bare (no ServeMux, so no redirects), treating every request as a message. It reads `RuntimeConfig` (max body) from an atomic pointer per request, so `SetConfig` applies live; `CoreConfig` (sync URL, timeout) is fixed from env; keeps atomic counters (`Stats`) and a drain flag. `New` wraps it in the client server; `NewHealth` serves `GET /healthz` on a separate internal port. Contains binary pack/unpack logic and inline sync client.
- `internal/connection/` — the minion connection schema declared at registration (`module.Config.Connection`): what minions are given to reach the channel. No transport/method/path parameters — the channel accepts all of them.
- `internal/management/` — the management manifest (properties, `drain`/`resume`/`check_core` actions, counter status) built from env config, bound to the `Handler` via `logos-golang-channel-core/v2/pkg/mgmt`. Settings for reaching core stay env-only; don't make them manageable.

Control-plane plumbing (reconnect, register/heartbeat/deregister, serving management on the `rpc_queue`) lives in channel-core `pkg/module`; this repo only declares what is managed. Keep it that way: no lifecycle code here, and no channel-specific behavior pushed into channel-core. Core rejects sync (403 → client 404) from unregistered instances.

**Request flow:**

1. Accept any method on any URL path (body capped at `MAX_BODY_BYTES`)
2. Parse binary body: extract offset, raw UUID, encrypted data parts
3. Build `InboundMinionMessage` envelope (base64 encrypted_data, formatted UUID string)
4. POST envelope as JSON to `{LOGOS_SYNC_BASE_URL}/api/channel/sync`
5. Receive `OutboundMinionMessage` from core
6. Pack response: random offset, split encrypted_data, embed raw UUID
7. Return raw bytes (`application/octet-stream`)

**Logging:** `log/slog` only, built by channel-core `logging.New` (`LOG_FORMAT` json/text on stderr, level from the `log_level` property via a `slog.LevelVar`, teed into `logstream.Hub` for core's live tail). Each request logs exactly one line when it ends (`httpserver/requestlog.go`): served at debug, client mistakes at info, core trouble at warn; all request headers unredacted, never the body or the minion payload fields.

**WebSocket** (`httpserver/websocket.go`): a valid handshake on any path opens a connection whose binary frames are messages in the same packed format; each is relayed through the same `relay` path as a request. Messages of one minion UUID are serialized (a per-UUID queue), different UUIDs run concurrently up to `ws_max_inflight`. The handshake is validated before `websocket.Accept` so a bad one still gets the plain 404; any failure on an open connection closes it with no reason. Connections are hijacked, so `ShutdownWebSockets` (not `http.Server.Shutdown`) closes them; drain closes them too.

**Every failure returns a plain `404 page not found`** (`http.NotFound`); the reason is only logged. Never return other error statuses to clients.

**External dependencies:** `logos-golang-protocol` — canonical envelope types (`InboundMinionMessage`, `OutboundMinionMessage`); `logos-golang-channel-core/v2` — control-plane session (`pkg/module`) and management server (`pkg/mgmt`).

## Testing

Tests are integration-style with a built-in core simulator (`test_logos_core_test.go`) that stubs `/api/channel/sync`. The mock decodes inbound base64 encrypted_data, prepends `resp:`, re-encodes, and echoes back.
