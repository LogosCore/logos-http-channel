package httpserver

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// testLag is the simulated latency the tests use, each way.
const testLag = 50 * time.Millisecond

// A delay is the latency spread by its jitter, either way, never below 0;
// a jitter past 100 % counts as 100 %.
func TestLatencyNext(t *testing.T) {
	if d := (Latency{Jitter: 50}).next(); d != 0 {
		t.Fatalf("no latency: %s", d)
	}
	if d := (Latency{Delay: 100 * time.Millisecond}).next(); d != 100*time.Millisecond {
		t.Fatalf("no jitter: %s", d)
	}
	for _, c := range []struct {
		jitter  int
		lo, hi  time.Duration
		spreads bool
	}{
		{25, 75 * time.Millisecond, 125 * time.Millisecond, true},
		{100, 0, 200 * time.Millisecond, true},
		{500, 0, 200 * time.Millisecond, true},
		{-5, 100 * time.Millisecond, 100 * time.Millisecond, false},
	} {
		l := Latency{Delay: 100 * time.Millisecond, Jitter: c.jitter}
		below, above := false, false
		for range 2000 {
			d := l.next()
			if d < c.lo || d > c.hi {
				t.Fatalf("jitter %d: %s outside %s-%s", c.jitter, d, c.lo, c.hi)
			}
			below, above = below || d < l.Delay, above || d > l.Delay
		}
		if c.spreads && (!below || !above) {
			t.Errorf("jitter %d: never spread both ways", c.jitter)
		}
	}
}

// Items come out in the order they went in, each once its own time has
// come: a later one with a shorter delay waits behind the one before it.
// A mark is closed once the items ahead of it went on.
func TestDelayLine(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l := newDelayLine[int]()
	type out struct {
		n  int
		at time.Time
	}
	got := make(chan out, 10)
	go l.run(ctx, func(n int) bool { got <- out{n, time.Now()}; return true })

	start := time.Now()
	due := make([]time.Time, 5)
	for i := range 5 {
		d := time.Duration(5-i) * 20 * time.Millisecond // 100ms, 80ms, … 20ms
		due[i] = time.Now().Add(d)
		if !l.put(ctx, i, d) {
			t.Fatal("put")
		}
	}
	if !l.busy() {
		t.Fatal("not busy with items on their way")
	}
	mark := l.after(ctx, 0)
	for i := range 5 {
		o := <-got
		if o.n != i {
			t.Fatalf("item %d came out %d-th", o.n, i)
		}
		if o.at.Before(due[i]) {
			t.Errorf("item %d came out %s before its time", i, due[i].Sub(o.at))
		}
		if o.at.Before(start.Add(100 * time.Millisecond)) {
			t.Errorf("item %d overtook the first, due at 100ms: out at %s", i, o.at.Sub(start))
		}
	}
	if !wait(ctx, mark) {
		t.Fatal("the mark never closed")
	}
	for deadline := time.Now().Add(time.Second); l.busy() && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	if l.busy() {
		t.Error("still busy with every item handed on")
	}
}

// Through a plain request the message reaches core a delay late and its
// answer, a 404 too, takes another to come back. A handler built without
// the option ignores the latency, and 0s is at once again.
func TestSimulatedLatencyRequests(t *testing.T) {
	core := newTestLogosCore(t)
	defer core.Close()
	cfg := RuntimeConfig{MaxBodyBytes: testMaxBody, Latency: Latency{Delay: testLag}}
	timed := func(h *Handler, body []byte) (int, time.Duration) {
		ts := httptest.NewServer(h)
		defer ts.Close()
		start := time.Now()
		resp := post(t, ts.URL, body)
		_ = resp.Body.Close()
		return resp.StatusCode, time.Since(start)
	}
	payload := buildPayload(0, []byte("x"), testUUID)

	if status, took := timed(NewHandler("http-main", CoreConfig{BaseURL: core.URL()}, cfg), payload); status != http.StatusOK || took >= testLag {
		t.Errorf("without the option: %d in %s, want 200 at once", status, took)
	}
	h := NewHandler("http-main", CoreConfig{BaseURL: core.URL()}, cfg, WithSimulatedLatency())
	if status, took := timed(h, payload); status != http.StatusOK || took < 2*testLag {
		t.Errorf("simulated: %d in %s, want 200 after a round trip of %s", status, took, 2*testLag)
	}
	if status, took := timed(h, []byte{1, 2}); status != http.StatusNotFound || took < 2*testLag {
		t.Errorf("a 404: %d in %s, want it after a round trip of %s", status, took, 2*testLag)
	}
	cfg.Latency = Latency{}
	h.SetConfig(cfg)
	if status, took := timed(h, payload); status != http.StatusOK || took >= testLag {
		t.Errorf("0s: %d in %s, want 200 at once", status, took)
	}
}

