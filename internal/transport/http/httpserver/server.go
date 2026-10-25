package httpserver

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	protocol "github.com/logoscore/logos-golang-protocol/protocol"
)

const uuidLen = 16

// envelopeOverhead bounds the JSON fields around encrypted_data in a core reply.
const envelopeOverhead = 64 << 10

// CoreConfig is how the handler reaches core. It comes from the env only and
// is fixed for the process lifetime.
type CoreConfig struct {
	SyncBaseURL string
	// SyncTimeout bounds each request's wait for core's reply (default 30s).
	SyncTimeout time.Duration
}

// RuntimeConfig holds the settings core can change live via management.
type RuntimeConfig struct {
	MaxBodyBytes int64
}

// Stats is a snapshot of the handler's counters.
type Stats struct {
	RequestsTotal   int64
	SucceededTotal  int64
	FailedTotal     int64 // every 404 returned
	CoreErrorsTotal int64 // core unreachable, non-200, or bad reply
	RejectedByCore  int64 // core answered 403: instance not registered
	LastCoreError   string
	LastCoreErrorAt time.Time
	Draining        bool
}

type coreError struct {
	msg string
	at  time.Time
}

// errRejectedByCore marks a 403 from core.
var errRejectedByCore = errors.New("core sync returned 403")

// Handler is the client-facing message handler. Its runtime config is read
// per request, so SetConfig takes effect without a restart. It is a bare
// handler, not a ServeMux: the mux answers unclean paths with redirects, and
// every response here must be either the reply or a 404.
type Handler struct {
	channelID   string
	client      *http.Client
	syncURL     string
	syncTimeout time.Duration

	cfg      atomic.Pointer[RuntimeConfig]
	draining atomic.Bool

	requests, succeeded, failed, coreErrors, rejected atomic.Int64
	lastCoreErr                                       atomic.Pointer[coreError]
}

// NewHandler returns a handler forwarding to {core.SyncBaseURL}/api/channel/sync.
func NewHandler(channelID string, core CoreConfig, cfg RuntimeConfig) *Handler {
	if core.SyncTimeout <= 0 {
		core.SyncTimeout = 30 * time.Second
	}
	h := &Handler{
		channelID: channelID,
		// Timeouts are per request (syncTimeout).
		client:      &http.Client{},
		syncURL:     SyncURL(core.SyncBaseURL),
		syncTimeout: core.SyncTimeout,
	}
	h.SetConfig(cfg)
	return h
}

// SyncURL returns core's sync endpoint for a base URL.
func SyncURL(syncBaseURL string) string {
	return strings.TrimRight(syncBaseURL, "/") + "/api/channel/sync"
}

// New returns the client-facing server for h. Every request, whatever its
// method or path, is treated as a minion message; any failure answers with a
// plain 404.
func New(addr string, h *Handler) *http.Server {
	return &http.Server{Addr: addr, Handler: h}
}

// SetConfig replaces the runtime config used by subsequent requests.
func (h *Handler) SetConfig(cfg RuntimeConfig) {
	h.cfg.Store(&cfg)
}

// Config returns the current runtime config.
func (h *Handler) Config() RuntimeConfig { return *h.cfg.Load() }

// SetDraining toggles drain mode: while draining every request gets a 404.
func (h *Handler) SetDraining(v bool) { h.draining.Store(v) }

// Draining reports whether drain mode is on.
func (h *Handler) Draining() bool { return h.draining.Load() }

// SyncEndpoint returns the core sync URL requests are forwarded to.
func (h *Handler) SyncEndpoint() string { return h.syncURL }

// Client returns the HTTP client used for core calls.
func (h *Handler) Client() *http.Client { return h.client }

