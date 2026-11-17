package main

import (
	_ "embed"
	"net/http"
	"path/filepath"
)

//go:embed openapi.json
var openAPISpec []byte

func (a *application) serveDocs(w http.ResponseWriter, r *http.Request) {
	a.browserHeaders(w)
	w.Header().Set("Cache-Control", "no-store")
	if !a.docsEnabled {
		writeError(w, 404, "API documentation disabled")
		return
	}
	if r.Method != "GET" && r.Method != "HEAD" {
		w.Header().Set("Allow", "GET, HEAD")
		writeError(w, 405, "method not allowed")
		return
	}
	switch r.URL.Path {
	case "/docs/openapi.json":
		w.Header().Set("Content-Type", "application/json")
		if r.Method != "HEAD" {
			_, _ = w.Write(openAPISpec)
		}
	case "/docs", "/docs/", "/docs/index.html":
		// Swagger UI uses inline styles; scripts and requests stay same-origin.
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		http.ServeFile(w, r, filepath.Join(a.webDist, "docs", "index.html"))
	default:
		writeError(w, 404, "documentation endpoint not found")
	}
}
