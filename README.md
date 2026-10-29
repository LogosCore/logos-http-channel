# logos-http-channel

HTTP channel module for Logos.

## Purpose

Receives HTTP requests (any method, any path) with a binary payload, extracts `id` (raw UUIDv4) and `encrypted_data`, and forwards them to the Logos core sync endpoint: the ciphertext as the raw request body, the ids in `Logos-*` headers. Packs core's reply back into the same binary format for the response.

Uses `github.com/logoscore/logos-golang-protocol` for the sync call's types and headers, and `logos-golang-channel-core`'s `syncclient.ReadReply` to read core's reply.

## Wire format

This is what a minion speaks to reach core through this channel: plain HTTP
requests, or [WebSocket](#websocket) frames carrying the same messages. The
channel never reads or changes the ciphertext. What is inside it (the message
format and the payload crypto) is between the minion and core, and not this
channel's concern.

### Message

Requests and replies share one layout:

```
offset    ciphertext part 1   build id           ciphertext part 2
[1 byte]  [offset bytes]      [16 bytes, raw]    [the rest]
```

- **Ciphertext** is part 1 followed by part 2. The build id sits inside it,
  `offset` bytes in (0 to 255, and at most the ciphertext's length), so it has
  no fixed position. A minion should pick a random offset for every message;
  the channel does for every reply.
- **Build id** is the 16 bytes of the UUID of the build that made the
  minion (`build_id`), in the order of its canonical text form: the hex digits
  without dashes, decoded.
- A message is at least 17 bytes. A request's ciphertext must not be empty,
  and the whole body is at most `max_body_bytes` (1 MiB by default, live).

Example: build id `5b0c4f6e-8a1d-4c2b-9e7f-3d2a1b0c9e8f`, ciphertext
`c1 c2 c3 c4 c5`, offset 2:

```
02  c1 c2  5b 0c 4f 6e 8a 1d 4c 2b 9e 7f 3d 2a 1b 0c 9e 8f  c3 c4 c5
```

### Requests

- **Any method, any path, any query string**, on the client port, including
  `/healthz`: every request is one minion message. The method, path and
  headers are the minion's choice (its connection settings give it the
  `base_url`, and optionally `host_header`, `sni`, `user_agent` and extra
  `headers`; see [Minion connection schema](#minion-connection-schema)). The
  channel reads only the body.
- The channel unpacks the message and relays it to core's sync endpoint: the
  ciphertext as the raw request body, the id as `Logos-Build-Id` (see the
  docs' [Channel ↔ Core: HTTP Sync](https://logoscore.github.io/logos-docs/contracts/channel-core-sync/)).
  Nothing is re-encoded on the way.

### Replies

- **`200`, `application/octet-stream`**, packed like a request, with core's
  ciphertext for the minion and its build id. A minion should check that
  the id is its own.
- **Nothing for the minion** (core's 204) is a `200` too, with an empty
  ciphertext: the offset byte `00` and the id, 17 bytes. Today every reply is
  this one, since core has no tasking yet.
- **Any failure is a plain `404 page not found`**: a malformed or oversized
  body, the channel draining, core unreachable or refusing the channel (an
  unregistered or blocked instance), or an invalid reply from core. The reason
  is only logged, so to anyone else the channel looks like a dead path. A
  minion treats a 404 as "try again at the next check-in".

### Timing

The channel waits up to `CORE_SYNC_TIMEOUT` (30s by default) for core. A
minion's `request_timeout` (40s by default) should stay above it, or it gives
up on replies still on their way. Between messages the minion waits its
`checkin_interval` with `checkin_jitter`.

## WebSocket

A minion may also keep a WebSocket open and exchange its messages over it:
a valid WebSocket handshake on **any path** is accepted, alongside plain
requests on the same port.

- Each **binary frame** is one message, packed exactly like a request body;
  the reply is one binary frame packed like a response. Each frame is relayed
  to core like a request; core does not tell the two apart.
- Up to `ws_max_inflight` messages per connection are relayed at once. Replies
  carry no id other than the minion UUID, so messages with the **same UUID are
  answered in order**, messages with different UUIDs as soon as core answers:
  match replies by UUID.
- The channel pings every `ws_ping_interval` (WebSocket control frames, never
  sent to core) and closes a connection that sends no message for
  `ws_idle_timeout`.
- Failures stay opaque: a refused handshake (invalid, draining, too many
  connections) gets the plain `404`; a failure on an open connection (text
  frame, malformed or oversized frame, core failure) closes it with no reason
  given. Reconnect and resend.
- Drain, shutdown and the `close_connections` action close connections with
  *going away* (1001) after answering the messages already in flight; frames
  received meanwhile are dropped.

Behind a reverse proxy, enable WebSocket upgrades and keep its idle timeout
above `ws_ping_interval` (nginx: `proxy_read_timeout`, 60s by default).

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
| `ws_max_connections` | int | live | open WebSockets at once, default 1000; further handshakes get a 404 |
| `ws_max_inflight` | int | live (new connections) | messages relayed at once per connection, default 4 |
| `ws_ping_interval` | duration | live (new connections) | default `30s`, 5s to 5m |
| `ws_idle_timeout` | duration | live (new connections) | default `5m`, 10s to 24h |

How the channel reaches core (`LOGOS_SYNC_BASE_URL`, `CORE_SYNC_TIMEOUT`) is
env-only and not manageable from core.

Actions: `drain` (every request and WebSocket handshake gets a 404 until `resume`, open WebSockets are closed; shown in the `draining` status value, heartbeat health stays `healthy`),
`resume`, `close_connections` (closes every WebSocket after its in-flight messages), and `check_core` (POSTs `{}` to the core sync URL and reports
`reachable`, `status` and `latency_ms`; core's 400 counts as reachable).

Status values, also sent in heartbeat metrics: `requests_total`,
`succeeded_total`, `failed_total` (HTTP requests and WebSocket handshakes;
failed = every 404), `ws_connections_open`, `ws_connections_total`,
`ws_messages_total`, `ws_messages_failed_total`, `core_errors_total` (core
unreachable, non-200 or invalid reply, both transports),
`rejected_by_core_total` (core 403: not registered), `too_large_total`
(request bodies and WebSocket messages over `max_body_bytes`, both
transports), `last_core_error`, `last_core_error_at`, `draining` (highlighted
when `true`).

## Minion connection schema

At registration the channel also declares what minions are given to reach it
(`internal/connection`): `base_url` (`http(s)://host[:port]`, no path),
`extra_base_urls` (more addresses in the same form, up to 16, which a minion
falls back to in order), `host_header`, `sni`, `proxy`, `tls_verify` (default `false`),
`tls_pin_sha256`, `user_agent`, `headers` (name → value), `request_timeout` (default `40s`;
keep it above `CORE_SYNC_TIMEOUT`),
`checkin_interval` (`60s`), `checkin_jitter` (`20` %) and
`ws_reconnect_backoff` (`60s`). Every instance declares the same schema. In core,
each operation keeps ready-to-use values for it (channel endpoints) for creating
minions; the channel never sees them. There is no transport, method or
path parameter: the channel accepts all of them. See the
[contract](https://logoscore.github.io/logos-docs/contracts/minion-connection/).

## Request log

Every request is logged once, when it ends:

- served: `debug` `request served`
- client mistake (unreadable or malformed body): `info` `request failed`
- body over `max_body_bytes`: `warn` `request failed` (`too_large`): a minion
  built for a larger limit keeps failing until it is raised
- client left while core was answering: `info` `request failed`
- core trouble (unreachable, non-200, bad reply, 403): `warn` `request failed`
- refused while drained: `debug` `request refused`

Fields: `request_id` (also sent to core as `X-Request-Id`), `remote_addr`,
`client_ip`, `proto`, `host`, `method`, `path`, `query`, `content_length`,
`body_bytes`, `headers.*` (every request header, lowercased, as received —
nothing is redacted), `core_status` and `core_ms` (when core was called),
`status`, `response_bytes`, `duration_ms`, `outcome` (`ok`, `draining`,
`bad_request`, `too_large`, `core_error`, `rejected_by_core`, `client_gone`), and `stage` /
`err` on failure. The body is never logged.

Logs (`log/slog`, JSON or text on stderr) can also be tailed live from the module's
Logs tab in core: the channel declares the `logs` capability and streams
records only while someone watches (module-core `pkg/logstream`). A viewer
may ask for debug records even while `log_level` is `info`; that does not
change stderr output.

Live values are not persisted by the channel: on every (re-)register core sends
the effective config and the channel applies it.

A WebSocket logs one line per connection when it ends (`websocket closed`, or
`websocket failed` at the failure's level): `conn_id`, the handshake request's
fields and headers, `duration_ms`, `frames_in` / `frames_out`, `bytes_in` /
`bytes_out`, `messages_failed` and `close_reason` (`client_closed`,
`idle_timeout`, `ping_timeout`, `connection_lost`, `bad_frame`,
`message_too_large`, `relay_failed`, `write_failed`, `draining`, `shutdown`,
`closed_by_admin`). Each relayed frame logs `websocket message relayed` at
debug (`conn_id`, `request_id`, sizes, core status and timing) or
`websocket message failed`. A refused handshake logs `websocket refused` like a
request line.

## Health check

`GET /healthz` is served on a separate internal port (`HEALTH_LISTEN_ADDR`,
default `:8081`). Don't publish that port; reach it only from the Docker network.

## Configuration

Environment variables:

- `CHANNEL_ID` (default: `http-main`)
- `LISTEN_ADDR` (default: `:8080`) — `host:port`, `:port`, or a bare port (all interfaces)
- `LOGOS_SYNC_BASE_URL` (default: `http://localhost:8002`)
- `CORE_SYNC_TIMEOUT` (default: `30s`) — how long a client request waits for core's sync reply
- `LOG_FORMAT` (default: `json`) — stderr log format, `json` or `text` (logfmt)
- `TRUSTED_PROXIES` (default: empty) — comma-separated IPs/CIDRs whose `X-Forwarded-For` / `X-Real-IP` is believed for `client_ip`
- `HEALTH_LISTEN_ADDR` (default: `:8081`) — internal health server, same forms as `LISTEN_ADDR`
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
