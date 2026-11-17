package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestWorkspaceImageUpdate(t *testing.T) {
	for _, scenario := range []string{"upgrade", "unchanged", "conflict", "conflict-exhausted", "missing", "deleting", "unmanaged", "wrong-strategy", "no-container", "api-failure", "pull-policy"} {
		t.Run(scenario, func(t *testing.T) {
			t.Setenv("WORKSPACE_IMAGE_PULL_POLICY", "Always")
			tokenFile := filepath.Join(t.TempDir(), "token")
			if err := os.WriteFile(tokenFile, []byte("test-token"), 0600); err != nil {
				t.Fatal(err)
			}
			k := &kubernetes{namespace: "test", image: "registry/herdr:new", storageSize: "10Gi"}
			encoded, _ := json.Marshal(k.resources(workspace{Name: "demo"}, "token")[2])
			var stored object
			_ = json.Unmarshal(encoded, &stored)
			meta := stored["metadata"].(map[string]any)
			meta["resourceVersion"] = "7"
			spec := stored["spec"].(map[string]any)
			template := spec["template"].(map[string]any)
			template["metadata"].(map[string]any)["annotations"] = object{"custom": "keep"}
			pod := template["spec"].(map[string]any)
			containers := pod["containers"].([]any)
			container := containers[0].(map[string]any)
			container["image"] = "registry/herdr:old"
			pod["containers"] = append(containers, object{"name": "sidecar", "image": "sidecar:keep"})
			switch scenario {
			case "unchanged":
				container["image"] = k.image
			case "pull-policy":
				container["image"] = k.image
				container["imagePullPolicy"] = "IfNotPresent"
			case "deleting":
				meta["deletionTimestamp"] = "2026-10-10T00:00:00Z"
			case "unmanaged":
				meta["labels"].(map[string]any)["app.kubernetes.io/managed-by"] = "other"
			case "wrong-strategy":
				spec["strategy"] = object{"type": "RollingUpdate"}
			case "no-container":
				container["name"] = "other"
			}
			// Compare the entire object to catch changes to volumes, environment,
			// annotations, sidecars or other runtime settings.
			before, _ := json.Marshal(stored)
			var want object
			_ = json.Unmarshal(before, &want)
			wantContainer := want["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
			wantContainer["image"] = k.image
			wantContainer["imagePullPolicy"] = "Always"
			gets, puts := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/apis/apps/v1/namespaces/test/deployments/herdr-demo" || r.Header.Get("Authorization") != "Bearer test-token" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				switch r.Method {
				case "GET":
					gets++
					if scenario == "missing" || scenario == "api-failure" {
						status := 404
						if scenario == "api-failure" {
							status = 503
						}
						writeError(w, status, "unavailable")
						return
					}
					writeJSON(w, 200, stored)
				case "PUT":
					puts++
					var updated object
					_ = json.NewDecoder(r.Body).Decode(&updated)
					if scenario == "conflict-exhausted" || (scenario == "conflict" && puts == 1) {
						meta["resourceVersion"] = "8"
						want["metadata"].(map[string]any)["resourceVersion"] = "8"
						template["metadata"].(map[string]any)["annotations"].(map[string]any)["concurrent"] = "keep"
						want["spec"].(map[string]any)["template"].(map[string]any)["metadata"].(map[string]any)["annotations"].(map[string]any)["concurrent"] = "keep"
						writeError(w, 409, "conflict")
						return
					}
					if !reflect.DeepEqual(updated, want) {
						t.Errorf("image update changed unrelated fields: got %+v, want %+v", updated, want)
					}
					stored = updated
					writeJSON(w, 200, stored)
				default:
					t.Errorf("must not create/delete resources: %s", r.Method)
					writeError(w, 500, "unexpected mutation")
				}
			}))
			defer server.Close()
			k.client, k.base, k.tokenFile = server.Client(), server.URL, tokenFile
			changed, err := k.updateWorkspaceImage(context.Background(), "demo")
			wantError := scenario == "unmanaged" || scenario == "wrong-strategy" || scenario == "no-container" || scenario == "api-failure" || scenario == "conflict-exhausted"
			wantChange := scenario == "upgrade" || scenario == "conflict" || scenario == "pull-policy"
			if (err != nil) != wantError || changed != wantChange {
				t.Fatalf("changed=%v err=%v", changed, err)
			}
			if wantChange {
				previousPuts := puts
				changed, err = k.updateWorkspaceImage(context.Background(), "demo")
				if changed || err != nil || puts != previousPuts {
					t.Fatalf("second pass restarted workspace: changed=%v err=%v", changed, err)
				}
			} else if scenario != "conflict-exhausted" && puts != 0 {
				t.Fatalf("unexpected writes: %d", puts)
			}
			if scenario == "conflict-exhausted" && (gets != 5 || puts != 5) {
				t.Fatalf("retry limit ignored: gets=%d puts=%d", gets, puts)
			}
		})
	}
}
