package httpserver

import (
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/logoscore/logos-golang-module-core/pkg/logging"
)

// Request outcomes, logged as "outcome".
const (
	outcomeOK             = "ok"
	outcomeDraining       = "draining"
	outcomeBadRequest     = "bad_request"      // unreadable or malformed body
	outcomeTooLarge       = "too_large"        // body or frame over max_body_bytes
	outcomeCoreError      = "core_error"       // core unreachable, non-200 or bad reply
	outcomeRejectedByCore = "rejected_by_core" // core answered 403: not registered
	outcomeRefused        = "refused"          // core refused the message: 422 (does not open) or 409 (a replay)
	outcomeClientGone     = "client_gone"      // the client left while core was answering
	// WebSocket only.
	outcomeTooManyConnections = "too_many_connections" // ws_max_connections reached
	outcomeClosed             = "closed"               // the connection ended without a failure
)

// requestLog collects one request's facts; log writes them as one line when
// the request ends.
type requestLog struct {
	id    string
	start time.Time
	level slog.Level
	msg   string

	outcome string
	stage   string
	err     error

	bodyBytes  int64
	coreStatus int
	coreTime   time.Duration // zero when core was not called
	// lagged is the simulated latency the request waited (latency.go), part
	// of its duration.
	lagged time.Duration
}

func (l *requestLog) fail(level slog.Level, outcome, stage string, err error) {
	l.level, l.msg, l.outcome, l.stage, l.err = level, "request failed", outcome, stage, err
}

// log writes the request line. The body itself is never logged. Nothing is
// built when the line would be dropped (served requests at the default info
// level); the hub's Enabled already counts a viewer asking for debug.
func (l *requestLog) log(r *http.Request, w *countingWriter) {
	if !slog.Default().Enabled(r.Context(), l.level) {
		return
	}
	attrs := append([]slog.Attr{slog.String("request_id", l.id)}, clientAttrs(r)...)
	attrs = append(attrs, slog.Int64("body_bytes", l.bodyBytes))
	if l.coreTime > 0 {
		attrs = append(attrs, slog.Int("core_status", l.coreStatus), slog.Float64("core_ms", ms(l.coreTime)))
	}
	attrs = append(attrs,
		slog.Int("status", w.status()),
		slog.Int64("response_bytes", w.n),
		slog.Float64("duration_ms", ms(time.Since(l.start))),
		slog.String("outcome", l.outcome),
	)
	if l.lagged > 0 {
		attrs = append(attrs, slog.Float64("simulated_latency_ms", ms(l.lagged)))
	}
	if l.stage != "" {
		attrs = append(attrs, slog.String("stage", l.stage))
	}
	if l.err != nil {
		attrs = append(attrs, slog.String("err", l.err.Error()))
	}
	// Last, so a request with very many headers cannot push the fields above
	// out of the live tail's attr budget.
	attrs = append(attrs, logging.Headers(r.Header))
	slog.LogAttrs(r.Context(), l.level, l.msg, attrs...)
}

// clientAttrs describes the client and its request (a plain request, or the
// handshake of a WebSocket connection). Headers are added separately, last.
func clientAttrs(r *http.Request) []slog.Attr {
	return []slog.Attr{
		slog.String("remote_addr", r.RemoteAddr),
		slog.String("client_ip", clientIP(r)),
		slog.String("proto", r.Proto),
		slog.String("host", r.Host),
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.String("query", r.URL.RawQuery),
		slog.Int64("content_length", r.ContentLength),
	}
}

// ms is d in milliseconds with microsecond precision.
func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// peerAddress is clientIP as core gets it (Logos-Peer-Address): an IP
// address, or "" when the peer or its proxy named something else.
func peerAddress(r *http.Request) string {
	a, err := netip.ParseAddr(clientIP(r))
	if err != nil {
		return ""
	}
	return a.Unmap().String()
}

// clientIP is the client as the proxies in front of the channel name it: the
// leftmost address in X-Forwarded-For (the client the first proxy saw), else
// X-Real-IP, else the peer itself. Every proxy is believed, whatever its
// address: nginx on the host, a redirector, a CDN. Hops that name no address
// ("unknown", empty) are skipped.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	for _, hop := range strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",") {
		if a, ok := parseHop(strings.TrimSpace(hop)); ok {
			return a.String()
		}
	}
	if a, ok := parseHop(strings.TrimSpace(r.Header.Get("X-Real-IP"))); ok {
		return a.String()
	}
	return host
}

// parseHop reads an address as proxies write it: bare, or with a port
// ("1.2.3.4:5", "[::1]:5").
func parseHop(s string) (netip.Addr, bool) {
	if a, err := netip.ParseAddr(s); err == nil {
		return a.Unmap(), true
	}
	if ap, err := netip.ParseAddrPort(s); err == nil {
		return ap.Addr().Unmap(), true
	}
	return netip.Addr{}, false
}

// countingWriter records the status and how many body bytes were written.
type countingWriter struct {
	http.ResponseWriter
	code int
	n    int64
}

func (w *countingWriter) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *countingWriter) Write(b []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.n += int64(n)
	return n, err
}

func (w *countingWriter) status() int {
	if w.code == 0 {
		return http.StatusOK
	}
	return w.code
}
