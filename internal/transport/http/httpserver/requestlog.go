package httpserver

import (
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/logoscore/logos-golang-channel-core/v2/pkg/logging"
)

// Request outcomes, logged as "outcome".
const (
	outcomeOK             = "ok"
	outcomeDraining       = "draining"
	outcomeBadRequest     = "bad_request"      // unreadable or malformed body
	outcomeCoreError      = "core_error"       // core unreachable, non-200 or bad reply
	outcomeRejectedByCore = "rejected_by_core" // core answered 403: not registered
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
}

func (l *requestLog) fail(level slog.Level, outcome, stage string, err error) {
	l.level, l.msg, l.outcome, l.stage, l.err = level, "request failed", outcome, stage, err
}

// log writes the request line. The body itself is never logged. Nothing is
// built when the line would be dropped (served requests at the default info
// level); the hub's Enabled already counts a viewer asking for debug.
func (l *requestLog) log(r *http.Request, w *countingWriter, trusted []netip.Prefix) {
	if !slog.Default().Enabled(r.Context(), l.level) {
		return
	}
	attrs := append([]slog.Attr{slog.String("request_id", l.id)}, clientAttrs(r, trusted)...)
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
func clientAttrs(r *http.Request, trusted []netip.Prefix) []slog.Attr {
	return []slog.Attr{
		slog.String("remote_addr", r.RemoteAddr),
		slog.String("client_ip", clientIP(r, trusted)),
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

// clientIP is the peer address, or, when the peer is a trusted proxy, the
// client it forwarded for: the rightmost X-Forwarded-For entry that is not
// itself a trusted proxy, else X-Real-IP. Headers from untrusted peers are
// ignored, since anyone can send them.
func clientIP(r *http.Request, trusted []netip.Prefix) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(host)
	if err != nil || !isTrusted(peer, trusted) {
		return host
	}
	if xff := r.Header.Values("X-Forwarded-For"); len(xff) > 0 {
		hops := strings.Split(strings.Join(xff, ","), ",")
		for i := len(hops) - 1; i >= 0; i-- {
			hop := strings.TrimSpace(hops[i])
			if hop == "" {
				continue
			}
			a, ok := parseHop(hop)
			if !ok {
				return hop // not an address; report what the proxy said
			}
			if !isTrusted(a, trusted) {
				return a.String()
			}
		}
		// Every hop is a trusted proxy: the leftmost is the closest we know.
		if a, ok := parseHop(strings.TrimSpace(hops[0])); ok {
			return a.String()
		}
		return host
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

func isTrusted(a netip.Addr, trusted []netip.Prefix) bool {
	a = a.Unmap()
	for _, p := range trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
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