// newLaggedChannel is a channel simulating latency, at lag each way, in
// front of core. The test ends once the channel's connections have, their
// lines logged: the clients close first (their cleanups are later ones),
// and a line logged after the test would land in the next test's capture.
func newLaggedChannel(t *testing.T, core *wsCore, lag Latency) (*Handler, string) {
	t.Helper()
	h := NewHandler("http-main", CoreConfig{BaseURL: core.srv.URL}, RuntimeConfig{MaxBodyBytes: testMaxBody, Latency: lag},
		WithSimulatedLatency())
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	t.Cleanup(func() {
		for deadline := time.Now().Add(5 * time.Second); h.Stats().WSConnectionsOpen != 0; time.Sleep(5 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Errorf("%d connections still open", h.Stats().WSConnectionsOpen)
				return
			}
		}
	})
	return h, "ws" + strings.TrimPrefix(ts.URL, "http")
}

// Over a WebSocket the handshake answers a round trip late, and every
// frame goes on a delay after it came, each way: a message's reply comes a
// round trip late, and core's push right behind the reply comes with it (a
// burst is not spread out). Whatever the jitter, frames keep their order.
// 0s is at once again.
func TestSimulatedLatencyWebSocket(t *testing.T) {
	core := newWSCore(t)
	h, url := newLaggedChannel(t, core, Latency{Delay: testLag})

	start := time.Now()
	c := dial(t, url)
	if took := time.Since(start); took < 2*testLag {
		t.Errorf("handshake in %s, want a round trip of %s", took, 2*testLag)
	}
	start = time.Now()
	send(t, c, testUUID, "push")
	if _, payload := recv(t, c); payload != "resp:push" {
		t.Fatalf("reply %q", payload)
	}
	if took := time.Since(start); took < 2*testLag {
		t.Errorf("reply in %s, want a round trip of %s", took, 2*testLag)
	}
	start = time.Now()
	if _, payload := recv(t, c); payload != "pushed" {
		t.Fatalf("push %q", payload)
	}
	if gap := time.Since(start); gap >= testLag/2 {
		t.Errorf("the push came %s after the reply, want with it", gap)
	}

	cfg := h.Config()
	cfg.Latency.Jitter = 100
	h.SetConfig(cfg)
	const n = 20
	for i := range n {
		send(t, c, testUUID, fmt.Sprintf("m%02d", i))
	}
	for i := range n {
		if _, payload := recv(t, c); payload != fmt.Sprintf("resp:m%02d", i) {
			t.Fatalf("reply %d: %q, want the replies in order", i, payload)
		}
	}

	cfg.Latency = Latency{}
	h.SetConfig(cfg)
	start = time.Now()
	send(t, c, testUUID, "quick")
	if _, payload := recv(t, c); payload != "resp:quick" {
		t.Fatalf("reply %q", payload)
	}
	if took := time.Since(start); took >= testLag {
		t.Errorf("0s: reply in %s, want at once", took)
	}
	if st := h.Stats(); st.WSMessagesTotal != n+2 || st.WSMessagesFailed != 0 {
		t.Errorf("stats = %+v", st)
	}
}

// A close reaches the other side after the frames sent before it: core's
// reply before it closed its socket reaches the minion, then the close; the
// minion's last messages before it closed reach core.
func TestSimulatedLatencyCloses(t *testing.T) {
	core := newWSCore(t)
	_, url := newLaggedChannel(t, core, Latency{Delay: testLag})

	c := dial(t, url)
	send(t, c, testUUID, "drop")
	if _, payload := recv(t, c); payload != "resp:drop" {
		t.Fatalf("reply before core's close %q", payload)
	}
	if code := closed(t, c); code != websocket.StatusNormalClosure {
		t.Fatalf("after core's socket closed: %v", code)
	}

	c = dial(t, url)
	send(t, c, testUUID, "last-1")
	send(t, c, testUUID, "last-2")
	_ = c.Close(websocket.StatusNormalClosure, "")
	want := []string{"drop", "last-1", "last-2"}
	var got []string
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		core.mu.Lock()
		got = slices.Clone(core.got)
		core.mu.Unlock()
		if len(got) >= len(want) {
			break
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("core got %q, want %q", got, want)
	}
}

// With latency the channel's failures close the connection as they do
// without: core refusing the socket, a drain.
func TestSimulatedLatencyFailures(t *testing.T) {
	core := newWSCore(t)
	h, url := newLaggedChannel(t, core, Latency{Delay: testLag, Jitter: 50})

	core.setRefuse(true)
	c := dial(t, url)
	send(t, c, testUUID, "hello")
	if code := closed(t, c); code != websocket.StatusNormalClosure {
		t.Errorf("core refused: close = %v", code)
	}
	st := h.Stats()
	for deadline := time.Now().Add(5 * time.Second); st.WSConnectionsOpen != 0 && time.Now().Before(deadline); st = h.Stats() {
		time.Sleep(10 * time.Millisecond)
	}
	if st.WSMessagesFailed != 1 || st.CoreErrorsTotal != 1 {
		t.Errorf("stats = %+v", st)
	}

	core.setRefuse(false)
	c = dial(t, url)
	send(t, c, testUUID, "hello")
	if _, payload := recv(t, c); payload != "resp:hello" {
		t.Fatalf("reply %q", payload)
	}
	h.SetDraining(true)
	if code := closed(t, c); code != websocket.StatusGoingAway {
		t.Errorf("drain: close = %v, want going away", code)
	}
}
