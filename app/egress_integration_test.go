package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Runs inside TestWorkspaceLifecycle with real PostgreSQL and mocked Kubernetes.
func exerciseEgress(t *testing.T, a *application, stored map[string]object, request func(*http.Cookie, string, string, string) *httptest.ResponseRecorder, admin, owner, other *http.Cookie) {
	t.Helper()
	ctx := context.Background()
	expect := func(cookie *http.Cookie, method, path, body string, status int) *httptest.ResponseRecorder {
		t.Helper()
		w := request(cookie, method, path, body)
		if w.Code != status {
			t.Fatalf("egress %s %s HTTP %d want %d: %s", method, path, w.Code, status, w.Body.String())
		}
		return w
	}
	rules := `{"rules":[{"name":"litellm","cidr":"192.0.2.10","protocol":"TCP","port":443},{"name":"proxy","namespace":"traefik","pod_selector":{"app.kubernetes.io/name":"traefik"},"protocol":"TCP","port":8443}]}`
	expect(owner, "GET", "egress-defaults", "", 200)
	expect(owner, "PUT", "egress-defaults", rules, 403)
	expect(other, "GET", "workspaces/demo/egress", "", 404)
	expect(other, "PUT", "workspaces/demo/egress", rules, 404)
	expect(owner, "PUT", "workspaces/missing/egress", rules, 404)
	for _, body := range []string{`{}`, `{"rules":null}`, `{"rules":[{"name":"oops","protocol":"TCP","port":443}]}`, `{"workspace":"other","rules":[]}`, `{"rules":[{"name":"oops","cidr":"1.2.3.4","protocol":"TCP","port":443,"podSelector":{}}]}`} {
		expect(owner, "PUT", "workspaces/demo/egress", body, 400)
	}
	before, _ := json.Marshal(stored)
	policyPath := "/apis/networking.k8s.io/v1/namespaces/test/networkpolicies/" + egressPolicyName("demo")
	// An ownership failure must retain desired state and surface a retryable error.
	stored[policyPath] = object{"metadata": object{"labels": object{"app.kubernetes.io/managed-by": "other"}}}
	expect(owner, "PUT", "workspaces/demo/egress", rules, 502)
	saved := expect(owner, "GET", "workspaces/demo/egress", "", 200)
	var settings egressSettings
	_ = json.Unmarshal(saved.Body.Bytes(), &settings)
	if settings.Status != "error" || settings.Error == "" || len(settings.Rules) != 2 {
		t.Fatal("failed reconciliation lost rules or error", saved.Body.String())
	}
	delete(stored, policyPath)
	if err := a.syncEgress(ctx, "demo"); err != nil {
		t.Fatal("retry did not recover", err)
	}
	expect(owner, "PUT", "workspaces/demo/egress", rules, 200)
	if stored[policyPath] == nil {
		t.Fatal("workspace policy not created")
	}
	expect(admin, "PATCH", "workspaces/demo/owner", `{"owner":"test-other"}`, 409)
	// Failed removals retain an empty desired rule set for reconciliation, and
	// ownership cannot transfer while old permissions still exist.
	stored[policyPath]["metadata"].(object)["deletionTimestamp"] = "now"
	expect(owner, "PUT", "workspaces/demo/egress", `{"rules":[]}`, 502)
	settings, err := a.egressSettings(ctx, "demo")
	if err != nil || len(settings.Rules) != 0 || settings.Status != "error" {
		t.Fatal("failed removal discarded retry state", err)
	}
	expect(admin, "PATCH", "workspaces/demo/owner", `{"owner":"test-other"}`, 502)
	delete(stored[policyPath]["metadata"].(object), "deletionTimestamp")
	if err = a.syncEgress(ctx, "demo"); err != nil || stored[policyPath] != nil {
		t.Fatal("failed removal did not recover", err)
	}
	expect(owner, "PUT", "workspaces/demo/egress", rules, 200)
	// Defaults match all workspaces, with no changes to runtime resources.
	expect(admin, "PUT", "egress-defaults", rules, 200)
	defaultPath := "/apis/networking.k8s.io/v1/namespaces/test/networkpolicies/" + egressPolicyName("")
	globalPolicy := stored[defaultPath]
	if globalPolicy == nil {
		t.Fatal("default policy not created")
	}
	if err := initializeDatabase(ctx, a.db); err != nil {
		t.Fatal(err)
	}
	settings, err = a.egressSettings(ctx, "")
	if err != nil || len(settings.Rules) != 2 {
		t.Fatal("startup reset default rules", err)
	}
	// Global selection automatically covers future workspaces. A private rule
	// on an undeployed workspace must not cause any runtime to be created.
	if _, err = a.db.Exec(ctx, `INSERT INTO workspaces(name,ssh_public_key,owner,status) VALUES('egress-only','','test-user','undeployed')`); err != nil {
		t.Fatal(err)
	}
	expect(owner, "PUT", "workspaces/egress-only/egress", rules, 200)
	if stored["/apis/apps/v1/namespaces/test/deployments/herdr-egress-only"] != nil {
		t.Fatal("rule edit created an undeployed runtime")
	}
	// Deleting an entry cleans its private permissions before cascading settings.
	expect(owner, "DELETE", "workspaces/egress-only", "", 204)
	if stored["/apis/networking.k8s.io/v1/namespaces/test/networkpolicies/"+egressPolicyName("egress-only")] != nil || stored[defaultPath] == nil {
		t.Fatal("workspace deletion left permissions behind or removed defaults")
	}
	// Persistence across undeploy and future deploy does not recreate a runtime.
	_, err = a.db.Exec(ctx, `UPDATE workspaces SET status='undeployed' WHERE name='demo'`)
	if err != nil {
		t.Fatal(err)
	}
	expect(owner, "PUT", "workspaces/demo/egress", rules, 200)
	_, err = a.db.Exec(ctx, `UPDATE workspaces SET status='provisioning' WHERE name='demo'`)
	if err != nil {
		t.Fatal(err)
	}
	// Scope is enforced for admins too: global rules are not overwritten by edits.
	expect(admin, "PUT", "workspaces/demo/egress", `{"rules":[]}`, 200)
	if stored[policyPath] != nil || stored[defaultPath] == nil {
		t.Fatal("workspace removal affected defaults or left permission behind")
	}
	expect(owner, "PUT", "egress-defaults", `{"rules":[]}`, 403)
	expect(admin, "PUT", "egress-defaults", `{"rules":[]}`, 200)
	if stored[defaultPath] != nil {
		t.Fatal("default permissions not removed")
	}
	after, _ := json.Marshal(stored)
	if string(before) != string(after) {
		t.Fatal("egress edits modified resources other than their own NetworkPolicies")
	}
	t.Setenv("WORKSPACE_NETWORK_POLICY_ENABLED", "false")
	expect(owner, "PUT", "workspaces/demo/egress", rules, 409)
	expect(admin, "PUT", "egress-defaults", rules, 409)
	t.Setenv("WORKSPACE_NETWORK_POLICY_ENABLED", "true")
	expect(owner, "PUT", "workspaces/demo/egress", rules, 200)
	t.Setenv("WORKSPACE_NETWORK_POLICY_ENABLED", "false")
	if err = a.syncEgress(ctx, "demo"); err != nil {
		t.Fatal(err)
	}
	settings, err = a.egressSettings(ctx, "demo")
	if err != nil || len(settings.Rules) != 2 || settings.Status != "inactive" || stored[policyPath] != nil {
		t.Fatal("disabled policies isolated pods or discarded saved rules", err)
	}
	t.Setenv("WORKSPACE_NETWORK_POLICY_ENABLED", "true")
	if err = a.syncEgress(ctx, "demo"); err != nil || stored[policyPath] == nil {
		t.Fatal("re-enabling did not restore saved exceptions", err)
	}
	expect(owner, "PUT", "workspaces/demo/egress", `{"rules":[]}`, 200)
	// Leave no test-only settings to affect the remaining lifecycle assertions.
	if _, err = a.db.Exec(ctx, `DELETE FROM hub_egress_defaults; DELETE FROM workspace_egress`); err != nil {
		t.Fatal(err)
	}
}
