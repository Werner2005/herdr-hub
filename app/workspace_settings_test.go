package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestDynamicListenerPreservesGateway(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("token"), 0600); err != nil {
		t.Fatal(err)
	}
	gateway := object{"metadata": object{"resourceVersion": "1"}, "spec": object{"listeners": []object{{"name": "https", "port": 443, "protocol": "HTTPS"}, {"name": "foreign", "port": 2209, "protocol": "TCP"}}}}
	conflicts := 1
	puts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/apis/gateway.networking.k8s.io/v1/namespaces/networking/gateways/public" {
			t.Error("wrong Gateway namespace")
		}
		if r.Method == "PUT" {
			puts++
			if conflicts > 0 {
				conflicts--
				writeError(w, 409, "concurrent update")
				return
			}
			var updated object
			if err := json.NewDecoder(r.Body).Decode(&updated); err != nil {
				t.Error(err)
			}
			gateway = updated
		}
		writeJSON(w, 200, gateway)
	}))
	defer server.Close()
	k := &kubernetes{client: server.Client(), base: server.URL, tokenFile: tokenFile, namespace: "hub", gatewayNamespace: "networking", gateway: "public"}
	item := workspace{Name: "demo", SSHListener: "herdr-demo", SSHPort: 2201}
	if err := k.workspaceListener(context.Background(), item, false); err != nil {
		t.Fatal(err)
	}
	if puts != 2 {
		t.Fatal("must retry a resourceVersion conflict")
	}
	listeners := gateway["spec"].(map[string]any)["listeners"].([]any)
	if len(listeners) != 3 || listeners[0].(map[string]any)["name"] != "https" || listeners[1].(map[string]any)["name"] != "foreign" {
		t.Fatal("foreign listeners were modified")
	}
	allowed := listeners[2].(map[string]any)["allowedRoutes"].(map[string]any)["namespaces"].(map[string]any)
	if allowed["from"] != "Selector" {
		t.Fatal("cross-namespace listener must select workspace namespace")
	}
	if err := k.workspaceListener(context.Background(), item, false); err != nil {
		t.Fatal(err)
	}
	if puts != 2 {
		t.Fatal("retry replaced existing listener")
	}
	conflict := item
	conflict.SSHPort = 2209
	if k.workspaceListener(context.Background(), conflict, false) == nil {
		t.Fatal("accepted listener port conflict")
	}
	if err := k.workspaceListener(context.Background(), item, true); err != nil {
		t.Fatal(err)
	}
	if len(gateway["spec"].(map[string]any)["listeners"].([]any)) != 2 {
		t.Fatal("did not remove only the workspace listener")
	}
	if err := k.workspaceListener(context.Background(), item, true); err != nil {
		t.Fatal(err)
	}
	if puts != 3 {
		t.Fatal("deletion retry was not idempotent")
	}
	if err := k.portListener(context.Background(), "herdr-export-abc", 2300, "UDP", "UDPRoute", false); err != nil {
		t.Fatal(err)
	}
	listeners = gateway["spec"].(map[string]any)["listeners"].([]any)
	udp := listeners[2].(map[string]any)
	kinds := udp["allowedRoutes"].(map[string]any)["kinds"].([]any)
	if udp["protocol"] != "UDP" || kinds[0].(map[string]any)["kind"] != "UDPRoute" {
		t.Fatal("UDP export listener has incorrect protocol or allowed route", udp)
	}
	if k.portListener(context.Background(), "herdr-export-other", 2300, "TCP", "TCPRoute", false) == nil {
		t.Fatal("cross-protocol external port collision accepted")
	}
	if err := k.portListener(context.Background(), "herdr-export-abc", 2300, "UDP", "UDPRoute", true); err != nil {
		t.Fatal(err)
	}
}
