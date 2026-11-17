package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateEgressRules(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		valid       bool
	}{
		{"empty", `[]`, true},
		{"ip", `[{"name":"api","cidr":"192.0.2.10","protocol":"tcp","port":443}]`, true},
		{"network", `[{"name":"api","cidr":"192.0.2.10/24","protocol":"TCP","port":443}]`, true},
		{"ipv6", `[{"name":"api","cidr":"2001:db8::1","protocol":"UDP","port":53}]`, true},
		{"pods", `[{"name":"proxy","namespace":"traefik","pod_selector":{"app.kubernetes.io/name":"traefik"},"protocol":"TCP","port":8443}]`, true},
		{"empty-value", `[{"name":"proxy","namespace":"traefik","pod_selector":{"flag":""},"protocol":"TCP","port":8443}]`, true},
		{"hostname", `[{"name":"api","cidr":"api.example.com","protocol":"TCP","port":443}]`, false},
		{"zone", `[{"name":"api","cidr":"fe80::1%eth0","protocol":"TCP","port":443}]`, false},
		{"mapped", `[{"name":"api","cidr":"::ffff:192.168.1.1","protocol":"TCP","port":443}]`, false},
		{"no-destination", `[{"name":"api","protocol":"TCP","port":443}]`, false},
		{"mixed", `[{"name":"api","cidr":"192.168.1.1","namespace":"traefik","protocol":"TCP","port":443}]`, false},
		{"wildcard-pods", `[{"name":"api","namespace":"traefik","protocol":"TCP","port":443}]`, false},
		{"no-namespace", `[{"name":"api","pod_selector":{"app":"api"},"protocol":"TCP","port":443}]`, false},
		{"bad-namespace", `[{"name":"api","namespace":"*","pod_selector":{"app":"api"},"protocol":"TCP","port":443}]`, false},
		{"bad-label", `[{"name":"api","namespace":"traefik","pod_selector":{"/name":"traefik"},"protocol":"TCP","port":443}]`, false},
		{"bad-value", `[{"name":"api","namespace":"traefik","pod_selector":{"app":"*"},"protocol":"TCP","port":443}]`, false},
		{"all-ports", `[{"name":"api","cidr":"192.168.1.1","protocol":"TCP","port":0}]`, false},
		{"high-port", `[{"name":"api","cidr":"192.168.1.1","protocol":"TCP","port":65536}]`, false},
		{"protocol", `[{"name":"api","cidr":"192.168.1.1","protocol":"ALL","port":443}]`, false},
		{"duplicate", `[{"name":"api","cidr":"192.168.1.1","protocol":"TCP","port":443},{"name":"api","cidr":"192.168.1.2","protocol":"TCP","port":443}]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var rules []egressRule
			if err := json.Unmarshal([]byte(tc.input), &rules); err != nil {
				t.Fatal(err)
			}
			if err := validateEgressRules(rules); (err == nil) != tc.valid {
				t.Fatalf("valid=%v: %v", tc.valid, err)
			}
			if tc.name == "ip" && (rules[0].CIDR != "192.0.2.10/32" || rules[0].Protocol != "TCP") {
				t.Fatal("IP/protocol was not normalized", rules)
			}
			if tc.name == "network" && rules[0].CIDR != "192.0.2.0/24" {
				t.Fatal("CIDR was not canonicalized", rules)
			}
		})
	}
	if validateEgressRules(make([]egressRule, 33)) == nil {
		t.Fatal("unbounded rule count")
	}
}

func TestSaveEgressPolicy(t *testing.T) {
	for _, scenario := range []string{"create", "same", "update", "conflict", "unmanaged", "wrong-scope", "deleting", "remove", "remove-conflict", "remove-pending", "absent"} {
		t.Run(scenario, func(t *testing.T) {
			rules := []egressRule{{Name: "api", CIDR: "192.0.2.10/32", Protocol: "TCP", Port: 443}}
			k := &kubernetes{namespace: "test"}
			var stored object
			if scenario != "create" && scenario != "absent" {
				// Roundtrip as a real API response, including float64 port numbers.
				b, _ := json.Marshal(k.egressPolicy("demo", rules))
				_ = json.Unmarshal(b, &stored)
				meta := stored["metadata"].(object)
				meta["resourceVersion"], meta["uid"], meta["annotations"] = "4", "original", object{"custom": "keep"}
				switch scenario {
				case "unmanaged":
					meta["labels"].(object)["app.kubernetes.io/managed-by"] = "other"
				case "wrong-scope":
					meta["labels"].(object)["herdr-hub/egress-scope"] = "other"
				case "deleting":
					meta["deletionTimestamp"] = "now"
				}
			}
			if scenario == "update" || scenario == "conflict" {
				rules[0].Port = 80
			}
			if strings.HasPrefix(scenario, "remove") || scenario == "absent" {
				rules = nil
			}
			writes := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.Contains(r.URL.Path, "/networkpolicies") {
					t.Error("egress touched unrelated resource", r.URL.Path)
					writeError(w, 500, "unexpected resource")
					return
				}
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
					if r.Method == "PUT" {
						meta := updated["metadata"].(object)
						if meta["resourceVersion"] != stored["metadata"].(object)["resourceVersion"] || meta["annotations"].(object)["custom"] != "keep" {
							t.Error("metadata/resourceVersion lost")
						}
					}
					stored = updated
					writeJSON(w, 200, stored)
				case "DELETE":
					writes++
					var options object
					_ = json.NewDecoder(r.Body).Decode(&options)
					pre := options["preconditions"].(object)
					if pre["uid"] != "original" || pre["resourceVersion"] != stored["metadata"].(object)["resourceVersion"] {
						t.Error("unguarded deletion")
					}
					if scenario == "remove-conflict" && writes == 1 {
						stored["metadata"].(object)["resourceVersion"] = "5"
						writeError(w, 409, "conflict")
						return
					}
					if scenario != "remove-pending" {
						stored = nil
					}
					writeJSON(w, 200, object{"status": "Success"})
				default:
					t.Error("unexpected method", r.Method)
				}
			}))
			defer server.Close()
			file := filepath.Join(t.TempDir(), "token")
			_ = os.WriteFile(file, []byte("test"), 0600)
			k.client, k.base, k.tokenFile = server.Client(), server.URL, file
			err := k.saveEgressPolicy(context.Background(), "demo", rules)
			wantError := scenario == "unmanaged" || scenario == "wrong-scope" || scenario == "deleting" || scenario == "remove-pending"
			if (err != nil) != wantError {
				t.Fatalf("unexpected result: %v", err)
			}
			if (wantError && scenario != "remove-pending" || scenario == "same" || scenario == "absent") && writes != 0 {
				t.Fatal("unsafe or unnecessary writes", writes)
			}
		})
	}
}

func TestEgressPolicyScope(t *testing.T) {
	k := &kubernetes{namespace: "test"}
	rules := []egressRule{{Name: "proxy", Namespace: "traefik", PodSelector: map[string]string{"app.kubernetes.io/name": "traefik"}, Protocol: "TCP", Port: 8443}}
	for _, scope := range []string{"", "demo"} {
		policy := k.egressPolicy(scope, rules)
		spec := policy["spec"].(object)
		if _, exists := spec["ingress"]; exists || len(spec["policyTypes"].([]string)) != 1 || spec["policyTypes"].([]string)[0] != "Egress" {
			t.Fatal("changed inbound policy")
		}
		selector := spec["podSelector"].(object)
		if scope == "" {
			if selector["matchExpressions"] == nil {
				t.Fatal("global must select existing and future workspaces, not all pods")
			}
		} else if selector["matchLabels"].(object)["herdr-hub/workspace"] != "demo" {
			t.Fatal("workspace selector escaped scope")
		}
		peer := spec["egress"].([]object)[0]["to"].([]object)[0]
		if peer["namespaceSelector"].(object)["matchLabels"].(object)["kubernetes.io/metadata.name"] != "traefik" || peer["podSelector"] == nil || peer["ipBlock"] != nil {
			t.Fatal("namespace and pod labels must match together")
		}
	}
}