// Stats returns a snapshot of the counters.
func (h *Handler) Stats() Stats {
	st := Stats{
		RequestsTotal:   h.requests.Load(),
		SucceededTotal:  h.succeeded.Load(),
		FailedTotal:     h.failed.Load(),
		CoreErrorsTotal: h.coreErrors.Load(),
		RejectedByCore:  h.rejected.Load(),
		Draining:        h.draining.Load(),
	}
	if e := h.lastCoreErr.Load(); e != nil {
		st.LastCoreError, st.LastCoreErrorAt = e.msg, e.at
	}
	return st
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	cfg := h.cfg.Load()
	h.requests.Add(1)
	// Client mistakes log at info, core trouble at warn.
	fail := func(level slog.Level, stage string, err error) {
		h.failed.Add(1)
		slog.Log(r.Context(), level, "request failed", "stage", stage, "err", err, "method", r.Method, "path", r.URL.Path)
		http.NotFound(w, r)
	}

	if h.draining.Load() {
		h.failed.Add(1)
		slog.Debug("request refused: draining", "method", r.Method, "path", r.URL.Path)
		http.NotFound(w, r)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, cfg.MaxBodyBytes))
	if err != nil {
		fail(slog.LevelInfo, "read body", err)
		return
	}

	id, encryptedData, err := unpack(body)
	if err != nil {
		fail(slog.LevelInfo, "unpack", err)
		return
	}

	inbound := protocol.InboundMinionMessage{
		MessageID: fmt.Sprintf("%s-%d", h.channelID, time.Now().UnixNano()),
		Type:      protocol.TypeInboundMinionMessage,
		Version:   protocol.VersionV1,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Source: protocol.SourceInfo{
			Module:         "channel",
			ModuleInstance: h.channelID,
			Transport:      "http",
			Tenant:         "default",
		},
		ID:            id,
		EncryptedData: encryptedData,
	}

	ctx, cancel := context.WithTimeout(r.Context(), h.syncTimeout)
	defer cancel()
	// Core returns encrypted_data base64-encoded (4/3 of the raw size).
	maxReplyBytes := 2*cfg.MaxBodyBytes + envelopeOverhead
	outbound, err := syncWithCore(ctx, h.client, h.syncURL, maxReplyBytes, inbound)
	if err != nil {
		if errors.Is(err, errRejectedByCore) {
			h.rejected.Add(1)
		} else if r.Context().Err() == nil {
			// A client that went away is not a core error.
			h.coreErrors.Add(1)
			h.lastCoreErr.Store(&coreError{msg: err.Error(), at: time.Now().UTC()})
		}
		fail(slog.LevelWarn, "core sync", err)
		return
	}

	resp, err := pack(outbound.ID, outbound.EncryptedData)
	if err != nil {
		h.coreErrors.Add(1)
		h.lastCoreErr.Store(&coreError{msg: "pack reply: " + err.Error(), at: time.Now().UTC()})
		fail(slog.LevelWarn, "pack", err)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(resp)
	h.succeeded.Add(1)
	slog.Debug("request served", "method", r.Method, "path", r.URL.Path, "id", id)
}

// NewHealth returns the internal health server. It listens on its own port,
// which is not published outside the Docker network.
func NewHealth(addr string) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return &http.Server{Addr: addr, Handler: mux}
}

// unpack parses: [offset:1][enc_part1:offset][id:16][enc_part2:rest]
func unpack(body []byte) (id string, encryptedData string, err error) {
	if len(body) < 1+uuidLen {
		return "", "", fmt.Errorf("body too short: need at least %d bytes", 1+uuidLen)
	}

	offset := int(body[0])
	if 1+offset+uuidLen > len(body) {
		return "", "", fmt.Errorf("invalid offset %d for body length %d", offset, len(body))
	}

	encPart1 := body[1 : 1+offset]
	rawID := body[1+offset : 1+offset+uuidLen]
	encPart2 := body[1+offset+uuidLen:]

	id = formatUUID(rawID)

	enc := make([]byte, 0, len(encPart1)+len(encPart2))
	enc = append(enc, encPart1...)
	enc = append(enc, encPart2...)
	if len(enc) > 0 {
		encryptedData = base64.StdEncoding.EncodeToString(enc)
	}
	return id, encryptedData, nil
}

// pack builds: [offset:1][enc_part1:offset][id:16][enc_part2:rest]
func pack(id string, encryptedData string) ([]byte, error) {
	rawID, err := parseUUID(id)
	if err != nil {
		return nil, err
	}

	var encBytes []byte
	if encryptedData != "" {
		encBytes, err = base64.StdEncoding.DecodeString(encryptedData)
		if err != nil {
			return nil, fmt.Errorf("invalid base64 encrypted_data: %w", err)
		}
	}

	maxOffset := len(encBytes)
	if maxOffset > 255 {
		maxOffset = 255
	}
	offset := 0
	if maxOffset > 0 {
		offset = rand.IntN(maxOffset + 1)
	}

	buf := make([]byte, 0, 1+len(encBytes)+uuidLen)
	buf = append(buf, byte(offset))
	buf = append(buf, encBytes[:offset]...)
	buf = append(buf, rawID...)
	buf = append(buf, encBytes[offset:]...)
	return buf, nil
}

func formatUUID(b []byte) string {
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func parseUUID(s string) ([]byte, error) {
	clean := strings.ReplaceAll(s, "-", "")
	if len(clean) != 32 {
		return nil, fmt.Errorf("invalid UUID: %s", s)
	}
	return hex.DecodeString(clean)
}

func syncWithCore(ctx context.Context, client *http.Client, syncURL string, maxReplyBytes int64, inbound protocol.InboundMinionMessage) (protocol.OutboundMinionMessage, error) {
	payload, err := json.Marshal(inbound)
	if err != nil {
		return protocol.OutboundMinionMessage{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, syncURL, bytes.NewReader(payload))
	if err != nil {
		return protocol.OutboundMinionMessage{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return protocol.OutboundMinionMessage{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusForbidden {
		return protocol.OutboundMinionMessage{}, errRejectedByCore
	}
	if resp.StatusCode != http.StatusOK {
		return protocol.OutboundMinionMessage{}, fmt.Errorf("core sync returned %d", resp.StatusCode)
	}

	var outbound protocol.OutboundMinionMessage
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxReplyBytes)).Decode(&outbound); err != nil {
		return protocol.OutboundMinionMessage{}, err
	}
	return outbound, nil
}
