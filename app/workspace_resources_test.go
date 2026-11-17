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

func TestWorkspaceResourceUpdate(t *testing.T) {
	for _, scenario := range []string{"update", "unchanged", "equivalent", "remove-cpu-limit", "conflict", "conflict-exhausted", "missing", "deleting", "unmanaged", "wrong-strategy", "no-container", "api-failure"} {
		t.Run(scenario, func(t *testing.T) {
			values := workspaceResources{"750m", "3", "2Gi", "6Gi"}
			k := &kubernetes{namespace: "test", image: "herdr:keep"}
			encoded, _ := json.Marshal(k.resources(workspace{Name: "demo", Resources: workspaceResources{"500m", "2", "1Gi", "4Gi"}}, "token")[2])
			var stored object
			_ = json.Unmarshal(encoded, &stored)
			meta := stored["metadata"].(object)
			meta["resourceVersion"] = "7"
			spec := stored["spec"].(object)
			pod := spec["template"].(object)["spec"].(object)
			container := pod["containers"].([]any)[0].(object)
			pod["containers"] = append(pod["containers"].([]any), object{"name": "sidecar", "image": "keep"})
			resources := container["resources"].(object)
			switch scenario {
			case "unchanged", "equivalent":
				values = workspaceResources{"500m", "2", "1Gi", "4Gi"}
				if scenario == "equivalent" {
					values = workspaceResources{"0.5", "2000m", "1024Mi", "4096Mi"}
				}
			case "remove-cpu-limit":
				values.CPULimit = ""
			case "deleting":
				meta["deletionTimestamp"] = "now"
			case "unmanaged":
				meta["labels"].(object)["app.kubernetes.io/managed-by"] = "other"
			case "wrong-strategy":
				spec["strategy"].(object)["type"] = "RollingUpdate"
			case "no-container":
				container["name"] = "other"
			}
			resources["requests"].(object)["ephemeral-storage"] = "2Gi"
			resources["limits"].(object)["example.com/gpu"] = "1"
			resources["claims"] = []any{object{"name": "keep"}}
			before, _ := json.Marshal(stored)
			var want object
			_ = json.Unmarshal(before, &want)
			wantResources := want["spec"].(object)["template"].(object)["spec"].(object)["containers"].([]any)[0].(object)["resources"].(object)
			wantResources["requests"].(object)["cpu"] = values.CPURequest
			wantResources["requests"].(object)["memory"] = values.MemoryRequest
			wantResources["limits"].(object)["memory"] = values.MemoryLimit
			if values.CPULimit == "" {
				delete(wantResources["limits"].(object), "cpu")
			} else {
				wantResources["limits"].(object)["cpu"] = values.CPULimit
			}
			gets, puts := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/apis/apps/v1/namespaces/test/deployments/herdr-demo" {
					t.Errorf("unexpected path %s", r.URL.Path)
				}
				switch r.Method {
				case "GET":
					gets++
					if scenario == "missing" {
						writeError(w, 404, "missing")
						return
					}
					if scenario == "api-failure" {
						writeError(w, 503, "unavailable")
						return
					}
					writeJSON(w, 200, stored)
				case "PUT":
					puts++
					var updated object
					_ = json.NewDecoder(r.Body).Decode(&updated)
					if scenario == "conflict-exhausted" || scenario == "conflict" && puts == 1 {
						meta["resourceVersion"] = "8"
						want["metadata"].(object)["resourceVersion"] = "8"
						pod["nodeSelector"] = object{"concurrent": "keep"}
						want["spec"].(object)["template"].(object)["spec"].(object)["nodeSelector"] = object{"concurrent": "keep"}
						writeError(w, 409, "conflict")
						return
					}
					if !reflect.DeepEqual(updated, want) {
						t.Errorf("resource update changed unrelated fields: got %+v want %+v", updated, want)
					}
					stored = updated
					writeJSON(w, 200, stored)
				default:
					t.Errorf("must not create/delete resources: %s", r.Method)
					writeError(w, 500, "unexpected request")
				}
			}))
			defer server.Close()
			k.client, k.base = server.Client(), server.URL
			k.tokenFile = filepath.Join(t.TempDir(), "token")
			if err := os.WriteFile(k.tokenFile, []byte("test-token"), 0600); err != nil {
				t.Fatal(err)
			}
			changed, err := k.updateWorkspaceResources(context.Background(), "demo", values)
			wantChanged := scenario == "update" || scenario == "remove-cpu-limit" || scenario == "conflict"
			wantError := scenario == "deleting" || scenario == "unmanaged" || scenario == "wrong-strategy" || scenario == "no-container" || scenario == "api-failure" || scenario == "conflict-exhausted"
			if changed != wantChanged || (err != nil) != wantError {
				t.Fatalf("changed=%v err=%v", changed, err)
			}
			if wantChanged {
				previous := puts
				changed, err = k.updateWorkspaceResources(context.Background(), "demo", values)
				if changed || err != nil || puts != previous {
					t.Fatalf("same settings restarted workspace: changed=%v err=%v", changed, err)
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

func exerciseWorkspaceResources(t *testing.T, a *application, objects map[string]object, request func(string, string, string) *httptest.ResponseRecorder, key string) {
	t.Helper()
	ctx := context.Background()
	expect := func(w *httptest.ResponseRecorder, status int) {
		t.Helper()
		if w.Code != status {
			t.Fatalf("resources HTTP %d want %d: %s", w.Code, status, w.Body.String())
		}
	}
	original, err := a.load(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	values := workspaceResources{"750m", "3", "2Gi", "6Gi"}
	body, _ := json.Marshal(values)
	path := "/apis/apps/v1/namespaces/test/deployments/herdr-demo"
	pvcBefore, _ := json.Marshal(objects["/api/v1/namespaces/test/persistentvolumeclaims/herdr-demo"])
	secretBefore, _ := json.Marshal(objects["/api/v1/namespaces/test/secrets/herdr-demo"])
	expect(request("PATCH", "workspaces/missing/resources", string(body)), 404)
	for _, invalid := range []string{`{}`, `{"cpu_request":"4","cpu_limit":"3","memory_request":"1Gi","memory_limit":"4Gi"}`, `{"cpu_request":"500m","memory_request":"4Gi","memory_limit":"1Gi"}`, `{"unknown":true}`} {
		expect(request("PATCH", "workspaces/demo/resources", invalid), 400)
	}
	for _, status := range []string{"deleting", "undeploying"} {
		if _, err = a.db.Exec(ctx, `UPDATE workspaces SET status=$1 WHERE name='demo'`, status); err != nil {
			t.Fatal(err)
		}
		expect(request("PATCH", "workspaces/demo/resources", string(body)), 409)
	}
	if _, err = a.db.Exec(ctx, `UPDATE workspaces SET status=$1 WHERE name='demo'`, original.Status); err != nil {
		t.Fatal(err)
	}
	labels := objects[path]["metadata"].(object)["labels"].(object)
	labels["app.kubernetes.io/managed-by"] = "other"
	expect(request("PATCH", "workspaces/demo/resources", string(body)), 502)
	unchanged, err := a.load(ctx, "demo")
	if err != nil || unchanged.Resources != original.Resources {
		t.Fatal("failed update changed saved settings", err)
	}
	labels["app.kubernetes.io/managed-by"] = "herdr-hub"
	expect(request("PATCH", "workspaces/demo/resources", string(body)), 200)
	updated, err := a.load(ctx, "demo")
	if err != nil || updated.Resources != values {
		t.Fatal("per-workspace resources not saved", err)
	}
	pod := objects[path]["spec"].(object)["template"].(object)["spec"].(object)
	if !reflect.DeepEqual(pod["containers"].([]any)[0].(object)["resources"], values.kubernetesResources()) {
		t.Fatal("live workspace resource settings not applied")
	}
	expect(request("POST", "workspaces/demo/undeploy", ""), 200)
	values.CPULimit, values.MemoryLimit = "", "8Gi"
	body, _ = json.Marshal(values)
	expect(request("PATCH", "workspaces/demo/resources", string(body)), 200)
	updated, err = a.load(ctx, "demo")
	if err != nil || updated.Status != "undeployed" || updated.Resources != values || objects[path] != nil {
		t.Fatal("resource update restarted undeployed workspace or lost settings", err)
	}
	expect(request("POST", "workspaces/demo/deploy", ""), 201)
	pod = objects[path]["spec"].(object)["template"].(object)["spec"].(object)
	if !reflect.DeepEqual(pod["containers"].([]any)[0].(object)["resources"], values.kubernetesResources()) {
		t.Fatal("redeployment lost resource override")
	}
	pvcAfter, _ := json.Marshal(objects["/api/v1/namespaces/test/persistentvolumeclaims/herdr-demo"])
	secretAfter, _ := json.Marshal(objects["/api/v1/namespaces/test/secrets/herdr-demo"])
	if string(pvcBefore) != string(pvcAfter) || string(secretBefore) != string(secretAfter) {
		t.Fatal("resource edits changed home or credentials")
	}
	body, _ = json.Marshal(original.Resources)
	expect(request("PATCH", "workspaces/demo/resources", string(body)), 200)
	// Creation can override defaults without affecting other workspaces.
	createBody, _ := json.Marshal(workspace{Name: "custom-resources", SSHKey: key, Resources: values})
	expect(request("POST", "workspaces", string(createBody)), 201)
	custom, err := a.load(ctx, "custom-resources")
	if err != nil || custom.Resources != values {
		t.Fatal("creation ignored workspace resource overrides", err)
	}
	expect(request("DELETE", "workspaces/custom-resources", ""), 204)
	delete(objects, "/api/v1/namespaces/test/persistentvolumeclaims/herdr-custom-resources")
}
