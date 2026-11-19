package hubclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Werner2005/herdr-hub/internal/workload"
)

var testToken = "hhw_" + strings.Repeat("0", 64)

func TestClientRequest(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != workload.APIPrefix+"/workloads" || r.Method != "POST" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer "+testToken || r.Header.Get("Content-Type") != "application/json" {
			t.Error("missing authentication or content type")
		}
		var spec workload.Spec
		if err := json.NewDecoder(r.Body).Decode(&spec); err != nil || spec.Name != "demo" {
			t.Error("invalid request body")
		}
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(workload.Item{Name: spec.Name, Status: "pending"})
	}))
	defer server.Close()
	c, err := New(server.URL, testToken, server.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	var result workload.Item
	if err = c.Do(context.Background(), "POST", "/workloads", workload.Spec{Name: "demo"}, &result); err != nil {
		t.Fatal(err)
	}
	if result.Name != "demo" || result.Status != "pending" {
		t.Fatalf("unexpected result %+v", result)
	}
}

func TestRejectInsecureConfiguration(t *testing.T) {
	for _, base := range []string{
		"http://hub.example", "https://user:pass@hub.example", "https://hub.example/api",
		"https://hub.example?token=secret", "https://hub.example/#fragment", "",
	} {
		if _, err := New(base, testToken, nil); err == nil {
			t.Errorf("accepted URL %q", base)
		}
	}
	for _, token := range []string{"", strings.Repeat("a", 64), "hhw_" + strings.Repeat("x", 64), testToken + "\n"} {
		if _, err := New("https://hub.example", token, nil); err == nil {
			t.Error("accepted non-workload token")
		}
	}
}

func TestClientErrorsAndRedirects(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
	}{
		{"redirect", 307, "private-proxy-secret"},
		{"auth", 401, "private-proxy-secret"},
		{"unimplemented", 404, "private-proxy-secret"},
		{"invalid-json", 200, "<html>private-proxy-secret</html>"},
		{"oversized", 200, strings.Repeat("x", maxResponse+1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Location", "/redirected")
				w.WriteHeader(test.status)
				_, _ = fmt.Fprint(w, test.body)
			}))
			defer server.Close()
			c, err := New(server.URL, testToken, server.Client().Transport)
			if err != nil {
				t.Fatal(err)
			}
			var result any
			err = c.Do(context.Background(), "GET", "/workloads", nil, &result)
			if err == nil || strings.Contains(err.Error(), "private-proxy-secret") {
				t.Fatalf("unsafe or missing error: %v", err)
			}
			if calls != 1 {
				t.Fatalf("followed redirect: %d calls", calls)
			}
		})
	}
}

func TestRejectPathEscape(t *testing.T) {
	c, err := New("https://hub.example", testToken, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{"/../users", "/workloads/a?token=bad", "//users", "/workloads/%2e%2e", "workloads", "/"} {
		if err := c.Do(context.Background(), "GET", endpoint, nil, nil); err == nil || err.Error() != "invalid workload API path" {
			t.Fatalf("did not reject path %q locally: %v", endpoint, err)
		}
	}
}

func TestCancellation(t *testing.T) {
	c, _ := New("https://hub.example", testToken, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Do(ctx, "GET", "/workloads", nil, nil); err != context.Canceled {
		t.Fatalf("expected canceled, got %v", err)
	}
}
