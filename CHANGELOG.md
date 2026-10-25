# Changelog

## Unreleased

- **Module management.** The channel declares a management manifest at
  registration and serves core's management RPC on its `rpc_queue`
  (`logos.channel.rpc.<CHANNEL_ID>`, consumed before registering):
  - Live properties `max_body_bytes`, `log_level`. Defaults are the env values.
    How the channel reaches core (sync URL, timeout) is env-only.
  - Actions `drain` (404 for every request; the heartbeat stays `healthy` and
    the `draining` status value shows the pause),
    `resume`, `check_core` (reachability and latency of the core sync URL).
  - Status counters `requests_total`, `succeeded_total`, `failed_total`,
    `core_errors_total`, `rejected_by_core_total`, plus `last_core_error`,
    `last_core_error_at`, `draining`; also sent in heartbeat metrics.
- The control-plane session (connect, serve management, register, heartbeat,
  deregister, reconnect with backoff) now runs on channel-core `pkg/module`;
  `internal/registration` is gone.
- **Live logs.** Logging moved to `log/slog`; core can tail it from the
  module's Logs tab (`logs` capability, channel-core `pkg/logstream`).
- New `CORE_SYNC_TIMEOUT` env (default `30s`): a per-request context timeout
  for core's sync reply, replacing the fixed HTTP client timeout.
- New `log_level=debug` logs every request outcome.

## v3.1.0

- Registers with core over RabbitMQ (`module.register` / heartbeat /
  `module.deregister`) using `logos-golang-channel-core/v2` `pkg/lifecycle`, so
  core's registration gate accepts this channel's sync traffic. Reconnects with
  backoff when RabbitMQ or core is unavailable; deregisters on shutdown after
  draining in-flight requests.
- New config: `RABBITMQ_HOST`, `RABBITMQ_PORT`, `RABBITMQ_USER`,
  `RABBITMQ_PASSWORD`, `RABBITMQ_VHOST`. `CHANNEL_ID` is the registration instance id.
- Default `LOGOS_SYNC_BASE_URL` is now `http://localhost:8002` (core's port).
- Image build version is reported to core at registration.
- Fixed: the `linux/arm64` image contained an amd64 binary.

## v3.0.0

Breaking:

- Any HTTP method is accepted as a minion message, not only POST.
- Every failure returns a plain `404 page not found` (previously 400/405/413/500/502).
- `/healthz` moved to a separate internal server (`HEALTH_LISTEN_ADDR`, default `:8081`).
  On the client port, `/healthz` is handled as a normal message path.

Added:

- `MAX_BODY_BYTES` (default 1 MiB) configures the request body cap.
  The core reply limit scales with it.

## v2.0.0

Breaking: the wire format and runtime model are replaced.

- Replaced profile-driven field extraction with a binary wire format:
  `[offset:1][enc_part1:offset][id:16 raw UUIDv4][enc_part2:rest]`.
  Responses use the same format with a randomized offset.
- Accepts POST on any path and forwards to `{LOGOS_SYNC_BASE_URL}/api/channel/sync`.
- Dropped the `logos-golang-channel-core` dependency and the profiles directory
  (`PROFILES_DIR` is no longer read).
- Switched to `logos-golang-protocol@v0.2.1` minion message types.
- Added `GET /healthz`.
- Request bodies are capped at 1 MiB (413 when exceeded).
- Request failures are logged.
- Graceful shutdown on SIGINT/SIGTERM.

## v0.3.0

- Implemented real profile-driven HTTP field extraction from:
  - `body:<field>`
  - `header:<name>`
  - `query:<name>`
  - `cookie:<name>`
- Added hint detection across transport locations.
- Fixed mapping flow to use resolved profile keys end-to-end.
- Added integration tests for hint/fallback/ambiguous and query+header mapping.
- Upgraded dependency to `logos-golang-channel-core@v0.3.0`.

## v0.2.0

- Added profile-driven runtime integration:
  - matcher resolve + `HandleWithProfile(...)`
- Added initial integration tests and mapping fixes.

## v0.1.0

- Initial HTTP channel scaffold with `/healthz` and `/sync`.
- Canonical `id` + `encrypted_data` flow wired to channel-core.
