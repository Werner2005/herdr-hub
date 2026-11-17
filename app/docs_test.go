package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDocumentationRoutes(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "docs"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docs", "index.html"), []byte("Swagger UI"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("Dashboard"), 0644); err != nil {
		t.Fatal(err)
	}
	a := &application{webDist: dir, docsEnabled: true}
	for _, path := range []string{"/docs", "/docs/", "/docs/openapi.json"} {
		for _, method := range []string{"GET", "HEAD"} {
			w := httptest.NewRecorder()
			a.serveWeb(w, httptest.NewRequest(method, path, nil))
			if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Fatalf("%s %s: %d %v", method, path, w.Code, w.Header())
			}
			if method == "HEAD" && w.Body.Len() != 0 {
				t.Fatal("HEAD returned a body")
			}
			if method == "GET" && path == "/docs/openapi.json" && !json.Valid(w.Body.Bytes()) {
				t.Fatal("invalid OpenAPI JSON")
			}
			if method == "GET" && path != "/docs/openapi.json" && w.Body.String() != "Swagger UI" {
				t.Fatal("served dashboard instead of documentation")
			}
		}
	}
	w := httptest.NewRecorder()
	a.serveWeb(w, httptest.NewRequest("POST", "/docs", nil))
	if w.Code != 405 || w.Header().Get("Allow") != "GET, HEAD" {
		t.Fatal("documentation accepts mutations")
	}
	w = httptest.NewRecorder()
	a.serveWeb(w, httptest.NewRequest("GET", "/docs/missing", nil))
	if w.Code != 404 || strings.Contains(w.Body.String(), "Dashboard") {
		t.Fatal("unknown docs path falls back to dashboard")
	}
	a.docsEnabled = false
	for _, path := range []string{"/docs", "/docs/", "/docs/index.html", "/docs/openapi.json", "/docs/missing"} {
		w = httptest.NewRecorder()
		a.serveWeb(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 404 {
			t.Fatalf("disabled documentation exposed at %s", path)
		}
	}
	w = httptest.NewRecorder()
	a.serveWeb(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 200 || w.Body.String() != "Dashboard" {
		t.Fatal("disabling docs disabled the dashboard")
	}
}

func TestOpenAPISpecification(t *testing.T) {
	var spec map[string]any
	if err := json.Unmarshal(openAPISpec, &spec); err != nil {
		t.Fatal(err)
	}
	if spec["openapi"] != "3.0.3" {
		t.Fatal("unexpected OpenAPI version")
	}
	var checkRefs func(any)
	checkRefs = func(value any) {
		switch v := value.(type) {
		case map[string]any:
			if ref, ok := v["$ref"].(string); ok {
				if !strings.HasPrefix(ref, "#/") {
					t.Fatalf("non-local reference %s", ref)
				}
				var target any = spec
				for _, key := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
					m, ok := target.(map[string]any)
					if !ok || m[key] == nil {
						t.Fatalf("unresolved reference %s", ref)
					}
					target = m[key]
				}
			}
			for _, child := range v {
				checkRefs(child)
			}
		case []any:
			for _, child := range v {
				checkRefs(child)
			}
		}
	}
	checkRefs(spec)
	paths := spec["paths"].(map[string]any)
	for _, path := range []string{"/api/api-tokens", "/api/api-tokens/{id}", "/api/workspaces", "/api/users", "/api/configurations", "/api/volumes", "/api/groups"} {
		if paths[path] == nil {
			t.Fatalf("missing endpoint %s", path)
		}
	}
}
