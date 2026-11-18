# logos-http-channel

HTTP channel module for Logos.

## Purpose

Receives HTTP requests (any method, any path) with a binary payload, extracts the build id (16 raw bytes) and the minion's ciphertext, and forwards them to the Logos core sync endpoint: the ciphertext as the raw request body, the ids in `Logos-*` headers. Packs core's reply back into the same binary format for the response. A minion may also keep a [WebSocket](#websocket) open, which the channel carries over a connection socket to core.

Uses `github.com/logoscore/logos-golang-protocol` for the sync call's types and headers, and `logos-golang-channel-core`'s `syncclient.ReadReply` to read core's reply and `connclient` for the WebSockets' sockets to core. It opens no control link: it has no bind kind, so core never calls it.

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
- **Build id** is the 16 bytes of the id the message is sent under, in the
  order of its canonical text form (the hex digits without dashes,
  decoded): the build's (`build_id`) until the minion has one of its own,
  then its temporary id (after a stage) or its session's (after its
  `hello`). The channel passes whichever it is to core as
  `Logos-Build-Id`, and never knows staging exists.
- A message is at least 17 bytes. A request's ciphertext must not be empty,
  and the whole body is at most `max_body_bytes` (1 MiB by default, live):
  room for the protocol's largest message, 768 KiB of plaintext, sealed and
  packed.

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
  ciphertext as the raw request body, the id as `Logos-Build-Id`, and the
  client's IP address as `Logos-Peer-Address`: the leftmost address in
  `X-Forwarded-For`, else `X-Real-IP`, else the peer (for a WebSocket, the
  handshake's, on every frame). Every proxy in front of the channel is
  believed, whatever its address: behind nginx, a redirector or a CDN,
  have it set `X-Forwarded-For` (or `X-Real-IP`), or every session shows
  the proxy's address. A client that reaches the channel directly can name
  any address in those headers. See the docs' [Channel ↔ Core: HTTP
  Sync](https://logoscore.github.io/logos-docs/contracts/channel-core-sync/).
  Nothing is re-encoded on the way.

### Replies

- **`200`, `application/octet-stream`**, packed like a request, with core's
  ciphertext for the minion and its build id. A minion should check that
  the id is its own.
- **Nothing for the minion** (core's 204) is a `200` too, with an empty
  ciphertext: the offset byte `00` and the id, 17 bytes.
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

It is a `reverse-persistent` connection: core sends over it whenever it has
something, not only in reply (the docs' [Channel ↔ Core:
Connections](https://logoscore.github.io/logos-docs/contracts/channel-core-connections/)).

- Each **binary frame** is one message, packed exactly like a request body.
  At the minion's first frame the channel opens a **connection socket** to
  core (a WebSocket to `/api/channel/connection`, with channel-core's
  `connclient`) and carries frames both ways over it: core's come back to the
  minion packed like a response, the reply to each message (always one) and
  the messages core starts (tasks pushed as they are queued, cancels). Match
  them by what is inside: a reply names the message it answers.
- If core's socket closes (core restarting, or closing it) or a send to core
  fails, the channel closes the minion's connection (`core_closed`,
  `relay_failed`): the minion reconnects, and its first message ties a new
  socket to its session. This is why a minion on a WebSocket need not check
  in on a schedule.
- The channel pings every `ws_ping_interval` (WebSocket control frames, never
  sent to core) and closes a connection that neither sends a message nor
  answers a ping for `ws_idle_timeout`: a minion waiting for tasks stays.
- Failures stay opaque: a refused handshake (invalid, draining, too many
  connections) gets the plain `404`; a failure on an open connection (text
  frame, malformed or oversized frame, core failure) closes it with no reason
  given. Reconnect and resend.
- Drain, shutdown and the `close_connections` action close connections with
  *going away* (1001), and their sockets to core with them; the minion sends
  again what was not answered.

Behind a reverse proxy, enable WebSocket upgrades and keep its idle timeout
above `ws_ping_interval` (nginx: `proxy_read_timeout`, 60s by default).

## Simulated latency

For testing: to reproduce what minions see over a slow link, the channel
can slow every message it carries. With `SIMULATE_LATENCY=true` it declares
two more management properties, `simulated_latency` (default `0s`) and
`simulated_latency_jitter` (default `0` %), which core sets live like any
other. Without it neither exists and nothing is ever delayed.

- **Each way.** A message waits `simulated_latency` on its way to core, and
  whatever goes back to the minion waits it again, so a round trip takes
  twice it longer: `345ms` turns a fast link into one with a 690 ms round
  trip. Through a plain request, the request reaches core a delay late and
  its answer, a 404 too, takes another to come back. Over a WebSocket the
  handshake answers a round trip late, and every frame, the minion's and
  core's (replies and pushes alike), goes on a delay after it came.
- **Jitter** spreads each delay at random by up to that percent of it,
  either way: with `50`, a `345ms` delay is anything from 172 to 517 ms.
- **A WebSocket stays a stream.** Its frames keep their order whatever the
  jitter: one never overtakes the one before it, as a TCP connection's bytes
  do. A burst is not spread out: each frame waits its own delay, so core's
  push sent right after a reply arrives with it. A close, either side's,
  reaches the other after the frames sent before it. Pings are answered at
  once (they are not messages).
- **At `0s`** frames go straight on, as without the setting, unless frames
  are still on their way ahead of them.

While a latency is set, the `simulating_latency` status value is `true`,
highlighted in core's UI, and the channel logs a warning saying so. A
request line says how much of its `duration_ms` was simulated
(`simulated_latency_ms`).

## A minion behind another

A minion that cannot reach the channel can check in through one that can: a
reverse port forward on minion A (`reverse_tunnel`: A listens, and core
connects each connection that comes to the destination, from its own
network), its destination this channel's address as core reaches it, and
minion B built with A's address and the forward's port as its `base_url`.
See the docs' [Tunnels](https://logoscore.github.io/logos-docs/contracts/minion-messages/tunnels/).

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
| `ws_ping_interval` | duration | live (new connections) | default `30s`, 5s to 5m |
| `ws_idle_timeout` | duration | live (new connections) | default `5m`, 10s to 24h |
| `simulated_latency` | duration | live | only with `SIMULATE_LATENCY`: added to every message each way, default `0s`, 0s to 1m ([Simulated latency](#simulated-latency)) |
| `simulated_latency_jitter` | int (%) | live | only with `SIMULATE_LATENCY`: each delay's random spread, either way, default `0`, 0 to 100 |

How the channel reaches core (`LOGOS_CORE_BASE_URL`, `CORE_SYNC_TIMEOUT`) is
env-only and not manageable from core.

Actions: `drain` (high risk: every request and WebSocket handshake gets a 404 until `resume`, open WebSockets are closed; shown in the `draining` status value, heartbeat health stays `healthy`),
`resume`, `close_connections` (high risk: closes every WebSocket after its in-flight messages), and `check_core` (POSTs `{}` to the core sync URL and reports
`reachable`, `status` and `latency_ms`; core's 400 counts as reachable).
An action that would change nothing is not offered: `drain` while draining,
`resume` while not, `close_connections` with no WebSocket open.

On SIGINT/SIGTERM the channel stops taking requests and lets those waiting
on core finish (up to `CORE_SYNC_TIMEOUT` + 5s; the compose file's
`stop_grace_period` is 40s, above it), closes the WebSockets after their
in-flight messages, and deregisters last: core takes no traffic from an
unregistered instance.

Status values, also sent in heartbeat metrics: `requests_total`,
`succeeded_total`, `failed_total` (HTTP requests and WebSocket handshakes;
failed = every 404), `ws_connections_open`, `ws_connections_total`,
`ws_messages_total`, `ws_messages_failed_total`, `core_errors_total` (core
unreachable, non-200 or invalid reply, both transports),
`rejected_by_core_total` (core 403: not registered), `too_large_total`
(request bodies and WebSocket messages over `max_body_bytes`, both
transports), `last_core_error`, `last_core_error_at`, `draining` (highlighted
when `true`), and with `SIMULATE_LATENCY` `simulating_latency` (highlighted
when `true`: a latency is set).

## Minion connection schema

At registration the channel also declares what minions are given to reach it
(`internal/connection`, version 3), for each of its two connection kinds:
`reverse-call` (plain requests) and `reverse-persistent` (a WebSocket). Both
kinds have every field below but `ws_reconnect_backoff`, the WebSocket's
alone; the timing fields read per kind (a check-in over a WebSocket is a
frame, not a request). Every instance declares the same schema. See the
[contract](https://logoscore.github.io/logos-docs/contracts/minion-connection/).

| Key | Num | Type | Default | Notes |
| --- | --- | --- | --- | --- |
| `base_url` | 1 | url | required | `http(s)://host[:port]`, **no path**: the channel accepts any path, the minion picks its own. At most 2048 characters |
| `extra_base_urls` | 2 | string list | | More addresses of the channel in the same form (other fronts), at most 16. A minion falls back to them, in order, when the base URL cannot be reached; the other fields apply to each |
| `host_header` | 3 | string | | The `Host` to send when it must differ from the base URL's host (a CDN, a reverse proxy). At most 255 characters |
| `sni` | 4 | string | | The TLS server name, when it must differ. At most 255 characters |
| `proxy` | 5 | url | | The minion's outbound proxy, e.g. `http://proxy.corp:3128` |
| `tls_verify` | 6 | bool | `false` | Check the certificate against the system's trusted roots |
| `tls_pin_sha256` | 7 | string | | The server certificate's SHA-256, 64 hex characters: authenticates the server with verification off |
| `user_agent` | 8 | string | | Empty: the minion's built-in one. At most 512 characters. A User-Agent field (`format: user_agent`): core's form offers real browsers' and says when a value is outdated or gives itself away |
| `headers` | 9 | string map | | Extra headers on every request and WebSocket handshake, name → value: names are header tokens, values have no line breaks and at most 4096 characters; at most 32 |
| `request_timeout` | 10 | duration | `40s` | 1s to 10m. How long the minion waits for a reply: keep it above `CORE_SYNC_TIMEOUT` (30s by default), or it drops replies still on their way |
| `checkin_interval` | 11 | duration | `60s` | 1s to 168h. `reverse-call`: how often to check in with nothing to send. `reverse-persistent`: how soon core expects the minion back after the WebSocket closed (late, then lost, by it); while it is open no check-in is needed |
| `checkin_jitter` | 12 | int (%) | `20` | 0 to 100: random spread around the interval |
| `ws_reconnect_backoff` | 13 | duration | `60s` | `reverse-persistent` only. 1s to 1h: the longest wait between reconnects, from 1s, doubling |

A minion built from core's [minion
config](https://logoscore.github.io/logos-docs/contracts/minion-build/#the-minion-config)
reads its settings by these numbers, so a number stays its field's
(`TestFieldNumbers` pins them): a new field takes the next one, and a
removed field's is never given again. There is no transport, method or
path parameter: the channel accepts plain requests and WebSockets, any
method, any path, and a minion may use and switch between them; a factory
that lets operators choose them declares its own connection parameters. In
core, each operation keeps ready-to-use values for the schema (channel
endpoints) for creating minions; the channel never sees them.

## Request log

Every request is logged once, when it ends:

- served: `debug` `request served`
- client mistake (unreadable or malformed body): `info` `request failed`
- body over `max_body_bytes`: `warn` `request failed` (`too_large`): a minion
  built for a larger limit keeps failing until it is raised
- core refused the message (a 422 or a 409, below): `info` `request failed`
  (`refused`)
- client left while core was answering: `info` `request failed`
- core trouble (unreachable, another status, a bad reply, a 403): `warn`
  `request failed`
- refused while drained: `debug` `request refused`

Fields: `request_id` (also sent to core as `X-Request-Id`), `remote_addr`,
`client_ip`, `proto`, `host`, `method`, `path`, `query`, `content_length`,
`body_bytes`, `headers.*` (every request header, lowercased, as received —
nothing is redacted), `core_status` and `core_ms` (when core was called),
`status`, `response_bytes`, `duration_ms`, `outcome` (`ok`, `draining`,
`bad_request`, `too_large`, `refused`, `core_error`, `rejected_by_core`,
`client_gone`; a WebSocket's line also `closed`, a handshake refused past
`ws_max_connections` `too_many_connections`), `simulated_latency_ms` (the
part of `duration_ms` a [simulated latency](#simulated-latency) took, when
one is set), and `stage` / `err` on failure. The body is never logged.

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
`message_too_large`, `relay_failed`, `core_closed`, `write_failed`,
`draining`, `shutdown`, `closed_by_admin`). Each relayed frame logs `websocket message relayed` at
debug (`conn_id`, `request_id`, sizes, core status and timing) or
`websocket message failed`. A refused handshake logs `websocket refused` like a
request line.

### Why a minion gets 404

Every failure looks the same to the minion, and to whoever probes the
channel. This log says why:

| `outcome` | `core_status` | Why |
| --- | --- | --- |
| `rejected_by_core` | 403 | The instance is not registered with core, or is blocked. |
| `refused` | 422 | Core knows no build, temporary id or session by the id the message came under, or the frame does not open (another id's keys, the wrong direction, a bad tag or padding), or it holds no message core takes (not the protocol's CBOR, over 768 KiB). Core's log (its `minionsync` logger) says which. |
| `refused` | 409 | A message sent again whose reply core no longer keeps: send the items in a new message. |
| `bad_request` | | The framing (stage `read body` or `unpack`): a body under 17 bytes, an offset past the ciphertext. |
| `too_large` | | A body over `max_body_bytes`. |
| `core_error` | | Core cannot be reached, or answered with another status: the minion keeps its session and tries again. |
| `draining` | | The channel is drained. |

On the dev stack: `docker logs logos-http-channel` and `docker logs
logos-core-dev`.

## Health check

`GET /healthz` is served on a separate internal port (`HEALTH_LISTEN_ADDR`,
default `:8081`). Don't publish that port; reach it only from the Docker network.

## Configuration

Environment variables:

- `CHANNEL_ID` (default: `http-main`)
- `LISTEN_ADDR` (default: `:8080`) — `host:port`, `:port`, or a bare port (all interfaces)
- `LOGOS_CORE_BASE_URL` (default: `http://localhost:8002`) — core's base URL, `http(s)://host[:port]`: the sync call (`/api/channel/sync`) and the WebSockets' sockets (`/api/channel/connection`) go under it
- `LOGOS_CORE_CA_FILE`, `LOGOS_CORE_CERT_SHA256`, `LOGOS_CORE_TLS_INSECURE` (default: none: the system's trust) — how the channel trusts core's certificate over HTTPS, for a self-signed one: a PEM to trust, the certificate's SHA-256 fingerprint, or no verification at all; see [How modules reach core](https://logoscore.github.io/logos-docs/core-infrastructure/#how-modules-reach-core)
- `CORE_SYNC_TIMEOUT` (default: `30s`) — how long a client request waits for core's sync reply
- `LOG_FORMAT` (default: `json`) — stderr log format, `json` or `text` (logfmt)
- `HEALTH_LISTEN_ADDR` (default: `:8081`) — internal health server, same forms as `LISTEN_ADDR`
- `MAX_BODY_BYTES` (default: `1048576`) — max client request body, 17 to 1 GiB
- `RABBITMQ_HOST` / `RABBITMQ_PORT` / `RABBITMQ_USER` / `RABBITMQ_VHOST`
  (defaults: `localhost` / `5672` / `logos` / `/`) — core's control-plane broker
- `RABBITMQ_PASSWORD` (required) — the broker's password, logos-core's `RABBITMQ_DEFAULT_PASS`
- `SIMULATE_LATENCY` (default: `false`) — for testing: declares the `simulated_latency` and `simulated_latency_jitter` properties, with which core can slow every message the channel carries ([Simulated latency](#simulated-latency)); `true` or `false`

`.env` fallback: pass path with `--config <path>`. Existing env vars take precedence.

## Run with logos-core's dev stack

Start logos-core (`make services`), put `RABBITMQ_PASSWORD` (core's
`RABBITMQ_DEFAULT_PASS`) in `.env`, then `docker compose up -d --build`. The
channel runs as its own stack on host port 8090 (`HTTP_CHANNEL_PORT`) and joins
the `logos-core_logos` network. A minion run on the host reaches it at
`http://localhost:8090` (its `base_url`). The compose file sets
`SIMULATE_LATENCY=true`: set `simulated_latency` on the channel's page in
core to reproduce a slow link.

`docker compose` reads `.env` too, and a core URL or broker host set there
wins over the compose file's, which reach core inside the network: for the
stack, keep the password alone in it (`.env.example` sets only that, the
rest commented out).

Or with plain `docker run`:

```bash
docker run --network logos-core_logos -p 8080:8080 \
  -e CHANNEL_ID=http-1 \
  -e LOGOS_CORE_BASE_URL=http://logos-core-dev:8002 \
  -e RABBITMQ_HOST=logos-rabbitmq -e RABBITMQ_USER=logos -e RABBITMQ_PASSWORD=... \
  ghcr.io/logoscore/logos-http-channel:latest
```

### Against an unreleased protocol or SDK

The image builds from tagged modules alone (`.dockerignore` keeps a
workspace out). With a local `go.work` (`use .` and
`../logos-golang-protocol`, checkouts side by side), run a binary built on
the host instead:

```bash
CGO_ENABLED=0 GOOS=linux go build -o .dev-bin/http-channel ./cmd/http-channel
```

mounted over the image's by a `docker-compose.override.yml`:

```yaml
services:
  http-channel:
    volumes:
      - ./.dev-bin/http-channel:/app/http-channel:ro
```

Then `docker compose up -d`, without `--build`; after each change, build it
again and `docker compose restart`. `go.work`, the override and `.dev-bin/`
are gitignored and never committed (the docs' [Changing the
Protocol](https://logoscore.github.io/logos-docs/contributing/changing-the-protocol/)).

## Run

```bash
go run ./cmd/http-channel --config .env
```

On the host, the defaults reach logos-core's dev stack (core at
`localhost:8002`, its RabbitMQ at `localhost:5672` as `logos`): set
`RABBITMQ_PASSWORD`, and a `LISTEN_ADDR` other than `:8080`, which the
stack's nginx has. `.env.example` lists every setting.

## Tests

```bash
go test ./...
```

Integration tests use a built-in core simulator that stubs `/api/channel/sync`
(`internal/transport/http/httpserver/test_logos_core_test.go`). CI
(`.github/workflows/ci.yml`) runs them and golangci-lint v2.13.2 (its default
linters, kept at 0 issues) on pushes to `main`, `v*` tags and pull requests;
a `v*` tag also builds and pushes the image, `linux/amd64` and `linux/arm64`
(`docker-image.yml`).
