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
	"github.com/logoscore/logos-golang-module-core/pkg/logging"
	protocol "github.com/logoscore/logos-golang-protocol/protocol"
)

// A minion may open a WebSocket (a valid handshake on any path) and send its
// messages as binary frames, each one packed exactly like a request body;
// every frame is relayed to core like a request and answered with one binary
// frame packed like a response.
//
// Up to WebSocketConfig.MaxInflight messages of a connection are relayed at
// once. Replies carry no id other than the minion UUID, so messages with the
// same UUID are relayed in order (first in, first out) and messages with
// different UUIDs concurrently: the minion matches a reply by its UUID.
//
// Failures stay as opaque as the 404: a refused handshake gets the plain 404,
// and any failure on an open connection closes it with no reason given. Drain,
// shutdown and the close_connections action close connections with "going
// away" after their in-flight messages are answered.

// WebSocketConfig holds the WebSocket limits core can change live; they apply
// to connections opened afterwards (MaxConnections at every handshake).
type WebSocketConfig struct {
	MaxConnections int
	MaxInflight    int           // per connection
	PingInterval   time.Duration // server pings keep proxies open and find dead peers
	IdleTimeout    time.Duration // no message from the minion for this long closes it
}

// DefaultWebSocketConfig fills zero WebSocketConfig fields; the management
// manifest declares the same values as defaults.
var DefaultWebSocketConfig = WebSocketConfig{
	MaxConnections: 1000,
	MaxInflight:    4,
	PingInterval:   30 * time.Second,
	IdleTimeout:    5 * time.Minute,
}

