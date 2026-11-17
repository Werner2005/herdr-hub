package main

import (
	"bytes"
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMutationLockCancellation(t *testing.T) {
	var m contextMutex
	if err := m.lock(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if m.lock(ctx) == nil {
		t.Fatal("expired waiter acquired lock")
	}
	m.unlock()
	if m.lock(ctx) == nil {
		t.Fatal("cancelled context acquired available lock")
	}
	if err := m.lock(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.unlock()
}

func TestSlowBodyDoesNotHoldMutationLock(t *testing.T) {
	a := &application{token: "test-token"}
	reader, writer := io.Pipe()
	r := httptest.NewRequest("POST", "/api/users", reader)
	r.Header.Set("Authorization", "Bearer test-token")
	done := make(chan struct{})
	go func() { defer close(done); a.api(httptest.NewRecorder(), r) }()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := a.mu.lock(ctx); err != nil {
		t.Fatal("body reader held mutation lock", err)
	}
	read := httptest.NewRequest("GET", "/api/me", nil)
	read.Header.Set("Authorization", "Bearer test-token")
	w := httptest.NewRecorder()
	a.api(w, read)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	a.mu.unlock()
	_ = writer.Close()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("request did not finish")
	}
}

func TestBrowserSecurity(t *testing.T) {
	for _, insecure := range []bool{false, true} {
		a := &application{insecureCookies: insecure, webDist: t.TempDir()}
		cookie := a.sessionCookie("secret", 43200)
		if cookie.Secure == insecure || cookie.Path != "/" || cookie.Domain != "" || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
			t.Fatal("unsafe cookie", cookie)
		}
		if !insecure && !strings.HasPrefix(cookie.Name, "__Host-") {
			t.Fatal("missing cookie prefix")
		}
		w := httptest.NewRecorder()
		a.serveWeb(w, httptest.NewRequest("GET", "/", nil))
		if !strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") || w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatal("security headers missing")
		}
		if (w.Header().Get("Strict-Transport-Security") == "") != insecure {
			t.Fatal("incorrect HSTS")
		}
	}
}

func TestMalformedSessionsRejectedBeforeDatabase(t *testing.T) {
	a := &application{}
	for _, value := range []string{"", "short", strings.Repeat("z", 64), strings.Repeat("a", 65)} {
		r := httptest.NewRequest("GET", "/api/me", nil)
		r.AddCookie(&http.Cookie{Name: a.sessionCookieName(), Value: value})
		if _, ok := a.authenticate(r); ok {
			t.Fatal("malformed session accepted")
		}
	}
}

func TestAPIOriginAndBodyLimits(t *testing.T) {
	a := &application{token: "test-token"}
	for _, origin := range []string{"http://hub.test", "https://evil.test", "https://hub.test/path", "https://user@hub.test", "null"} {
		r := httptest.NewRequest("POST", "https://hub.test/api/users", nil)
		r.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		a.api(w, r)
		if w.Code != 403 {
			t.Fatalf("origin %s allowed: %d", origin, w.Code)
		}
	}
	r := httptest.NewRequest("POST", "/api/users", strings.NewReader(strings.Repeat("x", 65537)))
	r.Header.Set("Authorization", "Bearer test-token")
	w := httptest.NewRecorder()
	a.api(w, r)
	if w.Code != 413 {
		t.Fatal("oversized body accepted", w.Code)
	}
	server := newHTTPServer(":8080", http.NotFoundHandler())
	if server.ReadTimeout == 0 || server.WriteTimeout == 0 || server.IdleTimeout == 0 || server.MaxHeaderBytes > 16384 {
		t.Fatal("missing server limits")
	}
}

func TestTrustedProxyAddresses(t *testing.T) {
	a := &application{}
	r := httptest.NewRequest("GET", "/api/me", nil)
	r.RemoteAddr = "10.0.0.1:1234"
	r.Header.Set("X-Forwarded-For", "203.0.113.1, 198.51.100.2, 10.0.0.2")
	if a.clientIP(r) != "10.0.0.1" {
		t.Fatal("trusted spoofed header")
	}
	_, network, _ := net.ParseCIDR("10.0.0.0/8")
	a.trustedProxies = []*net.IPNet{network}
	if a.clientIP(r) != "198.51.100.2" {
		t.Fatal("did not walk chain from trusted end")
	}
	r.Header.Set("X-Forwarded-For", "invalid")
	if a.clientIP(r) != "10.0.0.1" {
		t.Fatal("accepted malformed address")
	}
}

func TestAuditRedaction(t *testing.T) {
	var output bytes.Buffer
	a := &application{auditLogger: log.New(&output, "", 0)}
	r := httptest.NewRequest("PATCH", "/api/users/alice?token=query-secret", strings.NewReader(`{"password":"body-secret"}`))
	r.Header.Set("Authorization", "Bearer bearer-secret")
	r.AddCookie(&http.Cookie{Name: "session", Value: "cookie-secret"})
	a.audit(r, identity{"admin", "admin"}, 200)
	text := output.String()
	for _, secret := range []string{"query-secret", "body-secret", "bearer-secret", "cookie-secret"} {
		if strings.Contains(text, secret) {
			t.Fatal("audit leaked a credential")
		}
	}
	if !strings.Contains(text, `"actor":"admin"`) || !strings.Contains(text, `"status":200`) {
		t.Fatal("audit omitted actor/outcome")
	}
}
