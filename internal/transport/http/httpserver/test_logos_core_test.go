package httpserver

import (
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
	last   protocol.InboundAgentMessage
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
	var in protocol.InboundAgentMessage
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	c.last = in
	c.mu.Unlock()

	out := protocol.OutboundAgentMessage{
		MessageID:     "out-1",
		Type:          protocol.TypeOutboundAgentMessage,
		Version:       protocol.VersionV1,
		Timestamp:     "2026-03-11T00:00:00Z",
		Source:        protocol.SourceInfo{Module: "core", ModuleInstance: "main", Transport: "http", Tenant: "default"},
		ID:            in.ID + "-ack",
		EncryptedData: "resp:" + in.EncryptedData,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (c *testLogosCore) URL() string { return c.server.URL }
func (c *testLogosCore) Close()      { c.server.Close() }

func (c *testLogosCore) LastInbound() protocol.InboundAgentMessage {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.last
}
