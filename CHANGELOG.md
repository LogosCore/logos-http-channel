# Changelog

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
