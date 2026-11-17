package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSSHKeyUpdate(t *testing.T) {
	for _, scenario := range []string{"success", "unmanaged secret", "unmanaged deployment", "conflict", "restart failure"} {
		t.Run(scenario, func(t *testing.T) {
			labels := func(unmanaged bool) object {
				if unmanaged {
					return object{}
				}
				return object{"app.kubernetes.io/managed-by": "herdr-hub"}
			}
			secret := object{"metadata": object{"labels": labels(scenario == "unmanaged secret")}, "data": object{"token": "original-token", "authorized_keys": "old-key", "extra": "keep"}}
			deployment := object{"metadata": object{"labels": labels(scenario == "unmanaged deployment")}, "spec": object{"strategy": object{"type": "Recreate"}, "template": object{"metadata": object{"annotations": object{"existing": "keep"}}, "spec": object{"image": "unchanged"}}}}
			puts, conflicts := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				isSecret := strings.Contains(r.URL.Path, "/secrets/")
				if r.Method == "PUT" {
					puts++
					if scenario == "conflict" && conflicts == 0 {
						conflicts++
						writeError(w, 409, "conflict")
						return
					}
					if scenario == "restart failure" && !isSecret {
						writeError(w, 503, "unavailable")
						return
					}
					var obj object
					if err := json.NewDecoder(r.Body).Decode(&obj); err != nil {
						t.Error(err)
					}
					if isSecret {
						secret = obj
					} else {
						deployment = obj
					}
				}
				if isSecret {
					writeJSON(w, 200, secret)
				} else {
					writeJSON(w, 200, deployment)
				}
			}))
			defer server.Close()
			token := filepath.Join(t.TempDir(), "token")
			if err := os.WriteFile(token, []byte("test-token"), 0600); err != nil {
				t.Fatal(err)
			}
			k := &kubernetes{client: server.Client(), base: server.URL, namespace: "test", tokenFile: token}
			err := k.updateSSHKey(context.Background(), "demo", "new-key")
			if strings.HasPrefix(scenario, "unmanaged") {
				if err == nil || puts != 0 {
					t.Fatalf("unmanaged resources mutated: %v, %d writes", err, puts)
				}
				return
			}
			if scenario == "restart failure" {
				if err == nil {
					t.Fatal("restart failure was not reported")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			data := secret["data"].(map[string]any)
			if data["token"] != "original-token" || data["extra"] != "keep" || data["authorized_keys"] != base64.StdEncoding.EncodeToString([]byte("new-key\n")) {
				t.Fatalf("unexpected Secret: %+v", data)
			}
			template := deployment["spec"].(map[string]any)["template"].(map[string]any)
			annotations := template["metadata"].(map[string]any)["annotations"].(map[string]any)
			if annotations["existing"] != "keep" || annotations["herdr-hub/ssh-key-hash"] == nil || template["spec"].(map[string]any)["image"] != "unchanged" {
				t.Fatal("deployment settings were lost or restart missing")
			}
			before := puts
			if err := k.updateSSHKey(context.Background(), "demo", "new-key"); err != nil || puts != before {
				t.Fatalf("retry was not idempotent: %v", err)
			}
		})
	}
}
