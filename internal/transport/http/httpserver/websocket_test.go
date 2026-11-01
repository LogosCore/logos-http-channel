package httpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	protocol "github.com/logoscore/logos-golang-protocol/protocol"
)

// wsCore is a core stub for WebSocket tests: it serves connection sockets,
// answers each frame with "resp:"+payload, and, for a payload of "push",
// sends a frame of its own after; "drop" closes the socket after the reply.
// refuse makes it answer handshakes with 403.
type wsCore struct {
	srv *httptest.Server

	mu        sync.Mutex
	refuse    bool
	dials     int
	instances []string
	peers     []string
	kinds     []string
}

func newWSCore(t *testing.T) *wsCore {
	t.Helper()
	c := &wsCore{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		refuse := c.refuse
		c.mu.Unlock()
		if r.URL.Path != protocol.ConnectionPath || refuse {
			http.Error(w, "refused", http.StatusForbidden)
			return
		}
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = ws.CloseNow() }()
		c.mu.Lock()
		c.dials++
		c.instances = append(c.instances, r.Header.Get(protocol.HeaderChannelInstance))
		c.peers = append(c.peers, r.Header.Get(protocol.HeaderPeerAddress))
		c.kinds = append(c.kinds, r.Header.Get(protocol.HeaderConnectionKind))
		c.mu.Unlock()
		ctx := r.Context()
		for {
			_, b, err := ws.Read(ctx)
			if err != nil {
				return
			}
			f, err := protocol.DecodeConnFrame(b)
			if err != nil {
				return
			}
			out := []protocol.ConnFrame{{ID: f.ID, Data: append([]byte("resp:"), f.Data...)}}
			if string(f.Data) == "push" {
				out = append(out, protocol.ConnFrame{ID: f.ID, Data: []byte("pushed")})
			}
			for _, o := range out {
				enc, _ := protocol.EncodeConnFrame(o)
				if err := ws.Write(ctx, websocket.MessageBinary, enc); err != nil {
					return
				}
			}
			if string(f.Data) == "drop" {
				return
			}
		}
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *wsCore) setRefuse(v bool) {
	c.mu.Lock()
	c.refuse = v
	c.mu.Unlock()
}

func (c *wsCore) dialed() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dials
}

func newWSChannel(t *testing.T, core *wsCore, ws WebSocketConfig) (*Handler, string) {
	t.Helper()
	h := NewHandler("http-main", CoreConfig{SyncBaseURL: core.srv.URL}, RuntimeConfig{MaxBodyBytes: testMaxBody, WebSocket: ws})
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return h, "ws" + strings.TrimPrefix(ts.URL, "http")
}

func dial(t *testing.T, url string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", url, err)
	}
	t.Cleanup(func() { _ = c.CloseNow() })
	return c
}

var uuidB = []byte{0xa1, 0xb2, 0xc3, 0xd4, 0xe5, 0xf6, 0x47, 0x89, 0x9a, 0xbc, 0xde, 0xf0, 0x12, 0x34, 0x56, 0x78}

func send(t *testing.T, c *websocket.Conn, uuid []byte, data string) {
	t.Helper()
	if err := c.Write(context.Background(), websocket.MessageBinary, buildPayload(0, []byte(data), uuid)); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// recv reads one reply and returns its UUID and decoded payload.
func recv(t *testing.T, c *websocket.Conn) (string, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	typ, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if typ != websocket.MessageBinary {
		t.Fatalf("reply type = %v", typ)
	}
	id, raw, err := unpack(data)
	if err != nil {
		t.Fatalf("unpack reply: %v", err)
	}
	return id, string(raw)
}

// closed waits for the server to end the connection and returns its status.
func closed(t *testing.T, c *websocket.Conn) websocket.StatusCode {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		if _, _, err := c.Read(ctx); err != nil {
			if ctx.Err() != nil {
				t.Fatal("connection still open")
			}
			return websocket.CloseStatus(err)
		}
	}
}

// Frames go to core over a connection socket, which the channel opens with
// its instance, the kind and the minion's address; core's replies come back
// packed for the minion.
func TestWebSocketRoundTrip(t *testing.T) {
	core := newWSCore(t)
	h, url := newWSChannel(t, core, WebSocketConfig{})
	c := dial(t, url)
	send(t, c, testUUID, "one")
	if id, payload := recv(t, c); id != formatUUID(testUUID) || payload != "resp:one" {
		t.Fatalf("reply %s %q", id, payload)
	}
	send(t, c, uuidB, "two")
	if id, payload := recv(t, c); id != formatUUID(uuidB) || payload != "resp:two" {
		t.Fatalf("reply %s %q", id, payload)
	}
	core.mu.Lock()
	inst, kinds, peers, dials := core.instances, core.kinds, core.peers, core.dials
	core.mu.Unlock()
	if dials != 1 || inst[0] != "http-main" || kinds[0] != protocol.KindReversePersistent || peers[0] != "127.0.0.1" {
		t.Errorf("core saw %d sockets, instance %v, kind %v, peer %v", dials, inst, kinds, peers)
	}
	if st := h.Stats(); st.WSConnectionsOpen != 1 || st.WSConnectionsTotal != 1 || st.WSMessagesTotal != 2 || st.WSMessagesFailed != 0 {
		t.Errorf("stats = %+v", st)
	}
}

// What core sends unasked (a task pushed) reaches the minion.
func TestWebSocketPush(t *testing.T) {
	core := newWSCore(t)
	_, url := newWSChannel(t, core, WebSocketConfig{})
	c := dial(t, url)
	send(t, c, testUUID, "push")
	if _, payload := recv(t, c); payload != "resp:push" {
		t.Fatalf("reply %q", payload)
	}
	if id, payload := recv(t, c); id != formatUUID(testUUID) || payload != "pushed" {
		t.Fatalf("push %s %q", id, payload)
	}
}

