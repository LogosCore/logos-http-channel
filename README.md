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

## Health check

`GET /healthz` is served on a separate internal port (`HEALTH_LISTEN_ADDR`,
default `:8081`). Don't publish that port; reach it only from the Docker network.

## Configuration

Environment variables:

- `CHANNEL_ID` (default: `http-main`)
- `LISTEN_ADDR` (default: `:8080`)
- `LOGOS_SYNC_BASE_URL` (default: `http://localhost:9000`)
- `HEALTH_LISTEN_ADDR` (default: `:8081`) — internal health server
- `MAX_BODY_BYTES` (default: `1048576`) — max client request body, minimum 17

`.env` fallback: pass path with `--config <path>`. Existing env vars take precedence.

## Run

```bash
go run ./cmd/http-channel --config .env
```

## Tests

```bash
go test ./...
```

Integration tests use a built-in core simulator that stubs `/api/channel/sync`.
