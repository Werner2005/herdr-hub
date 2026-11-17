package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestWorkspacePortValidation(t *testing.T) {
	for _, protocol := range []string{"TCP", "UDP", "HTTP"} {
		if err := validateWorkspacePort(workspacePort{Name: "my-app", Protocol: protocol, InternalPort: 3000}); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []workspacePort{{Name: "bad/name", Protocol: "TCP", InternalPort: 80}, {Name: "app", Protocol: "TLS", InternalPort: 80}, {Name: "app", Protocol: "UDP", InternalPort: 0}, {Name: "app", Protocol: "TCP", InternalPort: 65536}, {Name: "app", Protocol: "HTTP", InternalPort: 7317}, {Name: "app", Protocol: "TCP", InternalPort: 2222}} {
		if validateWorkspacePort(p) == nil {
			t.Fatalf("accepted invalid mapping: %+v", p)
		}
	}
}

func TestExportResources(t *testing.T) {
	t.Setenv("WORKSPACE_NETWORK_POLICY_ENABLED", "true")
	t.Setenv("WORKSPACE_GATEWAY_POD_SELECTOR", `{"custom":"envoy"}`)
	t.Setenv("WORKSPACE_GATEWAY_NAMESPACE_SELECTOR", `{"network":"public"}`)
	k := &kubernetes{namespace: "workspaces", gatewayNamespace: "networking", gateway: "public", webListener: "https", domain: "apps.example.com"}
	for _, protocol := range []string{"TCP", "UDP", "HTTP"} {
		p := workspacePort{Name: "app", ID: "abc", Protocol: protocol, InternalPort: 3000, ExternalPort: 2300}
		resources := k.exportResources("demo", p)
		data, _ := json.Marshal(resources)
		var decoded []object
		_ = json.Unmarshal(data, &decoded)
		service := decoded[0]["spec"].(map[string]any)
		ports := service["ports"].([]any)[0].(map[string]any)
		want := "TCP"
		if protocol == "UDP" {
			want = "UDP"
		}
		if ports["protocol"] != want || ports["targetPort"] != float64(3000) {
			t.Fatal("incorrect service", service)
		}
		policy := decoded[1]["spec"].(map[string]any)
		ingress := policy["ingress"].([]any)[0].(map[string]any)
		peer := ingress["from"].([]any)[0].(map[string]any)
		selector := peer["podSelector"].(map[string]any)["matchLabels"].(map[string]any)
		if len(selector) != 1 || selector["custom"] != "envoy" {
			t.Fatal("custom selector not preserved", selector)
		}
		kind, plural := p.route()
		if decoded[2]["kind"] != kind || !strings.Contains(k.endpoint(plural, p.resource()), "/namespaces/workspaces/"+plural+"/") {
			t.Fatal("incorrect route")
		}
		if protocol == "HTTP" && (!strings.Contains(string(data), "demo.apps.example.com") || strings.Contains(string(data), "export-abc.apps.example.com")) {
			t.Fatal("HTTP hostname missing")
		}
		parent := decoded[2]["spec"].(map[string]any)["parentRefs"].([]any)[0].(map[string]any)
		if parent["sectionName"] != p.resource() {
			t.Fatal("route must bind only to its dedicated listener", parent)
		}
	}
}

func TestExportAddresses(t *testing.T) {
	k := &kubernetes{domain: "apps.example.com", sshHost: "ssh.apps.example.com"}
	for _, protocol := range []string{"HTTP", "TCP", "UDP"} {
		want := "test.apps.example.com:2300"
		if protocol == "HTTP" {
			want = "https://" + want
		}
		if got := k.exportAddress("test", workspacePort{ID: "abc", Protocol: protocol, ExternalPort: 2300}); got != want {
			t.Fatalf("%s: got %s want %s", protocol, got, want)
		}
	}
	if got := k.exportAddress("test", workspacePort{Protocol: "HTTP"}); got != "" {
		t.Fatal("unmigrated export must not advertise an incorrect URL", got)
	}
}

func TestHTTPSExportListener(t *testing.T) {
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("test"), 0600); err != nil {
		t.Fatal(err)
	}
	gateway := object{"metadata": object{"resourceVersion": "7"}, "spec": object{"listeners": []object{{"name": "https", "port": 443, "protocol": "HTTPS", "tls": object{"mode": "Terminate", "certificateRefs": []object{{"name": "wildcard-tls"}}}}}}}
	puts, conflicts := 0, 1
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" {
			puts++
			if conflicts > 0 {
				conflicts--
				writeError(w, 409, "concurrent update")
				return
			}
			var next object
			if err := json.NewDecoder(r.Body).Decode(&next); err != nil {
				t.Error(err)
			}
			if next["metadata"].(map[string]any)["resourceVersion"] != "7" {
				t.Error("resourceVersion lost")
			}
			gateway = next
		}
		writeJSON(w, 200, gateway)
	}))
	defer server.Close()
	k := &kubernetes{client: server.Client(), base: server.URL, tokenFile: token, namespace: "workspaces", gateway: "public", gatewayNamespace: "networking", webListener: "https", domain: "apps.example.com"}
	p := workspacePort{ID: "abc", Protocol: "HTTP", ExternalPort: 2300}
	if err := k.exportListener(context.Background(), "test", p, false); err != nil {
		t.Fatal(err)
	}
	if puts != 2 {
		t.Fatal("must retry Gateway conflicts", puts)
	}
	listeners := gateway["spec"].(map[string]any)["listeners"].([]any)
	listener := listeners[1].(map[string]any)
	if listener["protocol"] != "HTTPS" || listener["port"] != float64(2300) || listener["hostname"] != "test.apps.example.com" {
		t.Fatal("incorrect HTTPS export", listener)
	}
	refs := listener["tls"].(map[string]any)["certificateRefs"].([]any)
	if refs[0].(map[string]any)["name"] != "wildcard-tls" {
		t.Fatal("certificate not inherited")
	}
	if err := k.exportListener(context.Background(), "test", p, false); err != nil || puts != 2 {
		t.Fatal("retry must be idempotent", err, puts)
	}
	// Certificate changes are applied to existing export listeners too.
	web := listeners[0].(map[string]any)
	web["tls"].(map[string]any)["certificateRefs"] = []object{{"name": "rotated-tls"}}
	if err := k.exportListener(context.Background(), "test", p, false); err != nil || puts != 3 {
		t.Fatal("certificate refresh failed", err, puts)
	}
	// Cleanup must work even if the source web listener has lost its TLS config.
	listeners = gateway["spec"].(map[string]any)["listeners"].([]any)
	delete(listeners[0].(map[string]any), "tls")
	if err := k.exportListener(context.Background(), "test", p, true); err != nil {
		t.Fatal(err)
	}
	if len(gateway["spec"].(map[string]any)["listeners"].([]any)) != 1 {
		t.Fatal("export listener survived cleanup")
	}
	if err := k.exportListener(context.Background(), "test", p, false); err == nil {
		t.Fatal("must not create an HTTPS listener without a certificate")
	}
}

