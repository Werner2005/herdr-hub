package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// A zero-value, cancellation-aware mutation lock shared with reconciliation.
type contextMutex struct {
	once sync.Once
	ch   chan struct{}
}

func (m *contextMutex) lock(ctx context.Context) error {
	m.once.Do(func() { m.ch = make(chan struct{}, 1) })
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case m.ch <- struct{}{}:
		if err := ctx.Err(); err != nil {
			m.unlock()
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (m *contextMutex) unlock() { <-m.ch }

func (a *application) browserHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; font-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	if !a.insecureCookies {
		w.Header().Set("Strict-Transport-Security", "max-age=31536000")
	}
}

func (a *application) sessionCookieName() string {
	if a.insecureCookies {
		return "herdr_session"
	}
	return "__Host-herdr_session"
}
func (a *application) sessionCookie(value string, age int) *http.Cookie {
	return &http.Cookie{Name: a.sessionCookieName(), Value: value, Path: "/", HttpOnly: true, Secure: !a.insecureCookies, SameSite: http.SameSiteStrictMode, MaxAge: age}
}

// Read bounded bodies before acquiring the mutation lock. A socket deadline,
// not merely a context deadline, interrupts stalled HTTP request-body reads.
func bufferRequestBody(w http.ResponseWriter, r *http.Request, limit int64) bool {
	if r.Body == nil {
		return true
	}
	controller := http.NewResponseController(w)
	if err := controller.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		writeError(w, 500, "could not set request deadline")
		return false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	_ = r.Body.Close()
	if err != nil {
		var oversized *http.MaxBytesError
		if errors.As(err, &oversized) {
			writeError(w, 413, "request body too large")
		} else {
			writeError(w, 408, "could not read request body")
		}
		return false
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return true
}

type auditResponse struct {
	http.ResponseWriter
	status int
}

func (w *auditResponse) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}
func (w *auditResponse) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	return w.ResponseWriter.Write(b)
}
func (w *auditResponse) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Never log bodies, cookies, Authorization, query strings, tokens or passwords.
// JSON encoding prevents attacker-controlled names from injecting log lines.
func (a *application) audit(r *http.Request, u identity, status int) {
	actor := u.Username
	if actor == "" {
		actor = "anonymous"
		if u.Role == "admin" {
			actor = "api-token"
		}
	}
	path := r.URL.Path
	if len(path) > 256 {
		path = path[:256]
	}
	event := map[string]any{"event": "security_request", "actor": actor, "role": u.Role, "method": r.Method, "path": path, "status": status, "client_ip": a.clientIP(r)}
	b, _ := json.Marshal(event)
	if a.auditLogger != nil {
		a.auditLogger.Print(string(b))
	} else {
		log.Print(string(b))
	}
}

// Forwarded addresses are accepted only from explicitly configured proxies.
func (a *application) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer := net.ParseIP(host)
	trusted := func(ip net.IP) bool {
		for _, network := range a.trustedProxies {
			if network.Contains(ip) {
				return true
			}
		}
		return false
	}
	if !trusted(peer) {
		return host
	}
	forwarded := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for i := len(forwarded) - 1; i >= 0; i-- {
		ip := net.ParseIP(strings.TrimSpace(forwarded[i]))
		if ip == nil {
			return host
		}
		if !trusted(ip) {
			return ip.String()
		}
	}
	return host
}
