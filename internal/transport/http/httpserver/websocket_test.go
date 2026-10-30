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
)

// wsCore is a core stub for WebSocket tests: it echoes "resp:"+payload, and a
// payload starting with "slow" takes delay to answer, "fail" gets a 500.
type wsCore struct {
	srv   *httptest.Server
	delay time.Duration

	mu        sync.Mutex
	instances []string
	peers     []string
}

func newWSCore(t *testing.T, delay time.Duration) *wsCore {
	t.Helper()
	c := &wsCore{delay: delay}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		in, err := readSync(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		c.mu.Lock()
		c.instances = append(c.instances, in.ChannelInstance)
		c.peers = append(c.peers, in.PeerAddress)
		c.mu.Unlock()
		raw := in.Data
		switch {
		case strings.HasPrefix(string(raw), "fail"):
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		case strings.HasPrefix(string(raw), "slow"):
			time.Sleep(c.delay)
		}
		writeSync(w, in, append([]byte("resp:"), raw...))
	}))
	t.Cleanup(c.srv.Close)
	return c
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

func TestWebSocketRoundTrip(t *testing.T) {
	core := newWSCore(t, 0)
	h, url := newWSChannel(t, core, WebSocketConfig{})
	c := dial(t, url+"/any/path?x=1")

	for i, msg := range []string{"hello", "again"} {
		send(t, c, testUUID, msg)
		id, payload := recv(t, c)
		if id != formatUUID(testUUID) || payload != "resp:"+msg {
			t.Fatalf("reply %d = %s %q", i, id, payload)
		}
	}
	core.mu.Lock()
	instances := append([]string(nil), core.instances...)
	peers := append([]string(nil), core.peers...)
	core.mu.Unlock()
	if len(instances) != 2 || instances[0] != "http-main" {
		t.Errorf("instances at core = %v, want http-main twice", instances)
	}
	// Every frame carries the address the connection came from.
	if len(peers) != 2 || peers[0] != "127.0.0.1" || peers[1] != "127.0.0.1" {
		t.Errorf("peer addresses at core = %v, want 127.0.0.1 twice", peers)
	}
	st := h.Stats()
	if st.WSConnectionsOpen != 1 || st.WSConnectionsTotal != 1 || st.WSMessagesTotal != 2 || st.WSMessagesFailed != 0 {
		t.Errorf("stats = %+v", st)
	}
}

// Messages with different UUIDs are relayed concurrently; the same UUID's
// messages keep their order.
func TestWebSocketInflightOrdering(t *testing.T) {
	core := newWSCore(t, 300*time.Millisecond)
	_, url := newWSChannel(t, core, WebSocketConfig{MaxInflight: 4})
	c := dial(t, url)

	send(t, c, testUUID, "slow-1") // A: slow
	send(t, c, testUUID, "fast-2") // A: must wait for slow-1
	send(t, c, uuidB, "fast-3")    // B: overtakes A

	var got []string
	for range 3 {
		id, payload := recv(t, c)
		got = append(got, id[:4]+":"+payload)
	}
	a, b := formatUUID(testUUID)[:4], formatUUID(uuidB)[:4]
	want := []string{b + ":resp:fast-3", a + ":resp:slow-1", a + ":resp:fast-2"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("replies = %v, want %v", got, want)
		}
	}
}

func TestWebSocketRefusedHandshakeIs404(t *testing.T) {
	core := newWSCore(t, 0)
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
	core := newWSCore(t, 0)
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

	c = dial(t, url)
	send(t, c, testUUID, "fail")
	if code := closed(t, c); code != websocket.StatusNormalClosure {
		t.Errorf("core failure: close = %v", code)
	}
	if st := h.Stats(); st.WSMessagesFailed != 1 || st.CoreErrorsTotal != 1 || st.TooLargeTotal != 1 {
		t.Errorf("stats = %+v", st)
	}
}

// Drain and shutdown close connections with "going away", after answering
// what is already in flight.
func TestWebSocketDrainAndShutdown(t *testing.T) {
	core := newWSCore(t, 200*time.Millisecond)
	h, url := newWSChannel(t, core, WebSocketConfig{})

	c := dial(t, url)
	send(t, c, testUUID, "slow")
	time.Sleep(50 * time.Millisecond) // the message is in flight
	h.SetDraining(true)
	if _, payload := recv(t, c); payload != "resp:slow" {
		t.Fatalf("in-flight reply = %q", payload)
	}
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
	core := newWSCore(t, 0)
	_, url := newWSChannel(t, core, WebSocketConfig{IdleTimeout: 100 * time.Millisecond})
	c := dial(t, url)
	start := time.Now()
	closed(t, c)
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("idle connection closed after %v", d)
	}
}