func TestExportWithNetworkPolicyDisabled(t *testing.T) {
	t.Setenv("WORKSPACE_NETWORK_POLICY_ENABLED", "false")
	k := &kubernetes{namespace: "test", gateway: "public", webListener: "https"}
	resources := k.exportResources("demo", workspacePort{ID: "abc", Protocol: "HTTP", InternalPort: 3000})
	if len(resources) != 2 || resources[0]["kind"] != "Service" || resources[1]["kind"] != "HTTPRoute" {
		t.Fatal("disabled NetworkPolicy must not isolate the workspace", resources)
	}
}

func TestExportRouteMigration(t *testing.T) {
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("test"), 0600); err != nil {
		t.Fatal(err)
	}
	k := &kubernetes{namespace: "workspaces", gatewayNamespace: "networking", gateway: "public", webListener: "https", domain: "apps.example.com", tokenFile: token}
	resources := k.exportResources("test", workspacePort{ID: "abc", Protocol: "HTTP", InternalPort: 3000, ExternalPort: 2300})
	desired := resources[len(resources)-1]
	data, _ := json.Marshal(desired)
	var route object
	if err := json.Unmarshal(data, &route); err != nil {
		t.Fatal(err)
	}
	meta := route["metadata"].(map[string]any)
	meta["resourceVersion"] = "7"
	meta["annotations"] = object{"keep": "yes"}
	spec := route["spec"].(map[string]any)
	spec["hostnames"] = []string{"export-abc.apps.example.com"}
	spec["parentRefs"] = []object{{"name": "public", "namespace": "networking", "sectionName": "https"}}
	// Preserve backend rules and API-defaulted fields while moving the endpoint.
	rules := spec["rules"].([]any)
	rules[0].(map[string]any)["backendRefs"].([]any)[0].(map[string]any)["weight"] = float64(1)
	puts, conflicts := 0, 1
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" {
			puts++
			if conflicts > 0 {
				conflicts--
				writeError(w, 409, "concurrent update")
				return
			}
			var next object
			if err := json.NewDecoder(r.Body).Decode(&next); err != nil {
				t.Error(err)
			}
			route = next
		}
		writeJSON(w, 200, route)
	}))
	defer server.Close()
	k.base, k.client = server.URL, server.Client()
	if err := k.ensureExportResource(context.Background(), desired); err != nil || puts != 2 {
		t.Fatal("migration must retry conflicts", err, puts)
	}
	meta = route["metadata"].(map[string]any)
	spec = route["spec"].(map[string]any)
	if meta["resourceVersion"] != "7" || meta["annotations"].(map[string]any)["keep"] != "yes" || spec["hostnames"].([]any)[0] != "test.apps.example.com" || spec["parentRefs"].([]any)[0].(map[string]any)["sectionName"] != "herdr-export-abc" {
		t.Fatal("incorrect migration", route)
	}
	backend := spec["rules"].([]any)[0].(map[string]any)["backendRefs"].([]any)[0].(map[string]any)
	if backend["weight"] != float64(1) {
		t.Fatal("backend rules changed")
	}
	parent := spec["parentRefs"].([]any)[0].(map[string]any)
	parent["kind"] = "Gateway"
	parent["group"] = "gateway.networking.k8s.io"
	if err := k.ensureExportResource(context.Background(), desired); err != nil || puts != 2 {
		t.Fatal("API defaults must not cause repeated updates", err, puts)
	}
	meta["labels"].(map[string]any)["herdr-hub/workspace"] = "foreign"
	if err := k.ensureExportResource(context.Background(), desired); err == nil || puts != 2 {
		t.Fatal("must not update a different workspace's route", err, puts)
	}
}

