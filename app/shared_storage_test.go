package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Part of the real-PostgreSQL lifecycle test: checks permissions, group access,
// multi-workspace sharing and live-pod safeguards against premature revocation.
func exerciseSharedStorage(t *testing.T, a *application, objects map[string]object, admin func(string, string, string) *httptest.ResponseRecorder, session func(*http.Cookie, string, string, string) *httptest.ResponseRecorder, alice, bob *http.Cookie, key string) {
	t.Helper()
	previousClass := a.kube.storageClass
	a.kube.storageClass = "nfs"
	defer func() { a.kube.storageClass = previousClass }()
	defer a.db.Exec(context.Background(), `DELETE FROM workspaces WHERE name='team-workspace'; DELETE FROM shared_volumes WHERE name='shared-test'; DELETE FROM hub_groups WHERE name='shared-team'`)
	expect := func(w *httptest.ResponseRecorder, status int) {
		t.Helper()
		if w.Code != status {
			t.Fatalf("shared storage HTTP %d want %d: %s", w.Code, status, w.Body.String())
		}
	}
	req := func(c *http.Cookie, m, p, b string) *httptest.ResponseRecorder { return session(c, m, p, b) }
	expect(req(alice, "POST", "volumes", `{"name":"forbidden-class","storage_size":"1Gi","storage_class":"other"}`), 403)
	expect(req(alice, "POST", "volumes", `{"name":"too-large","storage_size":"101Gi"}`), 403)
	expect(req(alice, "POST", "volumes", `{"name":"shared-test","storage_size":"2Gi","storage_class":"nfs"}`), 201)
	limits, err := a.securityLimits(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Exec(context.Background(), `UPDATE hub_resource_defaults SET security_limits=$1 WHERE id=1`, limits)
	limited := limits
	limited.UserVolumes = 1
	if _, err = a.db.Exec(context.Background(), `UPDATE hub_resource_defaults SET security_limits=$1 WHERE id=1`, limited); err != nil {
		t.Fatal(err)
	}
	expect(req(alice, "POST", "volumes", `{"name":"too-many","storage_size":"1Gi"}`), 403)
	limited.VolumeGi = 3
	limited.UserStorageGi = 3
	if _, err = a.db.Exec(context.Background(), `UPDATE hub_resource_defaults SET security_limits=$1 WHERE id=1`, limited); err != nil {
		t.Fatal(err)
	}
	expect(req(alice, "PATCH", "volumes/shared-test/size", `{"storage_size":"3Gi"}`), 403)
	if _, err = a.db.Exec(context.Background(), `UPDATE hub_resource_defaults SET security_limits=$1 WHERE id=1`, limits); err != nil {
		t.Fatal(err)
	}
	expect(req(alice, "POST", "volumes", `{"name":"shared-test","storage_size":"2Gi","storage_class":"nfs"}`), 201)
	expect(req(alice, "POST", "volumes", `{"name":"shared-test","storage_size":"3Gi","storage_class":"nfs"}`), 409)
	expect(req(bob, "POST", "volumes", `{"name":"evil","owner":"test-user","storage_size":"1Gi"}`), 403)
	expect(req(alice, "POST", "volumes", `{"name":"bad","storage_size":"0Gi"}`), 400)
	exerciseSharedVolumeResize(t, a, objects, admin, session, alice, bob)
	exerciseMixedCaseIdentities(t, a, admin, session, alice)
	expect(req(alice, "POST", "groups", `{"name":"shared-team"}`), 201)
	for _, search := range []struct{ path, body string }{
		{"directory/users?q=TEST-USER", `["test-user"]`},
		{"directory/users?q=other", `["test-other"]`},
		{"directory/groups?q=team", `["shared-team"]`},
		{"directory/users?q=does-not-exist", `[]`},
		{"directory/users?q=", `[]`},
	} {
		response := req(bob, "GET", search.path, "")
		expect(response, 200)
		if strings.TrimSpace(response.Body.String()) != search.body {
			t.Fatalf("unexpected directory result: %s", response.Body.String())
		}
	}
	expect(req(bob, "GET", "directory/users?q=%25", ""), 400)
	expect(req(bob, "GET", "directory/users?q="+strings.Repeat("a", 41), ""), 400)
	expect(req(bob, "GET", "directory/passwords?q=test", ""), 404)
	expect(req(bob, "GET", "users", ""), 403)
	anonymous := httptest.NewRecorder()
	a.api(anonymous, httptest.NewRequest("GET", "/api/directory/users?q=test", nil))
	expect(anonymous, 401)
	expect(req(alice, "PUT", "groups/shared-team/members", `{"members":["test-other"]}`), 200)
	expect(req(bob, "PUT", "groups/shared-team/members", `{"members":[]}`), 403)
	expect(req(alice, "PUT", "volumes/shared-test/shares", `{"users":[],"groups":["missing"]}`), 400)
	expect(req(alice, "PUT", "volumes/shared-test/shares", `{"users":[],"groups":["shared-team"]}`), 200)
	list := req(bob, "GET", "volumes", "")
	expect(list, 200)
	if !strings.Contains(list.Body.String(), "shared-test") {
		t.Fatal("group member cannot see shared PVC")
	}
	body, _ := json.Marshal(workspace{Name: "team-workspace", Owner: "test-other", SSHKey: key})
	expect(admin("POST", "workspaces", string(body)), 201)
	expect(req(bob, "PUT", "workspaces/demo/volumes", `{"volumes":["shared-test"]}`), 404)
	expect(req(bob, "PUT", "volumes/shared-test/shares", `{"users":[],"groups":[]}`), 403)
	expect(req(alice, "PUT", "workspaces/demo/volumes", `{"volumes":["shared-test","shared-test"]}`), 400)
	expect(req(alice, "PUT", "workspaces/demo/volumes", `{"volumes":["missing"]}`), 403)
	deploymentPath := "/apis/apps/v1/namespaces/test/deployments/herdr-demo"
	savedDeployment := objects[deploymentPath]
	delete(objects, deploymentPath)
	expect(req(alice, "PUT", "workspaces/demo/volumes", `{"volumes":["shared-test"]}`), 502)
	if objects[deploymentPath] != nil {
		t.Fatal("mount update recreated a missing runtime")
	}
	expect(req(alice, "DELETE", "volumes/shared-test", ""), 409)
	objects[deploymentPath] = savedDeployment
	expect(req(alice, "PUT", "workspaces/demo/volumes", `{"volumes":["shared-test"]}`), 200)
	expect(req(bob, "PUT", "workspaces/team-workspace/volumes", `{"volumes":["shared-test"]}`), 200)
	// Transferring a mounted volume must retain both owners' access and all mounts.
	mountedBeforeTransfer, _ := json.Marshal(objects)
	expect(admin("PATCH", "volumes/shared-test/owner", `{"owner":"test-other"}`), 200)
	expect(admin("PATCH", "volumes/shared-test/owner", `{"owner":"test-user"}`), 200)
	mountedAfterTransfer, _ := json.Marshal(objects)
	if string(mountedBeforeTransfer) != string(mountedAfterTransfer) {
		t.Fatal("mounted volume transfer changed Kubernetes resources")
	}
	expect(req(alice, "POST", "workspaces/demo/undeploy", ""), 200)
	paused, err := a.load(context.Background(), "demo")
	if err != nil || !reflect.DeepEqual(paused.Volumes, []string{"shared-test"}) {
		t.Fatal("undeploy lost shared-volume assignments", err)
	}
	expect(req(alice, "DELETE", "volumes/shared-test", ""), 409)
	expect(req(alice, "POST", "workspaces/demo/deploy", ""), 201)
	// Expansion remains possible while several workspaces use the shared PVC.
	beforeMounts, _ := json.Marshal([]object{objects[deploymentPath], objects["/apis/apps/v1/namespaces/test/deployments/herdr-team-workspace"]})
	expect(admin("PATCH", "volumes/shared-test/size", `{"storage_size":"6Gi"}`), 200)
	afterMounts, _ := json.Marshal([]object{objects[deploymentPath], objects["/apis/apps/v1/namespaces/test/deployments/herdr-team-workspace"]})
	if string(beforeMounts) != string(afterMounts) {
		t.Fatal("shared volume expansion changed workspace deployments")
	}
	expect(req(bob, "PATCH", "volumes/shared-test/size", `{"storage_size":"7Gi"}`), 403)
	for _, name := range []string{"demo", "team-workspace"} {
		d := objects["/apis/apps/v1/namespaces/test/deployments/"+resourceName(name)]
		pod := d["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
		if !podUsesClaim(object{"spec": pod}, volumeClaim("shared-test")) || !podUsesClaim(object{"spec": pod}, resourceName(name)) {
			t.Fatal("shared mount missing or home replaced")
		}
		c := pod["containers"].([]any)[0].(map[string]any)
		found := false
		for _, raw := range c["volumeMounts"].([]any) {
			m := raw.(map[string]any)
			found = found || m["mountPath"] == "/mnt/shared/shared-test"
		}
		if !found {
			t.Fatal("mount path incorrect")
		}
	}
	expect(req(alice, "DELETE", "volumes/shared-test", ""), 409)
	expect(req(alice, "PUT", "volumes/shared-test/shares", `{"users":[],"groups":[]}`), 409)
	expect(req(alice, "PUT", "groups/shared-team/members", `{"members":[]}`), 409)
	expect(req(alice, "DELETE", "groups/shared-team", ""), 409)
	expect(admin("PATCH", "workspaces/demo/owner", `{"owner":"test-other"}`), 409)
	expect(req(alice, "PUT", "workspaces/demo/volumes", `{"volumes":[]}`), 200)
	expect(req(bob, "PUT", "workspaces/team-workspace/volumes", `{"volumes":[]}`), 200)
	// Terminating pods still hold mount access after the Deployment is updated.
	podPath := "/api/v1/namespaces/test/pods"
	objects[podPath] = object{"items": []object{{"metadata": object{"labels": object{"herdr-hub/workspace": "demo"}, "deletionTimestamp": "now"}, "spec": object{"volumes": []object{{"name": "shared-shared-test", "persistentVolumeClaim": object{"claimName": volumeClaim("shared-test")}}}}}}}
	expect(req(alice, "PUT", "volumes/shared-test/shares", `{"users":[],"groups":[]}`), 409)
	expect(req(alice, "DELETE", "volumes/shared-test", ""), 409)
	expect(admin("PATCH", "workspaces/demo/owner", `{"owner":"test-other"}`), 409)
	delete(objects, podPath)
	expect(req(alice, "PUT", "volumes/shared-test/shares", `{"users":["test-other"],"groups":[]}`), 200)
	expect(req(alice, "PUT", "groups/shared-team/members", `{"members":[]}`), 200)
	expect(req(bob, "PUT", "workspaces/team-workspace/volumes", `{"volumes":["shared-test"]}`), 200)
	expect(req(bob, "DELETE", "workspaces/team-workspace", ""), 204)
	if objects["/api/v1/namespaces/test/persistentvolumeclaims/"+volumeClaim("shared-test")] == nil {
		t.Fatal("runtime deletion erased shared data")
	}
	delete(objects, "/api/v1/namespaces/test/persistentvolumeclaims/herdr-team-workspace")
	expect(req(alice, "PUT", "volumes/shared-test/shares", `{"users":[],"groups":[]}`), 200)
	expect(req(bob, "GET", "volumes", ""), 200)
	allowed, err := a.volumeAccess(context.Background(), "shared-test", "test-other")
	if err != nil || allowed {
		t.Fatal("revoked user retains access", err)
	}
	expect(req(alice, "DELETE", "groups/shared-team", ""), 204)
	expect(req(alice, "PATCH", "volumes/shared-test/owner", `{"owner":"test-other"}`), 403)
	expect(admin("PATCH", "volumes/shared-test/owner", `{"owner":"missing-user"}`), 400)
	expect(admin("PATCH", "volumes/shared-test/owner", `{"owner":""}`), 400)
	expect(admin("PATCH", "volumes/shared-test/owner", `{"owner":"test-other","users":[]}`), 400)
	beforeTransfer, _ := json.Marshal(objects)
	expect(admin("PATCH", "volumes/shared-test/owner", `{"owner":"test-other"}`), 200)
	afterTransfer, _ := json.Marshal(objects)
	if string(beforeTransfer) != string(afterTransfer) {
		t.Fatal("owner transfer changed Kubernetes resources")
	}
	var transferredOwner string
	if err := a.db.QueryRow(context.Background(), `SELECT owner FROM shared_volumes WHERE name='shared-test'`).Scan(&transferredOwner); err != nil || transferredOwner != "test-other" {
		t.Fatal("owner transfer not saved", err)
	}
	for _, username := range []string{"test-user", "test-other"} {
		if allowed, err := a.volumeAccess(context.Background(), "shared-test", username); err != nil || !allowed {
			t.Fatal("transfer lost owner access", username, err)
		}
	}
	volumeList := admin("GET", "volumes", "")
	expect(volumeList, 200)
	var listed []sharedVolume
	if err := json.Unmarshal(volumeList.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	foundAccess := false
	for _, volume := range listed {
		if volume.Name == "shared-test" {
			foundAccess = reflect.DeepEqual(volume.AccessibleOwners, []string{"test-other", "test-user"})
		}
	}
	if !foundAccess {
		t.Fatal("admin catalog missing workspace-owner access metadata")
	}
	expect(req(alice, "DELETE", "volumes/shared-test", ""), 403)
	expect(admin("PATCH", "volumes/shared-test/owner", `{"owner":"test-user"}`), 200)
	expect(req(alice, "DELETE", "volumes/shared-test", ""), 204)
}

func exerciseSharedVolumeResize(t *testing.T, a *application, objects map[string]object, admin func(string, string, string) *httptest.ResponseRecorder, session func(*http.Cookie, string, string, string) *httptest.ResponseRecorder, owner, other *http.Cookie) {
	t.Helper()
	resize := func(cookie *http.Cookie, body string, status int) {
		t.Helper()
		w := session(cookie, "PATCH", "volumes/shared-test/size", body)
		if w.Code != status {
			t.Fatalf("resize HTTP %d want %d: %s", w.Code, status, w.Body.String())
		}
	}
	resize(other, `{"storage_size":"3Gi"}`, 403)
	for _, body := range []string{`{`, `{}`, `{"storage_size":"0Gi"}`, `{"storage_size":"1.5Gi"}`, `{"storage_size":"3Gi","owner":"test-other"}`} {
		resize(owner, body, 400)
	}
	path := "/api/v1/namespaces/test/persistentvolumeclaims/" + volumeClaim("shared-test")
	pvc := objects[path]
	meta := pvc["metadata"].(map[string]any)
	labels := meta["labels"].(map[string]any)
	for _, label := range []string{"app.kubernetes.io/managed-by", "herdr-hub/shared-volume"} {
		original := labels[label]
		labels[label] = "other"
		resize(owner, `{"storage_size":"3Gi"}`, 409)
		labels[label] = original
	}
	meta["deletionTimestamp"] = "now"
	resize(owner, `{"storage_size":"3Gi"}`, 409)
	delete(meta, "deletionTimestamp")
	delete(objects, path)
	resize(owner, `{"storage_size":"3Gi"}`, 502)
	if objects[path] != nil {
		t.Fatal("resize recreated a missing shared PVC")
	}
	objects[path] = pvc
	// Failed Kubernetes updates retain a conservative budget reservation.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			writeJSON(w, 200, pvc)
		} else {
			writeError(w, 403, "StorageClass does not allow expansion")
		}
	}))
	base := a.kube.base
	a.kube.base = server.URL
	resize(owner, `{"storage_size":"3Gi"}`, 502)
	a.kube.base = base
	server.Close()
	var saved string
	if err := a.db.QueryRow(context.Background(), `SELECT storage_size FROM shared_volumes WHERE name='shared-test'`).Scan(&saved); err != nil || saved != "3Gi" {
		t.Fatalf("failed resize lost budget reservation: %s, %v", saved, err)
	}
	before, _ := json.Marshal(pvc)
	resize(owner, `{"storage_size":"3Gi"}`, 200)
	var want object
	_ = json.Unmarshal(before, &want)
	requests := want["spec"].(map[string]any)["resources"].(map[string]any)["requests"].(map[string]any)
	requests["storage"] = "3Gi"
	if !reflect.DeepEqual(objects[path], want) {
		t.Fatal("expansion changed unrelated PVC fields")
	}
	resize(owner, `{"storage_size":"3Gi"}`, 200)
	resize(owner, `{"storage_size":"2Gi"}`, 400)
	// Simulate an expansion whose database update failed, then safely retry it.
	objects[path]["spec"].(map[string]any)["resources"].(map[string]any)["requests"].(map[string]any)["storage"] = "5Gi"
	resize(owner, `{"storage_size":"4Gi"}`, 400)
	resize(owner, `{"storage_size":"5Gi"}`, 200)
	if err := a.db.QueryRow(context.Background(), `SELECT storage_size FROM shared_volumes WHERE name='shared-test'`).Scan(&saved); err != nil || saved != "5Gi" {
		t.Fatalf("resize retry did not save database size: %s, %v", saved, err)
	}
	w := admin("PATCH", "volumes/missing/size", `{"storage_size":"5Gi"}`)
	if w.Code != 404 {
		t.Fatalf("missing volume resize HTTP %d want 404", w.Code)
	}
}

