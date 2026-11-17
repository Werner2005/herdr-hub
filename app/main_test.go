package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestServeWeb(t *testing.T) {
	dist := t.TempDir()
	if err := os.Mkdir(filepath.Join(dist, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dist, "index.html"), []byte("<main>Herdr Hub</main>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dist, "assets", "app.js"), []byte("console.log('ready')"), 0o644); err != nil {
		t.Fatal(err)
	}

	app := &application{webDist: dist}
	tests := []struct {
		name       string
		path       string
		method     string
		status     int
		body       string
		wantInBody string
	}{
		{name: "index", path: "/", method: http.MethodGet, status: http.StatusOK, wantInBody: "Herdr Hub"},
		{name: "asset", path: "/assets/app.js", method: http.MethodGet, status: http.StatusOK, wantInBody: "console.log"},
		{name: "SPA route fallback", path: "/workspaces/demo", method: http.MethodGet, status: http.StatusOK, wantInBody: "Herdr Hub"},
		{name: "unknown API remains 404", path: "/api/missing", method: http.MethodGet, status: http.StatusNotFound, wantInBody: "endpoint not found"},
		{name: "non-read method rejected", path: "/", method: http.MethodPost, status: http.StatusMethodNotAllowed, wantInBody: "method not allowed"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
			app.serveWeb(recorder, request)
			if recorder.Code != test.status {
				t.Fatalf("status = %d, want %d; response: %s", recorder.Code, test.status, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), test.wantInBody) {
				t.Fatalf("response body %q does not contain %q", recorder.Body.String(), test.wantInBody)
			}
		})
	}
}
