# Changelog

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
