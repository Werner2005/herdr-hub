package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/ssh"
)

// Run against a dedicated test database with TEST_DATABASE_URL set.
func TestWorkspaceLifecycle(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	db, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = initializeDatabase(ctx, db); err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(ctx, "TRUNCATE workspaces, workspace_configurations, workspace_ports, workspace_egress, hub_egress_defaults")
	if err != nil {
		t.Fatal(err)
	}
	objects := map[string]object{}
	gatewayPath := "/apis/gateway.networking.k8s.io/v1/namespaces/test/gateways/public"
	objects[gatewayPath] = object{"metadata": object{"name": "public"}, "spec": object{"listeners": []object{{"name": "https", "port": 443, "protocol": "HTTPS", "tls": object{"mode": "Terminate", "certificateRefs": []object{{"name": "gateway-tls"}}}}}}}
	failRoute := true
	failUndeploy := false
	failConfigurationRollout := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "PUT":
			if failConfigurationRollout && strings.Contains(r.URL.Path, "/deployments/") {
				writeError(w, 503, "injected configuration rollout failure")
				return
			}
			var obj object
			_ = json.NewDecoder(r.Body).Decode(&obj)
			objects[r.URL.Path] = obj
			writeJSON(w, 200, obj)
		case "POST":
			if strings.HasSuffix(r.URL.Path, "/httproutes") && failRoute {
				writeError(w, 503, "injected route failure")
				return
			}
			var obj object
			_ = json.NewDecoder(r.Body).Decode(&obj)
			name := obj["metadata"].(map[string]any)["name"].(string)
			if obj["kind"] == "Deployment" {
				obj["status"] = object{"readyReplicas": 1}
			}
			objects[r.URL.Path+"/"+name] = obj
			writeJSON(w, 201, obj)
		case "GET":
			if strings.HasSuffix(r.URL.Path, "/pods") {
				if list := objects[r.URL.Path]; list != nil {
					writeJSON(w, 200, list)
				} else {
					writeJSON(w, 200, object{"items": []object{}})
				}
				return
			}
			obj, ok := objects[r.URL.Path]
			if !ok {
				writeError(w, 404, "missing")
				return
			}
			writeJSON(w, 200, obj)
		case "DELETE":
			if failUndeploy && strings.HasSuffix(r.URL.Path, "/deployments/herdr-demo") {
				writeError(w, 503, "injected shutdown failure")
				return
			}
			delete(objects, r.URL.Path)
			writeJSON(w, 200, object{"status": "Success"})
		}
	}))
	defer server.Close()
	tokenFile := filepath.Join(t.TempDir(), "token")
	_ = os.WriteFile(tokenFile, []byte("kube-token"), 0600)
	k := &kubernetes{client: server.Client(), base: server.URL, namespace: "test", tokenFile: tokenFile, image: "herdr:test", domain: "apps.test", gateway: "public", webListener: "https", storageSize: "1Gi", listeners: map[string]int{"ssh-2201": 2201}}
	a := &application{db: db, kube: k, token: "hub-token"}
	request := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/api/"+path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer hub-token")
		w := httptest.NewRecorder()
		a.api(w, r)
		return w
	}
	expect := func(w *httptest.ResponseRecorder, status int) {
		t.Helper()
		if w.Code != status {
			t.Fatalf("HTTP %d, want %d: %s", w.Code, status, w.Body.String())
		}
	}
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	sshPub, _ := ssh.NewPublicKey(pub)
	input := workspace{Name: "demo", SSHKey: string(ssh.MarshalAuthorizedKey(sshPub)), SSHListener: "ssh-2201"}
	body, _ := json.Marshal(input)
	expect(request("POST", "workspaces", string(body)), 502)
	if len(objects) != 5 {
		t.Fatal("partial creation should leave four retryable resources")
	}
	secretPath := "/api/v1/namespaces/test/secrets/herdr-demo"
	originalToken := objects[secretPath]["data"].(map[string]any)["token"]
	failRoute = false
	expect(request("POST", "workspaces/demo/retry", ""), 201)
	if len(objects) != 7 {
		t.Fatal("retry did not finish provisioning")
	}
	if objects[secretPath]["data"].(map[string]any)["token"] != originalToken {
		t.Fatal("retry rotated credentials")
	}
	expect(request("POST", "workspaces", string(body)), 409)
	initialDefaults, err := a.resourceDefaults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Exec(ctx, `UPDATE hub_resource_defaults SET resources=$1 WHERE id=1`, initialDefaults)
	expect(request("PATCH", "config", `{"resource_defaults":{"cpu_request":"3","cpu_limit":"2","memory_request":"1Gi","memory_limit":"4Gi"}}`), 400)
	expect(request("PATCH", "config", `{"resource_defaults":{"cpu_request":"750m","cpu_limit":"3","memory_request":"2Gi","memory_limit":"6Gi"}}`), 200)
	if err = initializeDatabase(ctx, db); err != nil {
		t.Fatal(err)
	}
	savedDefaults, err := a.resourceDefaults(ctx)
	if err != nil || savedDefaults.CPURequest != "750m" {
		t.Fatal("startup reset saved resource defaults", err)
	}
	input.Name = "other"
	body, _ = json.Marshal(input)
	expect(request("POST", "workspaces", string(body)), 201)
	other, err := a.load(ctx, "other")
	if err != nil || other.SSHPort != 2202 {
		t.Fatalf("second workspace did not get a unique SSH port: %+v %v", other, err)
	}
	if other.Resources != savedDefaults {
		t.Fatalf("new workspace did not inherit defaults: %+v", other.Resources)
	}
	otherPod := objects["/apis/apps/v1/namespaces/test/deployments/herdr-other"]["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	otherResources := otherPod["containers"].([]any)[0].(map[string]any)["resources"].(map[string]any)
	if otherResources["requests"].(map[string]any)["cpu"] != "750m" || otherResources["limits"].(map[string]any)["cpu"] != "3" {
		t.Fatal("Deployment did not use saved resource requests and limits")
	}
	demo, err := a.load(ctx, "demo")
	if err != nil || demo.Resources != initialDefaults {
		t.Fatal("changing defaults changed existing workspace settings", err)
	}
	defaultsBody, _ := json.Marshal(map[string]any{"resource_defaults": initialDefaults})
	expect(request("PATCH", "config", string(defaultsBody)), 200)
	expect(request("DELETE", "workspaces/other", ""), 204)
	delete(objects, "/api/v1/namespaces/test/persistentvolumeclaims/herdr-other")
	podsPath := "/api/v1/namespaces/test/pods"
	metricsPath := "/apis/metrics.k8s.io/v1beta1/namespaces/test/pods"
	objects[podsPath] = object{"items": []object{{"metadata": object{"name": "demo-pod", "labels": object{"app.kubernetes.io/managed-by": "herdr-hub", "herdr-hub/workspace": "demo"}}, "status": object{"phase": "Running"}}}}
	objects[metricsPath] = object{"items": []object{{"metadata": object{"name": "demo-pod"}, "timestamp": time.Now().UTC().Format(time.RFC3339Nano), "containers": []object{{"name": "herdr", "usage": object{"cpu": "38765432n", "memory": "192Mi"}}}}}}
	list := request("GET", "workspaces", "")
	expect(list, 200)
	var items []workspace
	_ = json.Unmarshal(list.Body.Bytes(), &items)
	if len(items) != 1 || items[0].Status != "running" || items[0].SSHPort != 2201 {
		t.Fatalf("unexpected list: %s", list.Body.String())
	}
	if items[0].Usage == nil || items[0].Usage.CPUMillicores != 38.765432 || items[0].Usage.MemoryBytes != 192*1048576 {
		t.Fatalf("workspace list did not include usage: %s", list.Body.String())
	}
	delete(objects, metricsPath)
	list = request("GET", "workspaces", "")
	expect(list, 200)
	_ = json.Unmarshal(list.Body.Bytes(), &items)
	if items[0].Usage != nil {
		t.Fatal("missing metrics should be unavailable, not fabricated zero usage")
	}
	delete(objects, podsPath)
	expect(request("GET", "workspaces/demo/token", ""), 200)
	expect(request("PATCH", "workspaces/demo/size", `{"storage_size":"5Gi"}`), 200)
	expect(request("PATCH", "workspaces/demo/size", `{"storage_size":"2Gi"}`), 400)
	pvc := objects["/api/v1/namespaces/test/persistentvolumeclaims/herdr-demo"]
	if pvc["spec"].(map[string]any)["resources"].(map[string]any)["requests"].(map[string]any)["storage"] != "5Gi" {
		t.Fatal("PVC was not expanded")
	}
	_, _ = db.Exec(ctx, `DELETE FROM hub_users WHERE username IN ('test-user','test-other','test-admin')`)
	// Upgrade an existing runtime without replacing its home or credentials.
	k.image = "herdr:upgraded"
	if err = a.reconcileWorkspaceImages(ctx); err != nil {
		t.Fatal(err)
	}
	deployment := objects["/apis/apps/v1/namespaces/test/deployments/herdr-demo"]
	container := deployment["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
	if container["image"] != k.image || objects[secretPath]["data"].(map[string]any)["token"] != originalToken {
		t.Fatal("image update failed or changed credentials")
	}
	pvc = objects["/api/v1/namespaces/test/persistentvolumeclaims/herdr-demo"]
	if pvc == nil || pvc["spec"].(map[string]any)["resources"].(map[string]any)["requests"].(map[string]any)["storage"] != "5Gi" {
		t.Fatal("image update changed the retained home")
	}
	if _, err = db.Exec(ctx, `UPDATE workspaces SET status='deleting' WHERE name='demo'`); err != nil {
		t.Fatal(err)
	}
	k.image = "herdr:must-not-apply"
	if err = a.reconcileWorkspaceImages(ctx); err != nil || container["image"] == k.image {
		t.Fatal("image worker must skip deleting workspaces", err)
	}
	if _, err = db.Exec(ctx, `UPDATE workspaces SET status='provisioning' WHERE name='demo'`); err != nil {
		t.Fatal(err)
	}
	k.image = "herdr:upgraded"
	defer db.Exec(ctx, `DELETE FROM hub_users WHERE username IN ('test-user','test-other','test-admin')`)
	if err = a.bootstrapAdmin(ctx, "test-admin", "test-password-123"); err != nil {
		t.Fatal(err)
	}
	if err = a.bootstrapAdmin(ctx, "test-admin", "different-password-123"); err != nil {
		t.Fatal(err)
	}
	expect(request("POST", "users", `{"username":"test-user","password":"test-password-123","role":"user"}`), 201)
	expect(request("POST", "users", `{"username":"test-other","password":"test-password-123","role":"user"}`), 201)
	expect(request("PATCH", "workspaces/demo/owner", `{"owner":"test-user"}`), 200)
	transferred, err := a.load(ctx, "demo")
	if err != nil || transferred.Status != "undeployed" || transferred.SSHKey != "" || objects[secretPath]["data"].(map[string]any)["token"] == originalToken {
		t.Fatal("ownership transfer did not stop runtime and revoke credentials", err)
	}
	originalToken = objects[secretPath]["data"].(map[string]any)["token"]
	expect(request("POST", "workspaces/demo/deploy", ""), 201)
	login := func(username, password string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/login", strings.NewReader(`{"username":"`+username+`","password":"`+password+`"}`))
		w := httptest.NewRecorder()
		a.api(w, r)
		return w
	}
	expect(login("test-user", "wrong-password"), 401)
	adminLoggedIn := login("test-admin", "test-password-123")
	expect(adminLoggedIn, 200)
	loggedIn := login("test-user", "test-password-123")
	expect(loggedIn, 200)
	userRequest := func(cookie *http.Cookie, method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/api/"+path, strings.NewReader(body))
		r.AddCookie(cookie)
		w := httptest.NewRecorder()
		a.api(w, r)
		return w
	}
	cookie := loggedIn.Result().Cookies()[0]
	if cookie.Name != "__Host-herdr_session" || !cookie.Secure || !cookie.HttpOnly || cookie.Path != "/" || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatal("login did not issue a protected production cookie")
	}
	exerciseConfigurations(t, a, objects, userRequest, adminLoggedIn.Result().Cookies()[0], cookie, login("test-other", "test-password-123").Result().Cookies()[0], &failConfigurationRollout)
	exerciseEgress(t, a, objects, userRequest, adminLoggedIn.Result().Cookies()[0], cookie, login("test-other", "test-password-123").Result().Cookies()[0])
	expect(userRequest(cookie, "POST", "workspaces/demo/token", ""), 403)
	expect(userRequest(cookie, "PATCH", "workspaces/demo/resources", `{"cpu_request":"500m","cpu_limit":"","memory_request":"1Gi","memory_limit":"4Gi"}`), 403)
	expect(userRequest(cookie, "PATCH", "workspaces/demo/resources", `{"cpu_request":"500m","cpu_limit":"5","memory_request":"1Gi","memory_limit":"4Gi"}`), 403)
	expect(userRequest(cookie, "PATCH", "config", string(defaultsBody)), 403)
	ownerResources := workspaceResources{"750m", "3", "2Gi", "6Gi"}
	ownerResourceBody, _ := json.Marshal(ownerResources)
	expect(userRequest(cookie, "PATCH", "workspaces/demo/resources", string(ownerResourceBody)), 200)
	ownerUpdated, err := a.load(ctx, "demo")
	if err != nil || ownerUpdated.Resources != ownerResources {
		t.Fatal("owner could not update workspace resources", err)
	}
	initialResourceBody, _ := json.Marshal(initialDefaults)
	expect(userRequest(cookie, "PATCH", "workspaces/demo/resources", string(initialResourceBody)), 200)
	expect(userRequest(cookie, "PATCH", "workspaces/demo/resources", `{}`), 400)
	expect(userRequest(cookie, "PATCH", "workspaces/missing/resources", string(ownerResourceBody)), 404)
	expect(userRequest(cookie, "GET", "workspaces/demo/token", ""), 200)
	newPub, _, _ := ed25519.GenerateKey(rand.Reader)
	newSSH, _ := ssh.NewPublicKey(newPub)
	newKey := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(newSSH)))
	keyBody, _ := json.Marshal(map[string]string{"ssh_public_key": newKey})
	expect(userRequest(cookie, "PATCH", "workspaces/demo/ssh-key", `{"ssh_public_key":"invalid"}`), 400)
	expect(userRequest(cookie, "PATCH", "workspaces/demo/ssh-key", string(keyBody)), 200)
	updated, err := a.load(ctx, "demo")
	if err != nil || updated.SSHKey != newKey {
		t.Fatal("SSH key was not saved")
	}
	secretData := objects[secretPath]["data"].(map[string]any)
	if secretData["token"] != originalToken || secretData["authorized_keys"] != base64.StdEncoding.EncodeToString([]byte(newKey+"\n")) {
		t.Fatal("SSH key update lost token or did not update Secret")
	}
	keyParts := strings.Fields(newKey)
	keyBody, _ = json.Marshal(map[string]string{"ssh_public_key": input.SSHKey + "\n" + keyParts[0] + "\t" + keyParts[1][:20] + " \n" + keyParts[1][20:] + " laptop"})
	expect(userRequest(cookie, "PATCH", "workspaces/demo/ssh-key", string(keyBody)), 200)
	wantKeys := strings.TrimSpace(input.SSHKey) + "\n" + newKey + " laptop"
	updated, err = a.load(ctx, "demo")
	if err != nil || updated.SSHKey != wantKeys || objects[secretPath]["data"].(map[string]any)["authorized_keys"] != base64.StdEncoding.EncodeToString([]byte(wantKeys+"\n")) {
		t.Fatal("multiple SSH keys were not normalized and applied")
	}
	keyBody, _ = json.Marshal(map[string]string{"ssh_public_key": newKey + " laptop"})
	expect(userRequest(cookie, "PATCH", "workspaces/demo/ssh-key", string(keyBody)), 200)
	updated, err = a.load(ctx, "demo")
	if err != nil || updated.SSHKey != newKey+" laptop" || objects[secretPath]["data"].(map[string]any)["token"] != originalToken {
		t.Fatal("removing one SSH key did not preserve the remaining key and web token")
	}
	expect(userRequest(cookie, "PATCH", "workspaces/demo/ssh-key", `{"ssh_public_key":""}`), 200)
	updated, err = a.load(ctx, "demo")
	if err != nil || updated.SSHKey != "" || objects[secretPath]["data"].(map[string]any)["authorized_keys"] != base64.StdEncoding.EncodeToString([]byte("\n")) {
		t.Fatal("last SSH key was not removed")
	}
	expect(userRequest(cookie, "PATCH", "workspaces/demo/ssh-key", `{}`), 400)
	expect(userRequest(cookie, "PATCH", "workspaces/demo/ssh-key", `{"ssh_public_key":null}`), 400)
	expect(userRequest(cookie, "POST", "workspaces", string(body)), 403)
	expect(userRequest(cookie, "POST", "workspaces/demo/retry", ""), 403)
	expect(userRequest(cookie, "PATCH", "workspaces/demo/size", `{"storage_size":"10Gi"}`), 403)
	expect(userRequest(cookie, "GET", "users", ""), 403)
	otherLogin := login("test-other", "test-password-123")
	expect(otherLogin, 200)
	otherCookie := otherLogin.Result().Cookies()[0]
	expect(userRequest(otherCookie, "PATCH", "workspaces/demo/resources", string(ownerResourceBody)), 404)
	expect(userRequest(otherCookie, "POST", "workspaces/demo/undeploy", ""), 404)
	expect(userRequest(otherCookie, "POST", "workspaces/demo/deploy", ""), 404)
	expect(userRequest(cookie, "POST", "workspaces/missing/undeploy", ""), 404)
	deploymentPath := "/apis/apps/v1/namespaces/test/deployments/herdr-demo"
	failUndeploy = true
	expect(userRequest(cookie, "POST", "workspaces/demo/undeploy", ""), 502)
	expect(userRequest(cookie, "POST", "workspaces/demo/deploy", ""), 409)
	k.image = "herdr:paused-update"
	if err = a.reconcileWorkspaceImages(ctx); err != nil || objects[deploymentPath]["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["image"] == k.image {
		t.Fatal("image worker changed an undeploying workspace", err)
	}
	failUndeploy = false
	expect(userRequest(cookie, "POST", "workspaces/demo/undeploy", ""), 200)
	expect(userRequest(cookie, "POST", "workspaces/demo/undeploy", ""), 200)
	paused, err := a.load(ctx, "demo")
	if err != nil || paused.Status != "undeployed" || paused.Owner != "test-user" || paused.SSHPort != 2201 || paused.StorageSize != "5Gi" {
		t.Fatalf("undeploy lost workspace settings: %+v %v", paused, err)
	}
	if len(objects) != 3 || objects[deploymentPath] != nil || objects[secretPath]["data"].(map[string]any)["token"] != originalToken {
		t.Fatal("undeploy must retain only Gateway, home PVC and credentials")
	}
	pausedList := userRequest(cookie, "GET", "workspaces", "")
	expect(pausedList, 200)
	if err = json.Unmarshal(pausedList.Body.Bytes(), &items); err != nil || len(items) != 1 || items[0].Status != "undeployed" {
		t.Fatalf("undeployed workspace not visible: %s", pausedList.Body.String())
	}
	expect(userRequest(cookie, "PATCH", "workspaces/demo/resources", string(ownerResourceBody)), 200)
	ownerUpdated, err = a.load(ctx, "demo")
	if err != nil || ownerUpdated.Resources != ownerResources || ownerUpdated.Status != "undeployed" || objects[deploymentPath] != nil {
		t.Fatal("owner resource edit restarted undeployed workspace or lost settings", err)
	}
	expect(userRequest(cookie, "PATCH", "workspaces/demo/resources", string(initialResourceBody)), 200)
	expect(userRequest(cookie, "PATCH", "workspaces/demo/ssh-key", string(keyBody)), 409)
	expect(userRequest(cookie, "PUT", "workspaces/demo/volumes", `{"volumes":[]}`), 409)
	if err = a.reconcileWorkspaceImages(ctx); err != nil || objects[deploymentPath] != nil {
		t.Fatal("image worker restarted an undeployed workspace", err)
	}
	expect(request("PATCH", "config", `{"resource_defaults":{"cpu_request":"750m","cpu_limit":"3","memory_request":"2Gi","memory_limit":"6Gi"}}`), 200)
	expect(userRequest(cookie, "POST", "workspaces/demo/deploy", ""), 201)
	redeployed, err := a.load(ctx, "demo")
	if err != nil || redeployed.Resources != initialDefaults {
		t.Fatal("redeploy lost saved resource settings", err)
	}
	redeployedPod := objects[deploymentPath]["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	redeployedResources := redeployedPod["containers"].([]any)[0].(map[string]any)["resources"].(map[string]any)
	if redeployedResources["requests"].(map[string]any)["cpu"] != initialDefaults.CPURequest || redeployedResources["limits"].(map[string]any)["memory"] != initialDefaults.MemoryLimit {
		t.Fatal("redeployment applied changed defaults instead of saved resources")
	}
	expect(request("PATCH", "config", string(defaultsBody)), 200)
	if objects[deploymentPath] == nil || objects[secretPath]["data"].(map[string]any)["token"] != originalToken {
		t.Fatal("deploy failed to restore runtime with original credentials")
	}
	expect(request("POST", "workspaces/demo/undeploy", ""), 200)
	expect(request("POST", "workspaces/demo/deploy", ""), 201)
	exerciseWorkspacePorts(t, a, objects, request, userRequest, cookie, otherCookie)
	exerciseWorkspaceResources(t, a, objects, request, newKey)
	exerciseSharedStorage(t, a, objects, request, userRequest, cookie, otherCookie, newKey)
	expect(userRequest(otherCookie, "GET", "workspaces/demo/token", ""), 404)
	expect(userRequest(otherCookie, "PATCH", "workspaces/demo/ssh-key", string(keyBody)), 404)
	expect(userRequest(otherCookie, "DELETE", "workspaces/demo", ""), 404)
	otherList := userRequest(otherCookie, "GET", "workspaces", "")
	expect(otherList, 200)
	if strings.TrimSpace(otherList.Body.String()) != "[]" {
		t.Fatal("user can see another user's workspace")
	}
	expect(userRequest(cookie, "GET", "users/test-other", ""), 403)
	expect(userRequest(cookie, "PATCH", "users/test-other", `{"role":"admin"}`), 403)
	expect(userRequest(cookie, "DELETE", "users/test-other", ""), 403)
	expect(request("PATCH", "users/test-other", `{"role":"invalid"}`), 400)
	expect(request("PATCH", "users/test-other", `{"password":"short"}`), 400)
	expect(request("PATCH", "users/missing", `{"role":"user"}`), 404)
	expect(request("DELETE", "users/test-user", ""), 409)
	adminLogin := login("test-admin", "test-password-123")
	expect(adminLogin, 200)
	adminCookie := adminLogin.Result().Cookies()[0]
	expect(userRequest(adminCookie, "PATCH", "users/test-admin", `{"role":"user"}`), 409)
	expect(userRequest(adminCookie, "DELETE", "users/test-admin", ""), 409)
	var adminCount int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM hub_users WHERE role='admin'`).Scan(&adminCount); err != nil {
		t.Fatal(err)
	}
	if adminCount == 1 {
		expect(request("PATCH", "users/test-admin", `{"role":"user"}`), 409)
		expect(request("DELETE", "users/test-admin", ""), 409)
	}
	expect(userRequest(adminCookie, "PATCH", "users/test-admin", `{"password":"new-admin-password-123"}`), 200)
	expect(userRequest(adminCookie, "GET", "me", ""), 200)
	expect(login("test-admin", "test-password-123"), 401)
	expect(login("test-admin", "new-admin-password-123"), 200)
	expect(request("PATCH", "users/test-other", `{"role":"admin"}`), 200)
	expect(userRequest(otherCookie, "GET", "users", ""), 200)
	expect(request("PATCH", "users/test-other", `{"role":"user","password":"reset-password-123"}`), 200)
	expect(userRequest(otherCookie, "GET", "me", ""), 401)
	expect(login("test-other", "test-password-123"), 401)
	resetLogin := login("test-other", "reset-password-123")
	expect(resetLogin, 200)
	expect(request("DELETE", "users/test-other", ""), 204)
	expect(userRequest(resetLogin.Result().Cookies()[0], "GET", "me", ""), 401)
	expect(userRequest(cookie, "POST", "logout", ""), 204)
	expect(userRequest(cookie, "GET", "me", ""), 401)
	originalData := objects[secretPath]["data"]
	delete(objects[secretPath], "data")
	expect(request("GET", "workspaces/demo/token", ""), 502)
	objects[secretPath]["data"] = originalData
	expect(request("POST", "workspaces/demo/ports", `{"name":"cleanup","protocol":"HTTP","internal_port":3000}`), 201)
	expect(request("DELETE", "workspaces/demo", ""), 204)
	if len(objects) != 2 || objects["/api/v1/namespaces/test/persistentvolumeclaims/herdr-demo"] == nil {
		t.Fatal("delete must retain only home PVC")
	}
	list = request("GET", "workspaces", "")
	expect(list, 200)
	if strings.TrimSpace(list.Body.String()) != "[]" {
		t.Fatal("database record survived delete")
	}
	expect(request("DELETE", "workspaces/demo", ""), 404)
	input.Name = "demo"
	body, _ = json.Marshal(input)
	expect(request("POST", "workspaces", string(body)), 201)
	if len(objects) != 7 {
		t.Fatal("could not reattach retained home")
	}
	reattached, err := a.load(ctx, "demo")
	if err != nil || reattached.StorageSize != "5Gi" {
		t.Fatal("reattached home lost its expanded size")
	}
	expect(request("PATCH", "workspaces/demo/owner", `{"owner":"test-user"}`), 200)
	newLogin := login("test-user", "test-password-123")
	expect(newLogin, 200)
	beforeRotation := objects[secretPath]["data"].(map[string]any)["token"]
	expect(request("POST", "workspaces/demo/token", ""), 200)
	if objects[secretPath]["data"].(map[string]any)["token"] == beforeRotation || objects[deploymentPath] != nil {
		t.Fatal("explicit rotation did not revoke token or stop runtime")
	}
	expect(userRequest(newLogin.Result().Cookies()[0], "DELETE", "workspaces/demo", ""), 204)
}