// Core's socket drops: the minion's connection closes too, so the minion
// connects again and its next message ties a new socket to its session.
func TestWebSocketCoreSocketClosedClosesTheMinions(t *testing.T) {
	core := newWSCore(t)
	_, url := newWSChannel(t, core, WebSocketConfig{})
	c := dial(t, url)
	send(t, c, testUUID, "drop")
	if _, payload := recv(t, c); payload != "resp:drop" {
		t.Fatalf("reply %q", payload)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := c.Read(ctx); websocket.CloseStatus(err) != websocket.StatusNormalClosure {
		t.Fatalf("the minion's connection after core's socket closed: %v", err)
	}
	c = dial(t, url)
	send(t, c, testUUID, "again")
	if _, payload := recv(t, c); payload != "resp:again" {
		t.Fatalf("after reconnecting %q", payload)
	}
	if n := core.dialed(); n != 2 {
		t.Fatalf("core saw %d sockets, want 2", n)
	}
}

func TestWebSocketRefusedHandshakeIs404(t *testing.T) {
	core := newWSCore(t)
	h, url := newWSChannel(t, core, WebSocketConfig{MaxConnections: 1})
	httpURL := "http" + strings.TrimPrefix(url, "ws")

	// An upgrade request that is not a valid handshake (POST, no key).
	req, _ := http.NewRequest(http.MethodPost, httpURL+"/x", nil)
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	assertPlain404(t, resp, "invalid handshake")

	// The connection limit.
	dial(t, url)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, resp, err = websocket.Dial(ctx, url, nil)
	if err == nil || resp == nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("over the limit: err=%v resp=%v, want a 404", err, resp)
	}

	// Draining refuses handshakes.
	h.SetDraining(true)
	_, resp, err = websocket.Dial(ctx, url, nil)
	if err == nil || resp == nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("draining: err=%v resp=%v, want a 404", err, resp)
	}
}

func TestWebSocketFailuresCloseTheConnection(t *testing.T) {
	core := newWSCore(t)
	h, url := newWSChannel(t, core, WebSocketConfig{})

	c := dial(t, url)
	if err := c.Write(context.Background(), websocket.MessageText, []byte("hi")); err != nil {
		t.Fatal(err)
	}
	if code := closed(t, c); code != websocket.StatusNormalClosure {
		t.Errorf("text frame: close = %v", code)
	}

	c = dial(t, url)
	if err := c.Write(context.Background(), websocket.MessageBinary, []byte("short")); err != nil {
		t.Fatal(err)
	}
	if code := closed(t, c); code != websocket.StatusNormalClosure {
		t.Errorf("bad frame: close = %v", code)
	}

	c = dial(t, url)
	if err := c.Write(context.Background(), websocket.MessageBinary, make([]byte, testMaxBody+1)); err != nil {
		t.Fatal(err)
	}
	if code := closed(t, c); code != websocket.StatusMessageTooBig && code != websocket.StatusNormalClosure {
		t.Errorf("too large: close = %v", code)
	}

	// Core refuses the socket (not registered): the connection closes.
	core.setRefuse(true)
	c = dial(t, url)
	send(t, c, testUUID, "hello")
	if code := closed(t, c); code != websocket.StatusNormalClosure {
		t.Errorf("core refused: close = %v", code)
	}
	if st := h.Stats(); st.WSMessagesFailed != 1 || st.CoreErrorsTotal != 1 || st.TooLargeTotal != 1 {
		t.Errorf("stats = %+v", st)
	}
}

// Drain and shutdown close connections with "going away".
func TestWebSocketDrainAndShutdown(t *testing.T) {
	core := newWSCore(t)
	h, url := newWSChannel(t, core, WebSocketConfig{})

	c := dial(t, url)
	send(t, c, testUUID, "hello")
	if _, payload := recv(t, c); payload != "resp:hello" {
		t.Fatalf("reply %q", payload)
	}
	h.SetDraining(true)
	if code := closed(t, c); code != websocket.StatusGoingAway {
		t.Errorf("drain: close = %v, want going away", code)
	}

	h.SetDraining(false)
	c = dial(t, url)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() {
		_ = h.ShutdownWebSockets(ctx)
	}()
	if code := closed(t, c); code != websocket.StatusGoingAway {
		t.Errorf("shutdown: close = %v, want going away", code)
	}
}

func TestWebSocketIdleTimeout(t *testing.T) {
	core := newWSCore(t)
	_, url := newWSChannel(t, core, WebSocketConfig{IdleTimeout: 100 * time.Millisecond})
	c := dial(t, url)
	start := time.Now()
	closed(t, c)
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("idle connection closed after %v", d)
	}
}

// A minion that sends nothing but answers pings (one waiting for tasks) is
// not idle.
func TestWebSocketPongsKeepItOpen(t *testing.T) {
	core := newWSCore(t)
	_, url := newWSChannel(t, core, WebSocketConfig{IdleTimeout: 300 * time.Millisecond, PingInterval: 50 * time.Millisecond})
	c := dial(t, url)
	// Reading answers the channel's pings: one reader for the whole test (a
	// read whose context ends closes the connection).
	frames := make(chan []byte, 1)
	go func() {
		for {
			_, b, err := c.Read(context.Background())
			if err != nil {
				close(frames)
				return
			}
			frames <- b
		}
	}()
	time.Sleep(800 * time.Millisecond)
	send(t, c, testUUID, "still")
	select {
	case b, ok := <-frames:
		if !ok {
			t.Fatal("closed after 800 ms of quiet")
		}
		if _, raw, err := unpack(b); err != nil || string(raw) != "resp:still" {
			t.Fatalf("reply %q %v", raw, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no reply")
	}
}