func (c WebSocketConfig) withDefaults() WebSocketConfig {
	d := DefaultWebSocketConfig
	if c.MaxConnections <= 0 {
		c.MaxConnections = d.MaxConnections
	}
	if c.MaxInflight <= 0 {
		c.MaxInflight = d.MaxInflight
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
		queues:  map[string]chan wsJob{},
		sem:     make(chan struct{}, wcfg.MaxInflight),
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

	sem      chan struct{} // in-flight slots
	inflight sync.WaitGroup
	// admitMu orders admitting a message (inflight.Add) against goAway
	// starting to wait: once going is set, nothing more enters flight.
	admitMu sync.Mutex
	mu      sync.Mutex
	queues  map[string]chan wsJob // per minion UUID, first in first out

	going     atomic.Bool // closing gently: frames read meanwhile are dropped
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

type wsJob struct {
	messageID string
	id        string
	data      []byte
	size      int
}

// run reads frames until the connection ends, then waits for the messages it
// relayed to finish.
func (c *wsConn) run() {
	go c.pinger()
	for {
		readCtx, cancel := context.WithTimeout(c.ctx, c.cfg.IdleTimeout)
		typ, data, err := c.conn.Read(readCtx)
		idle := readCtx.Err() == context.DeadlineExceeded
		cancel()
		if err != nil {
			c.readEnded(err, idle)
			break
		}
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
		select {
		case c.sem <- struct{}{}:
		case <-c.ctx.Done():
		}
		if c.ctx.Err() != nil {
			break
		}
		c.admitMu.Lock()
		if c.going.Load() {
			c.admitMu.Unlock()
			<-c.sem
			continue // closing: dropped, like frames read meanwhile
		}
		c.inflight.Add(1)
		c.admitMu.Unlock()
		c.enqueue(wsJob{messageID: protocol.NewID(), id: id, data: enc, size: len(data)})
	}
	c.inflight.Wait()
	c.close(websocket.StatusNormalClosure, "")
}

// readEnded records why reading stopped, unless a close we started explains it.
func (c *wsConn) readEnded(err error, idle bool) {
	if c.ctx.Err() != nil || c.going.Load() {
		return
	}
	switch {
	case idle:
		c.setReason(closeIdleTimeout)
	case websocket.CloseStatus(err) == websocket.StatusMessageTooBig || strings.Contains(err.Error(), "read limited"):
		c.h.tooLarge.Add(1)
		c.fail(slog.LevelWarn, outcomeTooLarge, "frame", err, closeTooLarge)
	case websocket.CloseStatus(err) != -1:
		c.setReason(closeClientClosed)
	default:
		c.setReason(closeConnLost)
	}
}

// enqueue hands a message to its UUID's queue, starting a worker for the
// queue if none runs. The queue never blocks: at most MaxInflight messages
// are in flight on the connection.
func (c *wsConn) enqueue(job wsJob) {
	c.mu.Lock()
	defer c.mu.Unlock()
	q, ok := c.queues[job.id]
	if !ok {
		q = make(chan wsJob, cap(c.sem))
		c.queues[job.id] = q
		go c.worker(job.id, q)
	}
	q <- job
}

// worker relays one UUID's messages in order and exits when its queue is empty.
func (c *wsConn) worker(id string, q chan wsJob) {
	for {
		c.mu.Lock()
		select {
		case job := <-q:
			c.mu.Unlock()
			c.process(job)
		default:
			delete(c.queues, id)
			c.mu.Unlock()
			return
		}
	}
}

func (c *wsConn) process(job wsJob) {
	defer func() {
		<-c.sem
		c.inflight.Done()
	}()
	start := time.Now()
	res := c.h.relay(c.ctx, job.messageID, job.id, c.peer, job.data)
	c.h.ws.messages.Add(1)
	ml := wsMessageLog{connID: c.id, messageID: job.messageID, start: start, size: job.size, res: res}
	if res.err != nil {
		c.h.ws.failed.Add(1)
		c.msgFailed.Add(1)
		ml.log()
		if res.outcome != outcomeClientGone {
			c.fail(res.level, res.outcome, res.stage, res.err, closeRelayFailed)
		}
		return
	}
	wctx, cancel := context.WithTimeout(c.ctx, wsWriteTimeout)
	err := c.conn.Write(wctx, websocket.MessageBinary, res.reply)
	cancel()
	if err != nil {
		ml.writeErr = err
		ml.log()
		if c.ctx.Err() == nil {
			c.setReason(closeWriteFailed)
			c.close(websocket.StatusNormalClosure, "")
		}
		return
	}
	c.framesOut.Add(1)
	c.bytesOut.Add(int64(len(res.reply)))
	ml.log()
}

// pinger pings every PingInterval; a peer that does not answer within the
// interval is gone.
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

// close closes the connection once, then cancels its context, which ends the
// reader and aborts core calls still in flight.
func (c *wsConn) close(code websocket.StatusCode, _ string) {
	c.closeOnce.Do(func() {
		go func() {
			_ = c.conn.Close(code, "")
			c.cancel()
		}()
	})
}

// goAway closes gently: frames read meanwhile are dropped, messages already
// in flight are answered (bounded by grace), then "going away".
func (c *wsConn) goAway(reason string, grace time.Duration) {
	c.admitMu.Lock()
	first := c.going.CompareAndSwap(false, true)
	c.admitMu.Unlock()
	if !first {
		return
	}
	c.setReason(reason)
	done := make(chan struct{})
	go func() {
		c.inflight.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(grace):
	case <-c.ctx.Done():
	}
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

// wsMessageLog is one relayed frame's line: debug when answered, at the
// failure's level otherwise. The connection line carries the client details.
type wsMessageLog struct {
	connID, messageID string
	start             time.Time
	size              int
	res               relayResult
	writeErr          error
}

func (l wsMessageLog) log() {
	level, msg, outcome := slog.LevelDebug, "websocket message relayed", outcomeOK
	err := l.writeErr
	if l.res.err != nil {
		level, msg, outcome, err = l.res.level, "websocket message failed", l.res.outcome, l.res.err
	} else if err != nil {
		level, msg, outcome = slog.LevelInfo, "websocket message failed", outcomeClientGone
	}
	if !slog.Default().Enabled(context.Background(), level) {
		return
	}
	attrs := []slog.Attr{
		slog.String("conn_id", l.connID),
		slog.String("request_id", l.messageID),
		slog.Int("body_bytes", l.size),
		slog.Int("response_bytes", len(l.res.reply)),
		slog.Int("core_status", l.res.coreStatus),
		slog.Float64("core_ms", ms(l.res.coreTime)),
		slog.Float64("duration_ms", ms(time.Since(l.start))),
		slog.String("outcome", outcome),
	}
	if l.res.stage != "" {
		attrs = append(attrs, slog.String("stage", l.res.stage))
	}
	if err != nil {
		attrs = append(attrs, slog.String("err", err.Error()))
	}
	slog.LogAttrs(context.Background(), level, msg, attrs...)
}

// CloseWebSockets closes every open connection gently (going away after their
// in-flight messages) and reports how many there were.
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