func TestExportRouteReadiness(t *testing.T) {
	route := object{"metadata": object{"generation": float64(2)}, "status": object{"parents": []any{object{"parentRef": object{"name": "public", "namespace": "networking", "sectionName": "https"}, "conditions": []any{object{"type": "Accepted", "status": "True", "observedGeneration": float64(2)}, object{"type": "ResolvedRefs", "status": "True", "observedGeneration": float64(2)}}}}}}
	if status, _ := routeReadiness(route, "public", "networking", "https"); status != "ready" {
		t.Fatal(status)
	}
	if status, _ := routeReadiness(route, "other", "networking", "https"); status != "pending" {
		t.Fatal("foreign parent accepted")
	}
	route["metadata"].(object)["generation"] = float64(3)
	if status, _ := routeReadiness(route, "public", "networking", "https"); status != "pending" {
		t.Fatal("stale status accepted")
	}
}

func exerciseWorkspacePorts(t *testing.T, a *application, objects map[string]object, request func(string, string, string) *httptest.ResponseRecorder, userRequest func(*http.Cookie, string, string, string) *httptest.ResponseRecorder, cookie, otherCookie *http.Cookie) {
	t.Helper()
	ctx := context.Background()
	a.kube.sshHost = "ssh.apps.example.com"
	check := func(w *httptest.ResponseRecorder, status int) {
		t.Helper()
		if w.Code != status {
			t.Fatalf("exports HTTP %d want %d: %s", w.Code, status, w.Body.String())
		}
	}
	check(userRequest(otherCookie, "GET", "workspaces/demo/ports", ""), 404)
	check(userRequest(otherCookie, "POST", "workspaces/demo/ports", `{"name":"x","protocol":"HTTP","internal_port":3000}`), 404)
	check(userRequest(otherCookie, "DELETE", "workspaces/demo/ports/web", ""), 404)
	check(userRequest(cookie, "POST", "workspaces/demo/ports", `{"name":"bad","protocol":"TPC","internal_port":3000}`), 400)
	for _, p := range []string{"HTTP", "TCP", "UDP"} {
		body := `{"name":"` + strings.ToLower(p) + `","protocol":"` + p + `","internal_port":3000}`
		check(userRequest(cookie, "POST", "workspaces/demo/ports", body), 201)
		check(userRequest(cookie, "POST", "workspaces/demo/ports", body), 409)
	}
	ports, err := a.workspacePorts(ctx, "demo")
	if err != nil || len(ports) != 3 {
		t.Fatal(ports, err)
	}
	if ports[1].ExternalPort == ports[2].ExternalPort || ports[1].ExternalPort == 2201 {
		t.Fatal("port allocation collision", ports)
	}
	if ports[0].ExternalPort == 0 || ports[0].ExternalPort == ports[1].ExternalPort || ports[0].Address != "https://demo.apps.test:"+strconv.Itoa(ports[0].ExternalPort) {
		t.Fatal("HTTP must have a dedicated port on the workspace hostname", ports[0])
	}
	// Simulate an existing generated-host HTTP mapping from the previous release.
	httpPort := ports[0]
	if err := a.kube.exportListener(ctx, "demo", httpPort, true); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(ctx, `UPDATE workspace_ports SET external_port=0 WHERE workspace='demo' AND name=$1`, httpPort.Name); err != nil {
		t.Fatal(err)
	}
	routePath := strings.TrimPrefix(a.kube.endpoint("httproutes", httpPort.resource()), a.kube.base)
	route := objects[routePath]
	spec := route["spec"].(map[string]any)
	spec["hostnames"] = []string{"export-" + httpPort.ID + ".apps.test"}
	spec["parentRefs"] = []object{{"name": "public", "sectionName": "https"}}
	route["metadata"].(map[string]any)["resourceVersion"] = "legacy-route"
	if err := a.syncWorkspacePorts(ctx, "demo", false); err != nil {
		t.Fatal("legacy migration failed", err)
	}
	ports, err = a.workspacePorts(ctx, "demo")
	if err != nil || ports[0].ExternalPort == 0 {
		t.Fatal("migration did not persist allocation", ports, err)
	}
	migrated := objects[routePath]
	if migrated["metadata"].(map[string]any)["resourceVersion"] != "legacy-route" || migrated["spec"].(map[string]any)["hostnames"].([]any)[0] != "demo.apps.test" {
		t.Fatal("migration did not preserve metadata and replace hostname", migrated)
	}
	check(userRequest(cookie, "POST", "workspaces/demo/undeploy", ""), 200)
	// Undeployed export reservations must also block the SSH allocator.
	start, end := a.kube.sshPortStart, a.kube.sshPortEnd
	a.kube.sshPortStart, a.kube.sshPortEnd = ports[1].ExternalPort, ports[2].ExternalPort+1
	allocated, allocationErr := a.allocateSSH(ctx)
	a.kube.sshPortStart, a.kube.sshPortEnd = start, end
	if allocationErr != nil || allocated != ports[2].ExternalPort+1 {
		t.Fatal("SSH reused an export allocation", allocated, allocationErr)
	}
	for _, p := range ports {
		for path := range objects {
			if strings.HasSuffix(path, "/"+p.resource()) {
				t.Fatal("export survived undeploy", path)
			}
		}
	}
	retained, err := a.workspacePorts(ctx, "demo")
	if err != nil || len(retained) != 3 || retained[1].ExternalPort != ports[1].ExternalPort {
		t.Fatal("undeploy lost mappings", retained, err)
	}
	before := len(objects)
	if err := a.syncWorkspacePorts(ctx, "demo", false); err != nil || len(objects) != before {
		t.Fatal("export reconciliation recreated a missing runtime or its network resources", err)
	}
	check(userRequest(cookie, "POST", "workspaces/demo/deploy", ""), 201)
	for _, p := range ports {
		_, kind := p.route()
		path := strings.TrimPrefix(a.kube.endpoint(kind, p.resource()), a.kube.base)
		if objects[path] == nil {
			t.Fatal("redeploy did not restore export", path)
		}
		if p.Protocol == "UDP" {
			// Failed cleanup retains the tombstone and allocation, never deleting foreign resources.
			servicePath := strings.TrimPrefix(a.kube.endpoint("services", p.resource()), a.kube.base)
			service := objects[servicePath]
			labels := service["metadata"].(map[string]any)["labels"].(map[string]any)
			labels["app.kubernetes.io/managed-by"] = "other"
			check(userRequest(cookie, "DELETE", "workspaces/demo/ports/"+p.Name, ""), 502)
			pending, e := a.workspacePorts(ctx, "demo")
			if e != nil || len(pending) != 1 || !pending[0].Deleting || pending[0].ExternalPort != p.ExternalPort || objects[servicePath] == nil {
				t.Fatal("failed cleanup lost reservation", pending, e)
			}
			labels["app.kubernetes.io/managed-by"] = "herdr-hub"
		}
		check(userRequest(cookie, "DELETE", "workspaces/demo/ports/"+p.Name, ""), 204)
	}
	remaining, err := a.workspacePorts(ctx, "demo")
	if err != nil || len(remaining) != 0 {
		t.Fatal("export deletion incomplete", remaining, err)
	}
}
