package httpserver

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/logoscore/logos-golang-channel-core/v3/pkg/connclient"
	"github.com/logoscore/logos-golang-module-core/pkg/logging"
	protocol "github.com/logoscore/logos-golang-protocol/protocol"
)

// A minion may open a WebSocket (a valid handshake on any path) and send its
// messages as binary frames, each one packed exactly like a request body.
// The channel opens a connection socket to core for it (docs:
// contracts/channel-core-connections/) and carries frames both ways: the
// minion's to core, and core's to the minion, each packed like a response:
// core's replies, and the messages core starts (tasks pushed as they are
// queued). If core's socket closes (core restarted, or closed it), the
// channel closes the minion's connection too: core can tie a new socket to
// the session only through a message the minion seals, so the minion,
// connecting again, sends one, and core can reach it at once. A minion need
// not check in on a schedule to find out.
//
// Failures stay as opaque as the 404: a refused handshake gets the plain 404,
// and any failure on an open connection closes it with no reason given. Drain,
// shutdown and the close_connections action close connections with "going
// away".

// WebSocketConfig holds the WebSocket limits core can change live; they apply
// to connections opened afterwards (MaxConnections at every handshake).
type WebSocketConfig struct {
	MaxConnections int
	PingInterval   time.Duration // server pings keep proxies open and find dead peers
	// IdleTimeout closes a connection that neither sent a frame nor answered
	// a ping for this long: a minion waiting for tasks answers pings.
	IdleTimeout time.Duration
}

// DefaultWebSocketConfig fills zero WebSocketConfig fields; the management
// manifest declares the same values as defaults.
var DefaultWebSocketConfig = WebSocketConfig{
	MaxConnections: 1000,
	PingInterval:   30 * time.Second,
	IdleTimeout:    5 * time.Minute,
}

func (c WebSocketConfig) withDefaults() WebSocketConfig {
	d := DefaultWebSocketConfig
	if c.MaxConnections <= 0 {
		c.MaxConnections = d.MaxConnections
	}
	if c.PingInterval <= 0 {
		c.PingInterval = d.PingInterval
	}
	if c.IdleTimeout <= 0 {
		c.IdleTimeout = d.IdleTimeout
	}
	return c
}

// wsWriteTimeout bounds writing one reply frame.
const wsWriteTimeout = 10 * time.Second

// Why a connection ended, logged as close_reason.
const (
	closeClientClosed  = "client_closed"
	closeIdleTimeout   = "idle_timeout"
	closePingTimeout   = "ping_timeout"
	closeConnLost      = "connection_lost"
	closeBadFrame      = "bad_frame"
	closeTooLarge      = "message_too_large"
	closeRelayFailed   = "relay_failed"
	closeDraining      = "draining"
	closeShutdown      = "shutdown"
	closeAdmin         = "closed_by_admin"
	closeWriteFailed   = "write_failed"
	closeCoreClosed    = "core_closed"
	closeShutdownGrace = 5 * time.Second // after in-flight messages, for the close handshake
)

var errTextFrame = errors.New("text frame; messages are binary")

// isWebSocketUpgrade reports whether r asks for a WebSocket; whether the
// handshake is valid is checked separately.
func isWebSocketUpgrade(r *http.Request) bool {
	return headerHasToken(r.Header, "Upgrade", "websocket")
}

// validHandshake repeats the checks websocket.Accept makes, so an invalid
// handshake gets the channel's plain 404 instead of the library's 400/405/426.
func validHandshake(r *http.Request) error {
	switch {
	case !r.ProtoAtLeast(1, 1):
		return fmt.Errorf("handshake over %s", r.Proto)
	case r.Method != http.MethodGet:
		return fmt.Errorf("handshake method %s", r.Method)
	case !headerHasToken(r.Header, "Connection", "upgrade"):
		return errors.New("Connection header lacks upgrade")
	case r.Header.Get("Sec-WebSocket-Version") != "13":
		return fmt.Errorf("unsupported WebSocket version %q", r.Header.Get("Sec-WebSocket-Version"))
	}
	keys := r.Header.Values("Sec-WebSocket-Key")
	if len(keys) != 1 {
		return errors.New("need exactly one Sec-WebSocket-Key")
	}
	if k, err := base64.StdEncoding.DecodeString(keys[0]); err != nil || len(k) != 16 {
		return errors.New("invalid Sec-WebSocket-Key")
	}
	return nil
}

func headerHasToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, t := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}

// wsState is the handler's set of open connections.
type wsState struct {
	mu       sync.Mutex
	conns    map[*wsConn]struct{} // fully open connections only
	reserved int                  // handshakes being accepted, counted against the limit
	closing  bool                 // shutting down: refuse new connections
	wg       sync.WaitGroup

	open, total, messages, failed atomic.Int64
}

func (h *Handler) serveWebSocket(rw http.ResponseWriter, r *http.Request) {
	cfg := h.cfg.Load()
	wcfg := cfg.WebSocket.withDefaults()
	trusted := h.trustedProxies()
	c := &wsConn{
		h:       h,
		id:      protocol.NewID(),
		peer:    peerAddress(r, trusted),
		start:   time.Now(),
		cfg:     wcfg,
		maxBody: cfg.MaxBodyBytes,
		level:   slog.LevelDebug,
		outcome: outcomeClosed,
	}
	refuse := func(level slog.Level, outcome, stage string, err error) {
		h.requests.Add(1)
		h.failed.Add(1)
		w := &countingWriter{ResponseWriter: rw}
		http.NotFound(w, r)
		l := &requestLog{id: c.id, start: c.start, level: level, msg: "websocket refused", outcome: outcome, stage: stage, err: err}
		l.log(r, w, trusted)
	}
	if err := validHandshake(r); err != nil {
		refuse(slog.LevelInfo, outcomeBadRequest, "handshake", err)
		return
	}
	if h.draining.Load() {
		refuse(slog.LevelDebug, outcomeDraining, "", nil)
		return
	}

	// Reserve a slot before accepting, so the limit holds under bursts; the
	// connection joins the set only once it is fully set up, so closing it
	// from elsewhere (drain, shutdown) never sees it half-built.
	ws := &h.ws
	ws.mu.Lock()
	switch {
	case ws.closing:
		ws.mu.Unlock()
		refuse(slog.LevelDebug, outcomeDraining, "", nil)
		return
	case len(ws.conns)+ws.reserved >= wcfg.MaxConnections:
		ws.mu.Unlock()
		refuse(slog.LevelWarn, outcomeTooManyConnections, "handshake",
			fmt.Errorf("%d connections open (ws_max_connections)", wcfg.MaxConnections))
		return
	}
	ws.reserved++
	ws.wg.Add(1)
	ws.mu.Unlock()
	defer ws.wg.Done()
	release := func() {
		ws.mu.Lock()
		ws.reserved--
		ws.mu.Unlock()
	}

	// Minions are not browsers: no Origin check (it protects cookies a
	// browser would send). Payloads are encrypted: compression gains nothing.
	conn, err := websocket.Accept(rw, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
		CompressionMode:    websocket.CompressionDisabled,
	})
	if err != nil {
		release()
		// The handshake was valid; Accept failed on hijacking. It answered.
		l := &requestLog{id: c.id, start: c.start, level: slog.LevelInfo, msg: "websocket refused", outcome: outcomeBadRequest, stage: "accept", err: err}
		l.log(r, &countingWriter{ResponseWriter: rw}, trusted)
		return
	}
	h.requests.Add(1)
	h.succeeded.Add(1)
	ws.total.Add(1)
	ws.open.Add(1)
	defer ws.open.Add(-1)

	conn.SetReadLimit(cfg.MaxBodyBytes)
	c.conn = conn
	c.ctx, c.cancel = context.WithCancel(context.Background())
	defer c.cancel()

	ws.mu.Lock()
	ws.reserved--
	ws.conns[c] = struct{}{}
	closing := ws.closing
	ws.mu.Unlock()
	defer func() {
		ws.mu.Lock()
		delete(ws.conns, c)
		ws.mu.Unlock()
	}()
	if closing || h.draining.Load() {
		// Shutdown or drain began while accepting.
		go c.goAway(closeShutdown, closeShutdownGrace)
	}
	c.run()
	c.log(r, trusted)
}

