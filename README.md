# logos-http-channel

First production channel module for Logos.

## Purpose

`logos-http-channel` implements HTTP transport for minion <-> Server communication using:

- `github.com/logoscore/logos-golang-channel-core` (latest)
- `github.com/logoscore/logos-golang-protocol`

## Scope (v0)

- receive inbound HTTP requests
- resolve profile via channel-core matcher (`hint` -> `fallback`)
- pass canonical values into channel-core profile-aware runtime
- call Logos sync endpoint through channel-core runtime
- return outbound encrypted payload

## Configuration

Runtime config is read from environment variables:

- `CHANNEL_ID` (default: `http-main`)
- `LISTEN_ADDR` (default: `:8080`)
- `LOGOS_SYNC_BASE_URL` (default: `http://localhost:9000`)
- `PROFILES_DIR` (default: `profiles`)

`.env` fallback:

- Pass path with `--config <path-to-env-file>`
- `.env` is loaded as fallback only (existing environment variables win)

Profiles are loaded from YAML files in `PROFILES_DIR` and resolved via channel-core matcher.

Startup behavior:

- create `PROFILES_DIR` if it does not exist
- ensure `default.yaml` exists in that folder
- if missing, copy `examples/profiles/default.yaml` into `PROFILES_DIR/default.yaml`
  - default profile expects raw body payload `base64(id+encrypted_data)`
- watch `PROFILES_DIR` continuously:
  - new profile files are loaded instantly
  - updates to existing profile files are applied instantly
  - deleted profiles are unloaded instantly

Mapping refs support location prefixes:
- `body:<field>`
- `header:<name>`
- `query:<name>`
- `cookie:<name>`

Example: `id: query:minion_id`, `encrypted_data: header:X-Payload`.

## Run

```bash
go run ./cmd/http-channel --config .env
```

## Integration Tests

This module includes integration tests with a built-in `test-c2-core` simulator for `/api/channel/sync`.

Run:

```bash
go test ./...
```

Covered transposition profile scenarios (loaded from `examples/profiles/*.yaml`):

- `body` mapping
- `header` mapping
- `query` mapping
- `cookie` mapping
- hint-routed profile selection
- `base64` transform pipeline (`decode inbound` + `encode outbound`)
- ambiguous profile hint error handling
