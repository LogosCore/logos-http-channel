package httpserver

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	protocol "github.com/logoscore/logos-golang-protocol/protocol"
)

type testLogosCore struct {
	t      *testing.T
	server *httptest.Server
	mu     sync.RWMutex
	last   protocol.InboundMinionMessage
}

func newTestLogosCore(t *testing.T) *testLogosCore {
	t.Helper()
	c := &testLogosCore{t: t}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/channel/sync", c.handleSync)
	c.server = httptest.NewServer(mux)
	return c
}

func (c *testLogosCore) handleSync(w http.ResponseWriter, r *http.Request) {
	in, err := readSync(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	c.last = in
	c.mu.Unlock()
	writeSync(w, in, append([]byte("resp:"), in.Data...))
}

// readSync reads a sync request the way core does: the ciphertext as the raw
// body, the ids in headers.
func readSync(r *http.Request) (protocol.InboundMinionMessage, error) {
	if r.Header.Get("Content-Type") != protocol.SyncContentType {
		return protocol.InboundMinionMessage{}, fmt.Errorf("content type %q", r.Header.Get("Content-Type"))
	}
	in := protocol.InboundFromHeaders(r.Header)
	in.Data, _ = io.ReadAll(r.Body)
	return in, protocol.ValidateInbound(in)
}

// writeSync answers a sync request: 200 with data for the minion, or 204
// when there is none.
func writeSync(w http.ResponseWriter, in protocol.InboundMinionMessage, data []byte) {
	protocol.OutboundMinionMessage{MessageID: protocol.NewID(), MinionID: in.MinionID, Data: data}.SetHeaders(w.Header())
	if len(data) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (c *testLogosCore) URL() string { return c.server.URL }
func (c *testLogosCore) Close()      { c.server.Close() }

func (c *testLogosCore) LastInbound() protocol.InboundMinionMessage {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.last
}