// wsConn is one open connection.
type wsConn struct {
	h       *Handler
	id      string
	peer    string // the client's address, for every message (peerAddress)
	start   time.Time
	cfg     WebSocketConfig
	maxBody int64
	conn    *websocket.Conn
	ctx     context.Context
	cancel  context.CancelFunc

	// core is the connection socket to core, opened at the minion's first
	// frame and again when it dropped.
	coreMu sync.Mutex
	core   *connclient.Conn
	// active is when the minion last sent a frame or answered a ping (unix
	// nanoseconds).
	active atomic.Int64

	going     atomic.Bool // closing gently: frames read meanwhile are dropped
	ending    atomic.Bool // closing: core's socket closes because this one does
	closeOnce sync.Once

	framesIn, framesOut, bytesIn, bytesOut, msgFailed atomic.Int64

	// The connection's outcome for its log line; the first failure wins.
	logMu       sync.Mutex
	level       slog.Level
	outcome     string
	stage       string
	err         error
	closeReason string
}

func (c *wsConn) touch() { c.active.Store(time.Now().UnixNano()) }

// run reads the minion's frames until the connection ends, carrying each to
// core.
func (c *wsConn) run() {
	c.touch()
	go c.pinger()
	go c.watchdog()
	for {
		typ, data, err := c.conn.Read(c.ctx)
		if err != nil {
			c.readEnded(err)
			break
		}
		c.touch()
		c.framesIn.Add(1)
		c.bytesIn.Add(int64(len(data)))
		if c.going.Load() {
			continue // closing: the minion resends after it reconnects
		}
		if typ != websocket.MessageBinary {
			c.fail(slog.LevelInfo, outcomeBadRequest, "frame", errTextFrame, closeBadFrame)
			break
		}
		id, enc, err := unpack(data)
		if err != nil {
			c.fail(slog.LevelInfo, outcomeBadRequest, "unpack", err, closeBadFrame)
			break
		}
		if err := c.toCore(id, enc); err != nil {
			c.h.coreErrors.Add(1)
			c.h.ws.failed.Add(1)
			c.msgFailed.Add(1)
			c.fail(slog.LevelWarn, outcomeCoreError, "core", err, closeRelayFailed)
			break
		}
		c.h.ws.messages.Add(1)
		slog.Debug("websocket message relayed", slog.String("conn_id", c.id), slog.String("direction", "to_core"),
			slog.Int("body_bytes", len(data)))
	}
	c.close(websocket.StatusNormalClosure, "")
}

// readEnded records why reading stopped, unless a close we started explains it.
func (c *wsConn) readEnded(err error) {
	if c.ctx.Err() != nil || c.going.Load() {
		return
	}
	switch {
	case websocket.CloseStatus(err) == websocket.StatusMessageTooBig || strings.Contains(err.Error(), "read limited"):
		c.h.tooLarge.Add(1)
		c.fail(slog.LevelWarn, outcomeTooLarge, "frame", err, closeTooLarge)
	case websocket.CloseStatus(err) != -1:
		c.setReason(closeClientClosed)
	default:
		c.setReason(closeConnLost)
	}
}

// toCore sends a minion's message to core over the connection socket,
// opening one at the minion's first message. A send that fails closes the
// minion's connection (the caller's fail): the minion connects again and
// sends what was not answered.
func (c *wsConn) toCore(id string, enc []byte) error {
	raw, err := parseUUID(id)
	if err != nil {
		return err
	}
	core, err := c.coreConn()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(c.ctx, c.h.syncTimeout)
	defer cancel()
	return core.Send(ctx, protocol.ConnFrame{ID: raw, Data: enc})
}

