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
  invalid core reply): a plain `404 page not found`. The reason is only logged.

## Registration with core

Core only accepts sync traffic from registered module instances. On startup the
channel registers over RabbitMQ (`module.register` on `logos.core.rpc`) as
`module_type=channel`, `module_name=http`, `instance=CHANNEL_ID`, then sends
heartbeats on the interval core returns. It reconnects with backoff if RabbitMQ
or core is unavailable and deregisters on shutdown. Until it is registered,
core rejects messages and clients get 404s (the channel logs `core sync returned 403`).

Run each instance with a distinct `CHANNEL_ID`.

## Health check

`GET /healthz` is served on a separate internal port (`HEALTH_LISTEN_ADDR`,
default `:8081`). Don't publish that port; reach it only from the Docker network.

## Configuration

Environment variables:

- `CHANNEL_ID` (default: `http-main`)
- `LISTEN_ADDR` (default: `:8080`)
- `LOGOS_SYNC_BASE_URL` (default: `http://localhost:8002`)
- `HEALTH_LISTEN_ADDR` (default: `:8081`) — internal health server
- `MAX_BODY_BYTES` (default: `1048576`) — max client request body, minimum 17
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
