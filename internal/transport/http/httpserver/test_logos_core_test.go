package httpserver

import (
	"encoding/base64"
	"encoding/json"
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
	var in protocol.InboundMinionMessage
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	c.last = in
	c.mu.Unlock()

	var respEnc string
	if in.EncryptedData != "" {
		raw, _ := base64.StdEncoding.DecodeString(in.EncryptedData)
		respEnc = base64.StdEncoding.EncodeToString(append([]byte("resp:"), raw...))
	}

	out := protocol.OutboundMinionMessage{
		MessageID:     "out-1",
		Type:          protocol.TypeOutboundMinionMessage,
		Version:       protocol.VersionV1,
		Timestamp:     "2026-03-11T00:00:00Z",
		ID:            in.ID,
		EncryptedData: respEnc,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (c *testLogosCore) URL() string { return c.server.URL }
func (c *testLogosCore) Close()      { c.server.Close() }

func (c *testLogosCore) LastInbound() protocol.InboundMinionMessage {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.last
}
