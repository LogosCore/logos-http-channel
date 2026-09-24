# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What This Is

`logos-http-channel` is a Go module that implements HTTP transport for Logos implant/session communication. It receives HTTP requests, resolves an obfuscation profile via channel-core's matcher, extracts canonical fields (id + encrypted_data), forwards them to a Logos sync endpoint, and returns the encrypted response.

## Commands

```bash
# Run the server
go run ./cmd/http-channel --config .env

# Run all tests
go test ./...

# Run a single test
go test ./internal/transport/http/httpserver -run TestObfuscationProfiles_Body

# Build binary
go build -o http-channel ./cmd/http-channel
```

## Architecture

**Entrypoint:** `cmd/http-channel/main.go` — loads config, bootstraps profiles dir, starts filesystem watcher, creates HTTP server.

**Two internal packages:**

- `internal/config/` — environment-based config (`Config` struct), profile YAML loading from `PROFILES_DIR`, and a live filesystem watcher (`ProfilesState` + `StartProfilesWatcher`) that hot-reloads profiles on file changes with 150ms debounce.
- `internal/transport/http/httpserver/` — the HTTP server with two endpoints: `POST /sync` (main Logos relay) and `GET /healthz`. Contains all request processing logic in `server.go`.

**Request flow in `/sync`:**

1. Parse request body (JSON or raw)
2. Build `coreResolver.Input` from body, headers, query params, cookies
3. Detect profile hint from well-known locations or custom `profile_id` mapping
4. Resolve profile via `coreMatcher` (hint → specific profile, or fallback to enabled/ordered list)
5. Extract `id` and `encrypted_data` using either `combined_in` (single field with separator) or separate mapped fields
6. Apply inbound transforms (e.g., base64 decode)
7. Forward to Logos via `coreRuntime.HandleWithProfile`
8. Apply outbound transforms and write response

**Key external dependencies (from logos org):**

- `logos-golang-channel-core` — profile model, matcher, resolver, runtime, transform pipeline, sync client
- `logos-golang-protocol` — shared protocol types

**Profiles:** YAML files in `PROFILES_DIR` (default: `profiles/`). Example profiles live in `examples/profiles/`. The server auto-creates `profiles/default.yaml` from the example if missing. Profiles define where to find/place `id` and `encrypted_data` in HTTP requests/responses using location prefixes (`body:`, `header:`, `query:`, `cookie:`).

## Testing

Tests are integration-style with a built-in Logos core simulator (`test_c2_core_test.go`) that stubs `/api/channel/sync`. Tests load profile YAMLs from `examples/profiles/` — these files are part of the test contract. All tests live in `internal/transport/http/httpserver/`.
