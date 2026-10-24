package httpserver

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"

	protocol "github.com/logoscore/logos-golang-protocol/protocol"
)

const uuidLen = 16

// envelopeOverhead bounds the JSON fields around encrypted_data in a core reply.
const envelopeOverhead = 64 << 10

// New returns the client-facing server. Every request, whatever its method
// or path, is treated as a minion message; any failure answers with a plain 404.
func New(addr, channelID, syncBaseURL string, maxBodyBytes int64) *http.Server {
	syncURL := strings.TrimRight(syncBaseURL, "/") + "/api/channel/sync"
	client := &http.Client{Timeout: 30 * time.Second}
	// Core returns encrypted_data base64-encoded (4/3 of the raw size).
	maxReplyBytes := 2*maxBodyBytes + envelopeOverhead

	// A bare handler, not a ServeMux: the mux answers unclean paths with
	// redirects, and every response here must be either the reply or a 404.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
		if err != nil {
			log.Printf("read body: %v", err)
			http.NotFound(w, r)
			return
		}

		id, encryptedData, err := unpack(body)
		if err != nil {
			log.Printf("unpack: %v", err)
			http.NotFound(w, r)
			return
		}

		inbound := protocol.InboundMinionMessage{
			MessageID: fmt.Sprintf("%s-%d", channelID, time.Now().UnixNano()),
			Type:      protocol.TypeInboundMinionMessage,
			Version:   protocol.VersionV1,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Source: protocol.SourceInfo{
				Module:         "channel",
				ModuleInstance: channelID,
				Transport:      "http",
				Tenant:         "default",
			},
			ID:            id,
			EncryptedData: encryptedData,
		}

		outbound, err := syncWithCore(r.Context(), client, syncURL, maxReplyBytes, inbound)
		if err != nil {
			log.Printf("core sync: %v", err)
			http.NotFound(w, r)
			return
		}

		resp, err := pack(outbound.ID, outbound.EncryptedData)
		if err != nil {
			log.Printf("pack: %v", err)
			http.NotFound(w, r)
			return
		}

		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(resp)
	})

	return &http.Server{Addr: addr, Handler: handler}
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

	if resp.StatusCode != http.StatusOK {
		return protocol.OutboundMinionMessage{}, fmt.Errorf("core sync returned %d", resp.StatusCode)
	}

	var outbound protocol.OutboundMinionMessage
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxReplyBytes)).Decode(&outbound); err != nil {
		return protocol.OutboundMinionMessage{}, err
	}
	return outbound, nil
}