func TestGeneratedSharedMounts(t *testing.T) {
	k := &kubernetes{storageSize: "1Gi"}
	for _, o := range k.resources(workspace{Name: "demo", Volumes: []string{"data"}}, "token") {
		if o["kind"] != "Deployment" {
			continue
		}
		b, _ := json.Marshal(o)
		var d object
		_ = json.Unmarshal(b, &d)
		pod := d["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
		if !podUsesClaim(object{"spec": pod}, volumeClaim("data")) || !podUsesClaim(object{"spec": pod}, "herdr-demo") {
			t.Fatal("retry should generate shared mounts and retain home")
		}
		addSharedMounts(pod, []string{})
		if podUsesClaim(object{"spec": pod}, volumeClaim("data")) || !podUsesClaim(object{"spec": pod}, "herdr-demo") {
			t.Fatal("detach should preserve home")
		}
	}
}

func TestSharedMountUpdateGuards(t *testing.T) {
	for _, scenario := range []string{"conflict", "missing", "unmanaged", "deleting", "wrong-strategy"} {
		t.Run(scenario, func(t *testing.T) {
			k := &kubernetes{namespace: "test", image: "herdr:test", storageSize: "1Gi"}
			encoded, _ := json.Marshal(k.resources(workspace{Name: "demo"}, "token")[2])
			var stored object
			_ = json.Unmarshal(encoded, &stored)
			meta := stored["metadata"].(map[string]any)
			meta["resourceVersion"] = "1"
			spec := stored["spec"].(map[string]any)
			template := spec["template"].(map[string]any)
			template["metadata"].(map[string]any)["annotations"] = object{"custom": "keep"}
			pod := template["spec"].(map[string]any)
			pod["containers"] = append(pod["containers"].([]any), object{"name": "sidecar", "image": "keep"})
			switch scenario {
			case "unmanaged":
				meta["labels"].(map[string]any)["app.kubernetes.io/managed-by"] = "other"
			case "deleting":
				meta["deletionTimestamp"] = "now"
			case "wrong-strategy":
				spec["strategy"] = object{"type": "RollingUpdate"}
			}
			puts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					if scenario == "missing" {
						writeError(w, 404, "missing")
						return
					}
					writeJSON(w, 200, stored)
					return
				}
				if r.Method != "PUT" {
					t.Errorf("unexpected mutation: %s", r.Method)
					writeError(w, 500, "unexpected")
					return
				}
				puts++
				if puts == 1 {
					meta["resourceVersion"] = "2"
					template["metadata"].(map[string]any)["annotations"].(map[string]any)["concurrent"] = "keep"
					writeError(w, 409, "conflict")
					return
				}
				var updated object
				_ = json.NewDecoder(r.Body).Decode(&updated)
				data, _ := json.Marshal(stored)
				var want object
				_ = json.Unmarshal(data, &want)
				wantPod := want["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
				wantPod["volumes"] = append(wantPod["volumes"].([]any), map[string]any{"name": "shared-data", "persistentVolumeClaim": map[string]any{"claimName": "herdr-shared-data"}})
				container := wantPod["containers"].([]any)[0].(map[string]any)
				container["volumeMounts"] = append(container["volumeMounts"].([]any), map[string]any{"name": "shared-data", "mountPath": "/mnt/shared/data"})
				if !reflect.DeepEqual(updated, want) {
					t.Error("mount update lost unrelated fields or concurrent edits")
				}
				writeJSON(w, 200, updated)
			}))
			defer server.Close()
			tokenFile := filepath.Join(t.TempDir(), "token")
			if err := os.WriteFile(tokenFile, []byte("test"), 0600); err != nil {
				t.Fatal(err)
			}
			k.client, k.base, k.tokenFile = server.Client(), server.URL, tokenFile
			err := k.updateSharedMounts(context.Background(), "demo", []string{"data"})
			if scenario == "conflict" {
				if err != nil || puts != 2 {
					t.Fatalf("conflict retry failed: %v, puts=%d", err, puts)
				}
			} else if err == nil || puts != 0 {
				t.Fatalf("unsafe runtime mutated: %v, puts=%d", err, puts)
			}
		})
	}
}