// coreConn is the open connection socket to core, opened if there is none.
func (c *wsConn) coreConn() (*connclient.Conn, error) {
	c.coreMu.Lock()
	defer c.coreMu.Unlock()
	if c.core != nil {
		select {
		case <-c.core.Done():
		default:
			return c.core, nil
		}
	}
	ctx, cancel := context.WithTimeout(c.ctx, c.h.syncTimeout)
	defer cancel()
	core, err := connclient.Dial(ctx, connclient.Options{BaseURL: c.h.syncBase, Instance: c.h.channelID,
		Kind: protocol.KindReversePersistent, Peer: c.peer, HTTPClient: c.h.client, MaxFrame: c.maxBody})
	if err != nil {
		return nil, err
	}
	c.core = core
	go c.fromCore(core)
	return core, nil
}

// fromCore carries what core sends to the minion until core's socket
// closes, and then closes the minion's connection: the minion connects
// again, and its first message ties a new socket to its session.
func (c *wsConn) fromCore(core *connclient.Conn) {
	defer func() { _ = core.Close() }()
	for {
		f, err := core.Receive(c.ctx)
		if err != nil {
			if c.ctx.Err() == nil && !c.going.Load() && !c.ending.Load() {
				slog.Info("websocket: core's socket closed, closing the minion's connection",
					slog.String("conn_id", c.id), slog.String("err", err.Error()))
				c.setReason(closeCoreClosed)
				c.close(websocket.StatusNormalClosure, "")
			}
			return
		}
		out, err := pack(formatUUID(f.ID), f.Data)
		if err != nil {
			continue
		}
		wctx, cancel := context.WithTimeout(c.ctx, wsWriteTimeout)
		err = c.conn.Write(wctx, websocket.MessageBinary, out)
		cancel()
		if err != nil {
			if c.ctx.Err() == nil {
				c.setReason(closeWriteFailed)
				c.close(websocket.StatusNormalClosure, "")
			}
			return
		}
		c.framesOut.Add(1)
		c.bytesOut.Add(int64(len(out)))
		slog.Debug("websocket message relayed", slog.String("conn_id", c.id), slog.String("direction", "to_minion"),
			slog.Int("response_bytes", len(out)))
	}
}

// pinger pings every PingInterval; a peer that does not answer within the
// interval is gone, and one that answers is active.
func (c *wsConn) pinger() {
	t := time.NewTicker(c.cfg.PingInterval)
	defer t.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(c.ctx, c.cfg.PingInterval)
			err := c.conn.Ping(ctx)
			cancel()
			if err != nil && c.ctx.Err() == nil {
				c.setReason(closePingTimeout)
				c.close(websocket.StatusNormalClosure, "")
				return
			}
			c.touch()
		}
	}
}

// watchdog closes a connection idle for IdleTimeout: no frame and no pong.
func (c *wsConn) watchdog() {
	t := time.NewTicker(max(c.cfg.IdleTimeout/4, 10*time.Millisecond))
	defer t.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-t.C:
			if time.Since(time.Unix(0, c.active.Load())) > c.cfg.IdleTimeout {
				c.setReason(closeIdleTimeout)
				c.close(websocket.StatusNormalClosure, "")
				return
			}
		}
	}
}

// fail records a failure (the first one wins) and closes the connection. No
// reason is sent to the minion: failures stay as opaque as the 404.
func (c *wsConn) fail(level slog.Level, outcome, stage string, err error, reason string) {
	c.logMu.Lock()
	if c.err == nil {
		c.level, c.outcome, c.stage, c.err = level, outcome, stage, err
	}
	if c.closeReason == "" {
		c.closeReason = reason
	}
	c.logMu.Unlock()
	c.close(websocket.StatusNormalClosure, "")
}

