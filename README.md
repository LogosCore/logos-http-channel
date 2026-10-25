# logos-http-channel

HTTP channel module for Logos.

## Purpose

Receives HTTP requests (any method, any path) with a binary payload, extracts `id` (raw UUIDv4) and `encrypted_data`, builds a canonical envelope, and forwards it to the Logos core sync endpoint. Disassembles the outbound envelope back into the same binary format for the response.

Uses `github.com/logoscore/logos-golang-protocol` for canonical envelope types.

## Binary Wire Format

Request and response share the same structure:

```
[offset:1][enc_part1:offset bytes][id:16 bytes raw UUIDv4][enc_part2:remaining]
```

- `offset` — byte position where the 16-byte UUID sits within the encrypted payload (0–255)
- `id` — raw UUIDv4 (16 bytes)
- `encrypted_data` — `enc_part1 + enc_part2` concatenated
- Response offset is randomized per response

## Behavior

- Every request on the client port, any method and any path (including
  `/healthz`), is treated as a minion message.
- On success: `200` with the packed binary reply (`application/octet-stream`).
- On any failure (malformed or oversized body, core unreachable, core non-200,
  invalid core reply, draining): a plain `404 page not found`. The reason is only logged.

## Registration with core

Core only accepts sync traffic from registered module instances. On startup the
channel registers over RabbitMQ (`module.register` on `logos.core.rpc`) as
`module_type=channel`, `module_name=http`, `instance=CHANNEL_ID`, then sends
heartbeats on the interval core returns. It reconnects with backoff if RabbitMQ
or core is unavailable and deregisters on shutdown. Until it is registered,
core rejects messages and clients get 404s (the channel logs `core sync returned 403`).

Run each instance with a distinct `CHANNEL_ID`.

## Management

Core can manage the channel from its admin UI. The channel declares a
management manifest at `module.register` and serves core's management RPC on
its `rpc_queue` (`logos.channel.rpc.<CHANNEL_ID>`; exclusive and auto-delete,
consumed before registering, so a second instance with the same `CHANNEL_ID`
fails to start its session).

Properties (defaults are the env values the process started with):

| Key | Type | Applies | Notes |
| --- | --- | --- | --- |
| `max_body_bytes` | bytes | live | 17 to 64 MiB (or up to the env value, if that is larger); the core reply limit scales with it |
| `log_level` | `debug` / `info` / `warn` / `error` | live | stderr level, default `info`: `debug` adds every request outcome, `info` client errors, startup and management changes, `warn` core problems only |

How the channel reaches core (`LOGOS_SYNC_BASE_URL`, `CORE_SYNC_TIMEOUT`) is
env-only and not manageable from core.

Actions: `drain` (every request gets a 404 until `resume`; shown in the `draining` status value, heartbeat health stays `healthy`),
`resume`, and `check_core` (POSTs `{}` to the core sync URL and reports
`reachable`, `status` and `latency_ms`; core's 400 counts as reachable).

Status values, also sent in heartbeat metrics: `requests_total`,
`succeeded_total`, `failed_total` (every 404), `core_errors_total` (core
unreachable, non-200 or invalid reply), `rejected_by_core_total` (core 403:
not registered), `last_core_error`, `last_core_error_at`, `draining`.

## Request log

Every request is logged once, when it ends:

- served: `debug` `request served`
- client mistake (unreadable or malformed body): `info` `request failed`
- client left while core was answering: `info` `request failed`
- core trouble (unreachable, non-200, bad reply, 403): `warn` `request failed`
- refused while drained: `debug` `request refused`

Fields: `request_id` (also sent to core as `X-Request-Id`), `remote_addr`,
`client_ip`, `proto`, `host`, `method`, `path`, `query`, `content_length`,
`body_bytes`, `headers.*` (every request header, lowercased, as received —
nothing is redacted), `core_status` and `core_ms` (when core was called),
`status`, `response_bytes`, `duration_ms`, `outcome` (`ok`, `draining`,
`bad_request`, `core_error`, `rejected_by_core`, `client_gone`), and `stage` /
`err` on failure. The body is never logged.

Logs (`log/slog`, JSON or text on stderr) can also be tailed live from the module's
Logs tab in core: the channel declares the `logs` capability and streams
records only while someone watches (channel-core `pkg/logstream`). A viewer
may ask for debug records even while `log_level` is `info`; that does not
change stderr output.

Live values are not persisted by the channel: on every (re-)register core sends
the effective config and the channel applies it.

## Health check

`GET /healthz` is served on a separate internal port (`HEALTH_LISTEN_ADDR`,
default `:8081`). Don't publish that port; reach it only from the Docker network.

## Configuration

Environment variables:

- `CHANNEL_ID` (default: `http-main`)
- `LISTEN_ADDR` (default: `:8080`)
- `LOGOS_SYNC_BASE_URL` (default: `http://localhost:8002`)
- `CORE_SYNC_TIMEOUT` (default: `30s`) — how long a client request waits for core's sync reply
- `LOG_FORMAT` (default: `json`) — stderr log format, `json` or `text` (logfmt)
- `TRUSTED_PROXIES` (default: empty) — comma-separated IPs/CIDRs whose `X-Forwarded-For` / `X-Real-IP` is believed for `client_ip`
- `HEALTH_LISTEN_ADDR` (default: `:8081`) — internal health server
- `MAX_BODY_BYTES` (default: `1048576`) — max client request body, 17 to 1 GiB
- `RABBITMQ_HOST` / `RABBITMQ_PORT` / `RABBITMQ_USER` / `RABBITMQ_PASSWORD` / `RABBITMQ_VHOST`
  (defaults: `localhost` / `5672` / `guest` / `guest` / `/`) — core's control-plane broker

`.env` fallback: pass path with `--config <path>`. Existing env vars take precedence.

## Run with logos-core's dev stack

Start logos-core (`make services`), put `RABBITMQ_PASSWORD` (core's
`RABBITMQ_DEFAULT_PASS`) in `.env`, then `docker compose up -d --build`. The
channel runs as its own stack on host port 8090 (`HTTP_CHANNEL_PORT`) and joins
the `logos-core_logos` network.

Or with plain `docker run`:

```bash
docker run --network logos-core_logos -p 8080:8080 \
  -e CHANNEL_ID=http-1 \
  -e LOGOS_SYNC_BASE_URL=http://logos-core-dev:8002 \
  -e RABBITMQ_HOST=logos-rabbitmq -e RABBITMQ_USER=logos -e RABBITMQ_PASSWORD=... \
  ghcr.io/logoscore/logos-http-channel:latest
```

## Run

```bash
go run ./cmd/http-channel --config .env
```

## Tests

```bash
go test ./...
```

Integration tests use a built-in core simulator that stubs `/api/channel/sync`.
