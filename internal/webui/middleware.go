package webui

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

// SessionHeader carries the session secret on /rpc and /events (C4).
const SessionHeader = "X-ClawBridge-Session"

// CSP is the exact Content-Security-Policy of every response (C7, R1-T5).
const CSP = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self'; font-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'; require-trusted-types-for 'script'; trusted-types 'none'"

// SecurityHeaders is the exact header set on every response (C7).
var SecurityHeaders = map[string]string{
	"Content-Security-Policy":      CSP,
	"X-Frame-Options":              "DENY",
	"Referrer-Policy":              "no-referrer",
	"Cache-Control":                "no-store",
	"X-Content-Type-Options":       "nosniff",
	"Cross-Origin-Opener-Policy":   "same-origin",
	"Cross-Origin-Resource-Policy": "same-origin",
}

const maxLogPath = 64

// guard is the response writer of every request. It sets the C7 headers
// when the status is written, so no handler (the event stream's, an error
// page) can leave one out or change it, records the status for the log, and
// keeps an event stream's write deadline fresh.
type guard struct {
	http.ResponseWriter
	status int
	// deadline, if positive, is the write timeout set before each write.
	deadline time.Duration
}

func (g *guard) apply() {
	h := g.ResponseWriter.Header()
	for k, v := range SecurityHeaders {
		h.Set(k, v)
	}
}

func (g *guard) WriteHeader(code int) {
	if g.status != 0 {
		return
	}
	g.status = code
	g.apply()
	g.refresh()
	g.ResponseWriter.WriteHeader(code)
}

func (g *guard) refresh() {
	if g.deadline > 0 {
		// An unsupported writer just keeps the server's own timeouts.
		http.NewResponseController(g.ResponseWriter).SetWriteDeadline(time.Now().Add(g.deadline))
	}
}

func (g *guard) Write(b []byte) (int, error) {
	if g.status == 0 {
		g.WriteHeader(http.StatusOK)
	}
	g.refresh()
	return g.ResponseWriter.Write(b)
}

func (g *guard) Flush() {
	if g.status == 0 {
		g.WriteHeader(http.StatusOK)
	}
	g.refresh()
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (g *guard) Unwrap() http.ResponseWriter { return g.ResponseWriter }

// logger writes one line per request: method, path and status. Never the
// query string, a header, a body, a token or a secret (C12).
type logger struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *logger) request(r *http.Request, status int) {
	if l == nil || l.w == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(l.w, "ui: %s %s %d\n", logText(r.Method, 16), logText(r.URL.Path, maxLogPath), status)
}

// logText bounds s and drops what does not print, then quotes it: both come
// from the caller.
func logText(s string, max int) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if !unicode.IsPrint(r) {
			continue
		}
		if n == max {
			b.WriteString("…")
			break
		}
		b.WriteRune(r)
		n++
	}
	return strconv.Quote(b.String())
}

func refuse(w http.ResponseWriter, code int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	io.WriteString(w, body+"\n")
}

// secure is the outermost handler: C7 and C12, then C5, C6 and C4 in that
// order, then the routes.
func (s *Server) secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g := &guard{ResponseWriter: w}
		if r.URL.Path == "/events" {
			g.deadline = s.cfg.EventWriteTimeout
		}
		g.apply()
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			// A slow body cannot hold a connection open (U3). An unsupported
			// writer keeps the server's own limits.
			http.NewResponseController(w).SetReadDeadline(time.Now().Add(s.cfg.BodyReadTimeout))
		}
		defer func() {
			if g.status == 0 {
				g.status = http.StatusOK
			}
			s.log.request(r, g.status)
		}()
		if !s.hostOK(r.Host) {
			refuse(g, http.StatusMisdirectedRequest, "misdirected request")
			return
		}
		if (r.Method != http.MethodGet && r.Method != http.MethodHead || r.URL.Path == "/events") && !sameOrigin(r) {
			refuse(g, http.StatusForbidden, "forbidden")
			return
		}
		if r.URL.Path == "/rpc" || r.URL.Path == "/events" {
			if !s.tokens.validSecret(r.Header.Get(SessionHeader)) {
				// The same answer whether the secret is missing or wrong.
				refuse(g, http.StatusUnauthorized, "unauthorized")
				return
			}
		}
		next.ServeHTTP(g, r)
	})
}

// hostOK is C5: exactly 127.0.0.1:P or localhost:P.
func (s *Server) hostOK(host string) bool {
	return host == "127.0.0.1:"+s.port || host == "localhost:"+s.port
}

// sameOrigin is C6. It is a browser-only defence; C3 and C4 guard against a
// local process.
func sameOrigin(r *http.Request) bool {
	site := r.Header.Get("Sec-Fetch-Site")
	if site != "" && site != "same-origin" {
		return false
	}
	origins := r.Header.Values("Origin")
	switch len(origins) {
	case 0:
		return site == "same-origin"
	case 1:
		return origins[0] == "http://"+r.Host
	}
	return false
}