func (c *wsConn) setReason(reason string) {
	c.logMu.Lock()
	if c.closeReason == "" {
		c.closeReason = reason
	}
	c.logMu.Unlock()
}

// close closes the connection once, with core's socket, then cancels its
// context, which ends the reader.
func (c *wsConn) close(code websocket.StatusCode, _ string) {
	c.closeOnce.Do(func() {
		c.ending.Store(true)
		go func() {
			_ = c.conn.Close(code, "")
			c.coreMu.Lock()
			if c.core != nil {
				_ = c.core.Close()
			}
			c.coreMu.Unlock()
			c.cancel()
		}()
	})
}

// goAway closes gently: frames read meanwhile are dropped, then "going
// away". Core answers what it was sent over its socket, which closes with
// the connection: the minion sends again what was not answered.
func (c *wsConn) goAway(reason string, _ time.Duration) {
	if !c.going.CompareAndSwap(false, true) {
		return
	}
	c.setReason(reason)
	c.close(websocket.StatusGoingAway, "")
}

// log writes the connection's line when it ends: the handshake request (as a
// request line has it), totals and why it ended.
func (c *wsConn) log(r *http.Request, trusted []netip.Prefix) {
	c.logMu.Lock()
	level, outcome, stage, err, reason := c.level, c.outcome, c.stage, c.err, c.closeReason
	c.logMu.Unlock()
	if reason == "" {
		reason = closeClientClosed
	}
	if !slog.Default().Enabled(r.Context(), level) {
		return
	}
	msg := "websocket closed"
	if err != nil {
		msg = "websocket failed"
	}
	attrs := append([]slog.Attr{slog.String("conn_id", c.id)}, clientAttrs(r, trusted)...)
	attrs = append(attrs,
		slog.Float64("duration_ms", ms(time.Since(c.start))),
		slog.Int64("frames_in", c.framesIn.Load()),
		slog.Int64("frames_out", c.framesOut.Load()),
		slog.Int64("bytes_in", c.bytesIn.Load()),
		slog.Int64("bytes_out", c.bytesOut.Load()),
		slog.Int64("messages_failed", c.msgFailed.Load()),
		slog.String("close_reason", reason),
		slog.String("outcome", outcome),
	)
	if stage != "" {
		attrs = append(attrs, slog.String("stage", stage))
	}
	if err != nil {
		attrs = append(attrs, slog.String("err", err.Error()))
	}
	attrs = append(attrs, logging.Headers(r.Header))
	slog.LogAttrs(context.Background(), level, msg, attrs...)
}

// CloseWebSockets closes every open connection gently (going away) and
// reports how many there were.
func (h *Handler) CloseWebSockets(reason string) int {
	conns := h.wsSnapshot()
	for _, c := range conns {
		go c.goAway(reason, h.syncTimeout+time.Second)
	}
	return len(conns)
}

// ShutdownWebSockets refuses new connections, closes open ones gently and
// waits for them until ctx ends. http.Server.Shutdown does not track
// WebSockets: they are hijacked connections.
func (h *Handler) ShutdownWebSockets(ctx context.Context) error {
	h.ws.mu.Lock()
	h.ws.closing = true
	h.ws.mu.Unlock()
	grace := h.syncTimeout + time.Second
	if dl, ok := ctx.Deadline(); ok {
		grace = max(time.Until(dl)-closeShutdownGrace, time.Second)
	}
	for _, c := range h.wsSnapshot() {
		go c.goAway(closeShutdown, grace)
	}
	done := make(chan struct{})
	go func() {
		h.ws.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		for _, c := range h.wsSnapshot() {
			_ = c.conn.CloseNow()
		}
		return ctx.Err()
	}
}

func (h *Handler) wsSnapshot() []*wsConn {
	h.ws.mu.Lock()
	defer h.ws.mu.Unlock()
	out := make([]*wsConn, 0, len(h.ws.conns))
	for c := range h.ws.conns {
		out = append(out, c)
	}
	return out
}
