package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestSaveConfigurationResource(t *testing.T) {
	for _, scenario := range []string{"configmap", "secret", "update", "metadata-only", "conflict", "unmanaged", "unrelated", "deleting", "missing-data", "missing-key"} {
		t.Run(scenario, func(t *testing.T) {
			c := configuration{Name: "tools", Kind: "configmap", Keys: []string{"settings"}}
			if scenario == "secret" {
				c.Kind = "secret"
			}
			var stored object
			if scenario != "configmap" && scenario != "secret" && scenario != "missing-data" {
				stored = object{"apiVersion": "v1", "kind": "ConfigMap", "metadata": object{"name": configurationResource(c.Name), "resourceVersion": "4", "labels": object{"app.kubernetes.io/managed-by": "herdr-hub", "herdr-hub/configuration": c.Name, "custom": "keep"}}, "data": object{"settings": "old"}}
				meta := stored["metadata"].(object)
				switch scenario {
				case "unmanaged":
					meta["labels"].(object)["app.kubernetes.io/managed-by"] = "other"
				case "unrelated":
					meta["labels"].(object)["herdr-hub/configuration"] = "other"
				case "deleting":
					meta["deletionTimestamp"] = "now"
				case "missing-key":
					stored["data"] = object{}
				}
			}
			writes := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case "GET":
					if stored == nil {
						writeError(w, 404, "missing")
					} else {
						writeJSON(w, 200, stored)
					}
				case "POST", "PUT":
					writes++
					var updated object
					_ = json.NewDecoder(r.Body).Decode(&updated)
					if scenario == "conflict" && writes == 1 {
						stored["metadata"].(object)["resourceVersion"] = "5"
						writeError(w, 409, "conflict")
						return
					}
					if stored != nil && (updated["metadata"].(object)["resourceVersion"] != stored["metadata"].(object)["resourceVersion"] || updated["metadata"].(object)["labels"].(object)["custom"] != "keep") {
						t.Error("existing resource metadata lost")
					}
					stored = updated
					writeJSON(w, 200, stored)
				default:
					t.Errorf("unexpected %s", r.Method)
					writeError(w, 500, "unexpected")
				}
			}))
			defer server.Close()
			file := filepath.Join(t.TempDir(), "token")
			_ = os.WriteFile(file, []byte("test-token"), 0600)
			k := &kubernetes{client: server.Client(), base: server.URL, namespace: "test", tokenFile: file}
			data := map[string]string{"settings": "contents\nwith newline"}
			if scenario == "metadata-only" || scenario == "missing-data" || scenario == "missing-key" {
				data = nil
			}
			err := k.saveConfiguration(context.Background(), c, data)
			wantError := scenario == "unmanaged" || scenario == "unrelated" || scenario == "deleting" || scenario == "missing-data" || scenario == "missing-key"
			if (err != nil) != wantError {
				t.Fatalf("unexpected result: %v", err)
			}
			if wantError {
				if writes != 0 {
					t.Fatal("unsafe write")
				}
				return
			}
			want := "contents\nwith newline"
			if scenario == "metadata-only" {
				want = "old"
			}
			if scenario == "secret" {
				want = base64.StdEncoding.EncodeToString([]byte(want))
			}
			if stored["data"].(object)["settings"] != want {
				t.Fatal("contents not saved correctly")
			}
		})
	}
}
