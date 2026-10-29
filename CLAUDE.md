# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What This Is

`logos-http-channel` is a Go module that implements HTTP transport for Logos minion communication. It receives HTTP requests (any method, any path) containing a binary payload, extracts `id` (raw UUIDv4) and `encrypted_data`, and forwards them to the Logos core sync endpoint: the ciphertext as the raw request body, the ids in `Logos-*` headers. Core's reply is packed back into the same binary format.

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
- `id` — raw UUIDv4 bytes (16 bytes), formatted as a lowercase UUID string for core (`Logos-Minion-Id`)
- `encrypted_data` — `enc_part1 + enc_part2` concatenated, sent to core as it is (the request body)
- Response uses a random offset so the UUID position varies per response

**Entrypoint:** `cmd/http-channel/main.go` — embeds `icon.svg` and `icon-light.svg` (the channel's icon in core's UI and the same drawing with heavier lines for light themes, sent at registration as `Icon` and `IconLight`; `TestIconIsValid` checks both against `protocol.ValidateIcon`), loads config, builds the handler and its management server, starts the client server, the internal health server and the control-plane session (`channel-core/v3/pkg/channel.Run`, which declares the minion connection schema from `internal/connection`). On SIGINT/SIGTERM it drains the servers first, then deregisters.

**Internal packages:**

- `internal/config/` — environment-based config (`CHANNEL_ID`, `LISTEN_ADDR`, `LOGOS_SYNC_BASE_URL`, `CORE_SYNC_TIMEOUT`, `HEALTH_LISTEN_ADDR`, `MAX_BODY_BYTES`, `LOG_FORMAT`, `TRUSTED_PROXIES`, `RABBITMQ_*`)
- `internal/transport/http/httpserver/` — `Handler` is the client handler: bare (no ServeMux, so no redirects), treating every request as a message. It reads `RuntimeConfig` (max body) from an atomic pointer per request, so `SetConfig` applies live; `CoreConfig` (sync URL, timeout) is fixed from env; keeps atomic counters (`Stats`) and a drain flag. `New` wraps it in the client server; `NewHealth` serves `GET /healthz` on a separate internal port. Contains binary pack/unpack logic and inline sync client.
- `internal/connection/` — the minion connection schema declared at registration (`module.Config.Connection`): what minions are given to reach the channel. No transport/method/path parameters — the channel accepts all of them.
- `internal/management/` — the management manifest (properties, `drain`/`resume`/`close_connections`/`check_core` actions, counter status) built from env config, bound to the `Handler` via `logos-golang-module-core/pkg/mgmt`. An action that would change nothing declares it in `available_when` (drain while draining, resume while not, closing WebSockets with none open). Settings for reaching core stay env-only; don't make them manageable.

Control-plane plumbing (reconnect, register/heartbeat/deregister, serving management on the `rpc_queue`) lives in module-core `pkg/module` (wrapped by channel-core `pkg/channel`); this repo only declares what is managed. Keep it that way: no lifecycle code here, no channel-specific behavior pushed into channel-core, and nothing channel-specific in module-core. Core rejects sync (403 → client 404) from unregistered instances.

**Request flow:**

1. Accept any method on any URL path (body capped at `MAX_BODY_BYTES`)
2. Parse binary body: extract offset, raw UUID, encrypted data parts
3. Build an `InboundMinionMessage` (formatted UUID string, the ciphertext as bytes)
4. POST it to `{LOGOS_SYNC_BASE_URL}/api/channel/sync`: the ciphertext as an `application/octet-stream` body, `Logos-Message-Id` / `Logos-Minion-Id` / `Logos-Channel-Instance` headers
5. Read core's reply with channel-core's `syncclient.ReadReply`: 200 with ciphertext, or 204 with nothing for the minion (403 counts as rejected by core)
6. Pack response: random offset, split the reply's data, embed raw UUID
7. Return raw bytes (`application/octet-stream`)

**Logging:** `log/slog` only, built by module-core `logging.New` (`LOG_FORMAT` json/text on stderr, level from the `log_level` property via a `slog.LevelVar`, teed into `logstream.Hub` for core's live tail). Each request logs exactly one line when it ends (`httpserver/requestlog.go`): served at debug, client mistakes at info, core trouble at warn; all request headers unredacted, never the body or the minion payload fields.

**WebSocket** (`httpserver/websocket.go`): a valid handshake on any path opens a connection whose binary frames are messages in the same packed format; each is relayed through the same `relay` path as a request. Messages of one minion UUID are serialized (a per-UUID queue), different UUIDs run concurrently up to `ws_max_inflight`. The handshake is validated before `websocket.Accept` so a bad one still gets the plain 404; any failure on an open connection closes it with no reason. Connections are hijacked, so `ShutdownWebSockets` (not `http.Server.Shutdown`) closes them; drain closes them too.

**Every failure returns a plain `404 page not found`** (`http.NotFound`); the reason is only logged. Never return other error statuses to clients.

**External dependencies:** `logos-golang-protocol` — the sync call's types and headers (`InboundMinionMessage`, `OutboundMinionMessage`); `logos-golang-module-core` — control-plane session (`pkg/module`), management server (`pkg/mgmt`), logging and live log tail; `logos-golang-channel-core/v3` — `pkg/channel.Run` (module session as a channel, with the connection schema) and `pkg/syncclient.ReadReply` (core's reply).

## Testing

Tests are integration-style with a built-in core simulator (`test_logos_core_test.go`) that stubs `/api/channel/sync`. The mock reads the raw-body request (`readSync`), prepends `resp:` to the data and answers with it (`writeSync`).
