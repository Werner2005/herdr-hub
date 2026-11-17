package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestUsageQuantity(t *testing.T) {
	for _, test := range []struct {
		value string
		want  float64
	}{
		{"0", 0}, {"38765432n", 0.038765432}, {"500u", 0.0005}, {"125m", 0.125}, {"0.5", 0.5}, {"010", 10}, {"512Ki", 524288}, {"1Mi", 1048576}, {"2Gi", 2147483648}, {"1G", 1000000000}, {"1k", 1000},
	} {
		n, err := usageQuantity(test.value)
		if err != nil {
			t.Fatalf("%s: %v", test.value, err)
		}
		got, _ := n.Float64()
		if got != test.want {
			t.Errorf("%s: got %v want %v", test.value, got, test.want)
		}
	}
	for _, value := range []string{"", "-1", "NaN", "1GB", "999999999999999999999Gi"} {
		if _, err := usageQuantity(value); err == nil {
			t.Errorf("accepted invalid quantity %q", value)
		}
	}
}

func TestAggregateWorkspaceUsage(t *testing.T) {
	now := time.Now().UTC()
	metric := func(name, cpu, memory string, at time.Time) object {
		return object{"metadata": object{"name": name}, "timestamp": at.Format(time.RFC3339Nano), "containers": []any{object{"name": "sidecar", "usage": object{"cpu": "99", "memory": "99Gi"}}, object{"name": "herdr", "usage": object{"cpu": cpu, "memory": memory}}}}
	}
	owners := map[string]string{"demo-a": "demo", "demo-b": "demo", "other": "other", "zero": "zero"}
	metrics := []any{metric("demo-a", "125000000n", "128Mi", now), metric("demo-b", "250m", "256Mi", now.Add(-time.Minute)), metric("other", "1", "1Gi", now), metric("zero", "0", "0", now), metric("unrelated", "100", "100Gi", now)}
	got := aggregateWorkspaceUsage(owners, metrics, now)
	if len(got) != 3 || got["demo"].CPUMillicores != 375 || got["demo"].MemoryBytes != 384*1048576 || !got["demo"].SampledAt.Equal(now.Add(-time.Minute)) {
		t.Fatalf("unexpected aggregate: %+v", got)
	}
	if got["zero"] == nil || got["zero"].CPUMillicores != 0 {
		t.Fatal("valid zero usage treated as missing")
	}
	for _, invalid := range []object{metric("demo-b", "bad", "256Mi", now), metric("demo-b", "250m", "bad", now), metric("demo-b", "250m", "256Mi", now.Add(-3*time.Minute)), metric("demo-b", "250m", "256Mi", now.Add(time.Minute))} {
		got = aggregateWorkspaceUsage(owners, []any{metrics[0], invalid}, now)
		if got["demo"] != nil {
			t.Fatal("partial/invalid/stale metrics reported as complete")
		}
	}
	got = aggregateWorkspaceUsage(map[string]string{"demo-a": "demo"}, []any{metrics[0], metrics[0]}, now)
	if got["demo"].CPUMillicores != 125 {
		t.Fatal("duplicate metrics double counted")
	}
}

func TestWorkspaceUsageAPI(t *testing.T) {
	for _, scenario := range []string{"available", "metrics-failure", "pods-failure"} {
		t.Run(scenario, func(t *testing.T) {
			pod := func(name, workspace, manager, phase string) object {
				return object{"metadata": object{"name": name, "labels": object{"herdr-hub/workspace": workspace, "app.kubernetes.io/managed-by": manager}}, "status": object{"phase": phase}}
			}
			metricsCalls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer test-token" {
					t.Error("invalid metrics request")
				}
				switch r.URL.Path {
				case "/api/v1/namespaces/test/pods":
					if scenario == "pods-failure" {
						writeError(w, 503, "unavailable")
						return
					}
					terminating := pod("terminating", "demo", "herdr-hub", "Running")
					terminating["metadata"].(object)["deletionTimestamp"] = "now"
					writeJSON(w, 200, object{"items": []object{pod("owned", "demo", "herdr-hub", "Running"), pod("foreign", "hidden", "herdr-hub", "Running"), pod("unmanaged", "demo", "other", "Running"), pod("finished", "demo", "herdr-hub", "Succeeded"), terminating}})
				case "/apis/metrics.k8s.io/v1beta1/namespaces/test/pods":
					metricsCalls++
					if scenario == "metrics-failure" {
						writeError(w, 403, "forbidden")
						return
					}
					metrics := []object{}
					for _, name := range []string{"owned", "foreign", "unmanaged", "finished", "terminating"} {
						metrics = append(metrics, object{"metadata": object{"name": name}, "timestamp": time.Now().UTC().Format(time.RFC3339Nano), "containers": []object{{"name": "herdr", "usage": object{"cpu": "100m", "memory": "1Mi"}}}})
					}
					writeJSON(w, 200, object{"items": metrics})
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
					writeError(w, 404, "missing")
				}
			}))
			defer server.Close()
			tokenFile := filepath.Join(t.TempDir(), "token")
			if err := os.WriteFile(tokenFile, []byte("test-token"), 0600); err != nil {
				t.Fatal(err)
			}
			k := &kubernetes{namespace: "test", base: server.URL, client: server.Client(), tokenFile: tokenFile}
			usage := k.workspaceUsage(context.Background(), []string{"demo"})
			if scenario == "available" {
				if len(usage) != 1 || usage["demo"] == nil || usage["demo"].CPUMillicores != 100 || usage["demo"].MemoryBytes != 1048576 {
					t.Fatalf("ownership filtering failed: %+v", usage)
				}
			} else if len(usage) != 0 {
				t.Fatalf("failed metrics API returned usage: %+v", usage)
			}
			if scenario == "pods-failure" && metricsCalls != 0 {
				t.Fatal("requested metrics without owned pod information")
			}
		})
	}
}
