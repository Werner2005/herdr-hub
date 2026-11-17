package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// Exercised by TestWorkspaceLifecycle against a dedicated PostgreSQL database.
func exerciseConfigurations(t *testing.T, a *application, stored map[string]object, request func(*http.Cookie, string, string, string) *httptest.ResponseRecorder, admin, owner, other *http.Cookie, failRollout *bool) {
	t.Helper()
	ctx := context.Background()
	expect := func(cookie *http.Cookie, method, path, body string, status int) *httptest.ResponseRecorder {
		t.Helper()
		w := request(cookie, method, path, body)
		if w.Code != status {
			t.Fatalf("configuration %s %s HTTP %d want %d: %s", method, path, w.Code, status, w.Body.String())
		}
		return w
	}
	deploymentPath := "/apis/apps/v1/namespaces/test/deployments/herdr-demo"
	secretPath := "/api/v1/namespaces/test/secrets/herdr-config-cfg-private"
	podsPath := "/api/v1/namespaces/test/pods"
	pod := func(name string) object {
		return stored["/apis/apps/v1/namespaces/test/deployments/herdr-"+name]["spec"].(object)["template"].(object)["spec"].(object)
	}
	global := `{"name":"cfg-global","kind":"configmap","global":true,"data":{"settings":"global contents"},"mapping":{"files":[{"key":"settings","path":"/home/dev/.config/team/settings"}],"env":[{"key":"settings","name":"TEAM_SETTINGS"}]}}`
	expect(owner, "POST", "configurations", global, 403)
	w := expect(admin, "POST", "configurations", global, 201)
	if strings.Contains(w.Body.String(), "global contents") {
		t.Fatal("configuration API returned values")
	}
	if !podUsesConfiguration(pod("demo"), configurationResource("cfg-global")) {
		t.Fatal("global did not reach existing workspace")
	}
	// Future workspaces automatically receive global mappings.
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	public, _ := ssh.NewPublicKey(pub)
	newWorkspace, _ := json.Marshal(workspace{Name: "cfg-new", Owner: "test-other", SSHKey: string(ssh.MarshalAuthorizedKey(public))})
	expect(admin, "POST", "workspaces", string(newWorkspace), 201)
	if !podUsesConfiguration(pod("cfg-new"), configurationResource("cfg-global")) {
		t.Fatal("global did not reach new workspace")
	}
	private := `{"name":"cfg-private","kind":"secret","data":{"token":"secret-DO-NOT-RETURN"}}`
	expect(owner, "POST", "configurations", `{"name":"cfg-forged","owner":"test-other","kind":"secret","data":{"token":"x"}}`, 403)
	// Existing resources may not be overwritten just because their names match.
	brokenPath := "/api/v1/namespaces/test/secrets/herdr-config-cfg-broken"
	stored[brokenPath] = object{"metadata": object{"labels": object{"app.kubernetes.io/managed-by": "other"}}, "data": object{"token": "keep"}}
	expect(owner, "POST", "configurations", `{"name":"cfg-broken","kind":"secret","data":{"token":"replacement"}}`, 502)
	if stored[brokenPath]["data"].(object)["token"] != "keep" {
		t.Fatal("unmanaged resource overwritten")
	}
	expect(owner, "PUT", "configurations/cfg-broken", `{}`, 400)
	delete(stored, brokenPath)
	expect(owner, "PUT", "configurations/cfg-broken", `{"data":{"token":"replacement"}}`, 200)
	expect(owner, "DELETE", "configurations/cfg-broken", "", 204)
	// Bearer-token admins can create entries when explicitly assigning an account.
	bearer := httptest.NewRequest("POST", "/api/configurations", strings.NewReader(`{"name":"cfg-bearer","owner":"test-admin","kind":"configmap","data":{"settings":"x"}}`))
	bearer.Header.Set("Authorization", "Bearer "+a.token)
	bearerResponse := httptest.NewRecorder()
	a.api(bearerResponse, bearer)
	if bearerResponse.Code != 201 {
		t.Fatal("bearer creation failed", bearerResponse.Body.String())
	}
	expect(admin, "DELETE", "configurations/cfg-bearer", "", 204)
	expect(owner, "POST", "configurations", private, 201)
	expect(other, "POST", "configurations", private, 409)
	expect(other, "PUT", "configurations/cfg-private", private, 404)
	expect(other, "DELETE", "configurations/cfg-private", "", 404)
	visible := expect(other, "GET", "configurations", "", 200)
	if strings.Contains(visible.Body.String(), "cfg-private") || !strings.Contains(visible.Body.String(), "cfg-global") {
		t.Fatal("configuration catalog access leak")
	}
	for _, cookie := range []*http.Cookie{owner, admin} {
		visible = expect(cookie, "GET", "configurations", "", 200)
		if strings.Contains(visible.Body.String(), "secret-DO-NOT-RETURN") || strings.Contains(visible.Body.String(), base64.StdEncoding.EncodeToString([]byte("secret-DO-NOT-RETURN"))) {
			t.Fatal("Secret values disclosed")
		}
	}
	if stored[secretPath]["data"].(object)["token"] != base64.StdEncoding.EncodeToString([]byte("secret-DO-NOT-RETURN")) {
		t.Fatal("Secret not stored as Kubernetes data")
	}
	mapping := `{"configurations":[{"configuration":"cfg-private","files":[{"key":"token","path":"/mnt/secrets/tool/token"}],"env":[{"key":"token","name":"TOOL_TOKEN"}]}]}`
	var initial struct {
		Configurations []configurationMapping `json:"configurations"`
	}
	if err := json.Unmarshal([]byte(mapping), &initial); err != nil {
		t.Fatal(err)
	}
	// Initial attachments use exactly the same owner/key/collision rules as edits,
	// and invalid inputs must not leave a workspace or any Kubernetes resources.
	invalidCases := []workspace{
		{Name: "cfg-invalid", Owner: "test-other", Configurations: initial.Configurations},
		{Name: "cfg-invalid", Owner: "test-user", Configurations: []configurationMapping{{Configuration: "missing", Env: []configurationEnv{{Key: "token", Name: "TOOL_TOKEN"}}}}},
		{Name: "cfg-invalid", Owner: "test-user", Configurations: append(append([]configurationMapping{}, initial.Configurations...), initial.Configurations...)},
		{Name: "cfg-invalid", Owner: "test-user", Configurations: []configurationMapping{{Configuration: "cfg-private", Env: []configurationEnv{{Key: "token", Name: "HERDR_WEB_TOKEN"}}}}},
		{Name: "cfg-invalid", Owner: "test-user", Configurations: []configurationMapping{{Configuration: "cfg-private", Files: []configurationFile{{Key: "token", Path: "/home/dev/.config/team/settings"}}}}},
		{Name: "cfg-invalid", Owner: "test-admin", Configurations: []configurationMapping{{Configuration: "cfg-global", Env: []configurationEnv{{Key: "settings", Name: "OTHER_SETTINGS"}}}}},
	}
	for _, invalid := range invalidCases {
		invalid.SSHKey = string(ssh.MarshalAuthorizedKey(public))
		body, _ := json.Marshal(invalid)
		before := len(stored)
		expect(admin, "POST", "workspaces", string(body), 400)
		var exists bool
		if err := a.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workspaces WHERE name='cfg-invalid')`).Scan(&exists); err != nil || exists || len(stored) != before {
			t.Fatal("invalid attachment left workspace state", err)
		}
	}
	attached := workspace{Name: "cfg-attached", Owner: "test-user", SSHKey: string(ssh.MarshalAuthorizedKey(public)), Configurations: initial.Configurations}
	body, _ := json.Marshal(attached)
	// Inject a real insert failure after the workspace insert; the transaction
	// must roll both records back before provisioning starts.
	if _, err := a.db.Exec(ctx, `ALTER TABLE workspace_configurations ADD CONSTRAINT config_creation_atomic_test CHECK (workspace <> 'cfg-attached')`); err != nil {
		t.Fatal(err)
	}
	defer a.db.Exec(ctx, `ALTER TABLE workspace_configurations DROP CONSTRAINT IF EXISTS config_creation_atomic_test`)
	expect(admin, "POST", "workspaces", string(body), 500)
	if _, err := a.db.Exec(ctx, `ALTER TABLE workspace_configurations DROP CONSTRAINT config_creation_atomic_test`); err != nil {
		t.Fatal(err)
	}
	var exists bool
	if err := a.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workspaces WHERE name='cfg-attached')`).Scan(&exists); err != nil || exists {
		t.Fatal("failed mapping insert did not roll back workspace", err)
	}
	// A failed Kubernetes provision retains committed mappings. Its initial pod
	// already contains both private and global refs, and retry keeps credentials.
	attachedRoute := "/apis/gateway.networking.k8s.io/v1/namespaces/test/httproutes/herdr-cfg-attached"
	stored[attachedRoute] = object{"metadata": object{"labels": object{"app.kubernetes.io/managed-by": "other"}}}
	created := expect(admin, "POST", "workspaces", string(body), 502)
	var createdWorkspace workspace
	if err := json.Unmarshal(created.Body.Bytes(), &createdWorkspace); err != nil || len(createdWorkspace.Configurations) != 1 {
		t.Fatal("creation response lost attachments", err)
	}
	if !podUsesConfiguration(pod("cfg-attached"), configurationResource("cfg-private")) || !podUsesConfiguration(pod("cfg-attached"), configurationResource("cfg-global")) {
		t.Fatal("first start lacked initial attachments")
	}
	attachedToken := stored["/api/v1/namespaces/test/secrets/herdr-cfg-attached"]["data"].(object)["token"]
	delete(stored, attachedRoute)
	expect(admin, "POST", "workspaces/cfg-attached/retry", "", 201)
	if stored["/api/v1/namespaces/test/secrets/herdr-cfg-attached"]["data"].(object)["token"] != attachedToken {
		t.Fatal("creation retry rotated credentials")
	}
	expect(admin, "POST", "workspaces", string(body), 409)
	itemWithAttachments, err := a.load(ctx, "cfg-attached")
	if err != nil || len(itemWithAttachments.Configurations) != 1 {
		t.Fatal("creation retry lost persisted attachments", err)
	}
	expect(admin, "DELETE", "workspaces/cfg-attached", "", 204)
	delete(stored, "/api/v1/namespaces/test/persistentvolumeclaims/herdr-cfg-attached")
	expect(other, "PUT", "workspaces/demo/configurations", mapping, 404)
	expect(admin, "PUT", "workspaces/cfg-new/configurations", mapping, 400)
	expect(owner, "PUT", "workspaces/demo/configurations", mapping, 200)
	if !podUsesConfiguration(pod("demo"), configurationResource("cfg-private")) {
		t.Fatal("private Secret not mapped")
	}
	expect(owner, "PUT", "configurations/cfg-private", `{"data":{"other":"missing referenced key"}}`, 409)
	expect(owner, "DELETE", "configurations/cfg-private", "", 409)
	expect(admin, "DELETE", "users/test-user", "", 409)
	expect(admin, "PATCH", "workspaces/demo/owner", `{"owner":"test-other"}`, 409)
	conflict := `{"name":"cfg-conflict","kind":"configmap","global":true,"data":{"value":"x"},"mapping":{"env":[{"key":"value","name":"TOOL_TOKEN"}]}}`
	expect(admin, "POST", "configurations", conflict, 409)
	oldRevision := stored[deploymentPath]["spec"].(object)["template"].(object)["metadata"].(object)["annotations"].(object)["herdr-hub/configuration-version"]
	expect(owner, "PUT", "configurations/cfg-private", `{"data":{"token":"new-secret"}}`, 200)
	newRevision := stored[deploymentPath]["spec"].(object)["template"].(object)["metadata"].(object)["annotations"].(object)["herdr-hub/configuration-version"]
	if oldRevision == newRevision {
		t.Fatal("secret value replacement did not trigger rollout")
	}
	// Assignments persist while undeployed, and the latest global/private mappings
	// are included in the create-only redeployment (not reconciled by recreation).
	expect(owner, "POST", "workspaces/demo/undeploy", "", 200)
	item, err := a.load(ctx, "demo")
	if err != nil || len(item.Configurations) != 1 {
		t.Fatal("undeploy lost configuration assignments", err)
	}
	expect(owner, "PUT", "workspaces/demo/configurations", mapping, 200)
	expect(owner, "POST", "workspaces/demo/deploy", "", 201)
	if !podUsesConfiguration(pod("demo"), configurationResource("cfg-private")) {
		t.Fatal("redeploy lost configuration")
	}
	// A failed detach keeps desired state and is repaired without re-creating the
	// Deployment. The old Deployment/pods prevent premature resource deletion.
	*failRollout = true
	expect(owner, "PUT", "workspaces/demo/configurations", `{"configurations":[]}`, 502)
	expect(owner, "DELETE", "configurations/cfg-private", "", 409)
	*failRollout = false
	if err = a.reconcileConfigurationsLocked(ctx); err != nil {
		t.Fatal("failed detach was not repaired", err)
	}
	if podUsesConfiguration(pod("demo"), configurationResource("cfg-private")) {
		t.Fatal("reconcile kept detached Secret")
	}
	savedPods := stored[podsPath]
	stored[podsPath] = object{"items": []object{{"spec": object{"containers": []object{{"env": []object{{"name": "TOOL_TOKEN", "valueFrom": object{"secretKeyRef": object{"name": configurationResource("cfg-private"), "key": "token"}}}}}}}}}}
	expect(owner, "DELETE", "configurations/cfg-private", "", 409)
	if savedPods == nil {
		delete(stored, podsPath)
	} else {
		stored[podsPath] = savedPods
	}
	expect(owner, "DELETE", "configurations/cfg-private", "", 204)
	if stored[secretPath] != nil {
		t.Fatal("Secret retained after deletion")
	}
	expect(admin, "DELETE", "configurations/cfg-global", "", 409)
	expect(admin, "PUT", "configurations/cfg-global", `{"global":false}`, 200)
	if podUsesConfiguration(pod("demo"), configurationResource("cfg-global")) || podUsesConfiguration(pod("cfg-new"), configurationResource("cfg-global")) {
		t.Fatal("global removal did not reach all workspaces")
	}
	expect(admin, "DELETE", "configurations/cfg-global", "", 204)
	expect(admin, "DELETE", "workspaces/cfg-new", "", 204)
	delete(stored, "/api/v1/namespaces/test/persistentvolumeclaims/herdr-cfg-new")
}
