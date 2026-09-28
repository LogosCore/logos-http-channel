# logos-http-channel

HTTP channel module for Logos.

## Purpose

Receives POST requests with a binary payload, extracts `id` (raw UUIDv4) and `encrypted_data`, builds a canonical envelope, and forwards it to the Logos core sync endpoint. Disassembles the outbound envelope back into the same binary format for the response.

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

## Configuration

Environment variables:

- `CHANNEL_ID` (default: `http-main`)
- `LISTEN_ADDR` (default: `:8080`)
- `LOGOS_SYNC_BASE_URL` (default: `http://localhost:9000`)

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
