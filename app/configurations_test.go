package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func testConfigurationItems() []resolvedConfiguration {
	c := configuration{Name: "tools", Owner: "alice", Kind: "secret", Keys: []string{"token"}, Ready: true, Version: "random-revision"}
	m := configurationMapping{Configuration: c.Name, Files: []configurationFile{{Key: "token", Path: "/home/dev/.config/tool/token"}}, Env: []configurationEnv{{Key: "token", Name: "TOOL_TOKEN"}}}
	return []resolvedConfiguration{{c, m}}
}

func TestConfigurationValidation(t *testing.T) {
	for _, p := range []string{"/home/dev/.gitconfig", "/home/dev/.config/tool/settings.json", "/home/dev/workspace/.env", "/mnt/config/tool/file", "/mnt/secrets/token"} {
		if !validConfigurationPath(p) {
			t.Errorf("valid path rejected: %s", p)
		}
	}
	for _, p := range []string{"", "/etc/passwd", "/run/ssh-keys/authorized_keys", "/home/dev", "/home/dev/workspace", "/home/dev/.ssh/key", "/home/dev/.config", "/home/dev/.config/herdr/env", "/home/dev/.config/ovis/env", "/home/dev/.config/herdr-web-ui/env", "/home/dev/.local/bin/herdr", "/mnt/shared/a/file", "/home/dev/../root/file", "/home/dev/a/", "/home/dev/a\nfile"} {
		if validConfigurationPath(p) {
			t.Errorf("unsafe path accepted: %s", p)
		}
	}
	for _, name := range []string{"MY_TOKEN", "TOOL_CONFIG", "lowercase", "_KEY", "NODE_OPTIONS"} {
		if !validConfigurationEnv(name) {
			t.Errorf("valid env rejected: %s", name)
		}
	}
	for _, name := range []string{"HOST", "PORT", "HERDR_WEB_TOKEN", "HERDR_WEB_STATE_DIR", "OVIS_TOKEN", "OVIS_STATE_DIR", "HOME", "PATH", "LD_PRELOAD", "BASH_ENV", "XDG_STATE_HOME", "SHELL", "BUN_OPTIONS", "bad-name", "1KEY"} {
		if validConfigurationEnv(name) {
			t.Errorf("unsafe env accepted: %s", name)
		}
	}
	items := testConfigurationItems()
	if err := validateConfigurationMappings(items); err != nil {
		t.Fatal(err)
	}
	items[0].Mapping.Files = append(items[0].Mapping.Files, configurationFile{Key: "token", Path: "/home/dev/.config/tool/token/child"})
	if validateConfigurationMappings(items) == nil {
		t.Fatal("overlapping paths accepted")
	}
	items = testConfigurationItems()
	items[0].Mapping.Env = append(items[0].Mapping.Env, items[0].Mapping.Env[0])
	if validateConfigurationMappings(items) == nil {
		t.Fatal("duplicate environment accepted")
	}
	items = testConfigurationItems()
	items[0].Mapping.Env[0].Key = "missing"
	if validateConfigurationMappings(items) == nil {
		t.Fatal("missing key accepted")
	}
	items = testConfigurationItems()
	items[0].Configuration.Ready = false
	if validateConfigurationMappings(items) == nil {
		t.Fatal("unready configuration accepted")
	}
}

func TestConfigurationOwnerResolution(t *testing.T) {
	items := testConfigurationItems()
	c, m := items[0].Configuration, items[0].Mapping
	if _, err := resolveConfigurationCatalog([]configuration{c}, "bob", []configurationMapping{m}); err == nil {
		t.Fatal("cross-owner mount accepted")
	}
	if _, err := resolveConfigurationCatalog([]configuration{c}, "alice", []configurationMapping{m, m}); err == nil {
		t.Fatal("duplicate configuration accepted")
	}
	resolved, err := resolveConfigurationCatalog([]configuration{c}, "alice", []configurationMapping{m})
	if err != nil || len(resolved) != 1 {
		t.Fatal(resolved, err)
	}
	c.Global = true
	c.Mapping = m
	resolved, err = resolveConfigurationCatalog([]configuration{c}, "bob", nil)
	if err != nil || len(resolved) != 1 {
		t.Fatal("global not automatically mapped", err)
	}
	if _, err = resolveConfigurationCatalog([]configuration{c}, "alice", []configurationMapping{m}); err == nil {
		t.Fatal("global override accepted")
	}
}

func TestConfigurationRuntimeMappings(t *testing.T) {
	for _, scenario := range []string{"update", "conflict", "exhausted", "missing", "deleting", "unmanaged", "wrong-strategy", "env-collision", "no-container"} {
		t.Run(scenario, func(t *testing.T) {
			k := &kubernetes{namespace: "test", image: "herdr:test", storageSize: "1Gi"}
			encoded, _ := json.Marshal(k.resources(workspace{Name: "demo", Volumes: []string{"shared"}}, "original-token")[2])
			var stored object
			_ = json.Unmarshal(encoded, &stored)
			meta := stored["metadata"].(object)
			meta["resourceVersion"] = "4"
			spec := stored["spec"].(object)
			template := spec["template"].(object)
			template["metadata"].(object)["annotations"] = object{"custom": "preserve"}
			pod := template["spec"].(object)
			c := pod["containers"].([]any)[0].(object)
			pod["containers"] = append(pod["containers"].([]any), object{"name": "sidecar", "image": "sidecar:keep", "env": []object{{"name": "TOOL_TOKEN", "value": "sidecar-only"}}})
			switch scenario {
			case "deleting":
				meta["deletionTimestamp"] = "now"
			case "unmanaged":
				meta["labels"].(object)["app.kubernetes.io/managed-by"] = "other"
			case "wrong-strategy":
				spec["strategy"] = object{"type": "RollingUpdate"}
			case "env-collision":
				c["env"] = append(c["env"].([]any), object{"name": "TOOL_TOKEN", "value": "keep"})
			case "no-container":
				c["name"] = "other"
			}
			before, _ := json.Marshal(stored)
			puts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case "GET":
					if scenario == "missing" {
						writeError(w, 404, "missing")
					} else {
						writeJSON(w, 200, stored)
					}
				case "PUT":
					puts++
					if scenario == "exhausted" || (scenario == "conflict" && puts == 1) {
						meta["resourceVersion"] = "5"
						writeError(w, 409, "conflict")
						return
					}
					var updated object
					_ = json.NewDecoder(r.Body).Decode(&updated)
					if updated["metadata"].(object)["resourceVersion"] != meta["resourceVersion"] {
						t.Error("resourceVersion lost")
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
			k.client, k.base, k.tokenFile = server.Client(), server.URL, file
			items := testConfigurationItems()
			err := k.updateConfigurationMappings(context.Background(), "demo", items)
			wantError := slices.Contains([]string{"exhausted", "unmanaged", "wrong-strategy", "env-collision", "no-container"}, scenario)
			if (err != nil) != wantError {
				t.Fatalf("unexpected error: %v", err)
			}
			if scenario != "update" && scenario != "conflict" {
				if scenario != "exhausted" && puts != 0 {
					t.Fatal("unsafe update")
				}
				return
			}
			previousPuts := puts
			if err = k.updateConfigurationMappings(context.Background(), "demo", items); err != nil || puts != previousPuts {
				t.Fatal("unchanged mappings restarted runtime", err)
			}
			updatedPod := stored["spec"].(object)["template"].(object)["spec"].(object)
			if !podUsesConfiguration(updatedPod, configurationResource("tools")) {
				t.Fatal("configuration not referenced")
			}
			for _, v := range objects(updatedPod["volumes"]) {
				if v["name"] == "config-file-0" && v["secret"].(object)["defaultMode"] != float64(288) {
					t.Fatal("secret file permissions not 0440")
				}
			}
			if err = k.updateConfigurationMappings(context.Background(), "demo", nil); err != nil {
				t.Fatal(err)
			}
			if podUsesConfiguration(stored["spec"].(object)["template"].(object)["spec"].(object), configurationResource("tools")) {
				t.Fatal("detach kept references")
			}
			// Strip only our owned annotation to compare everything else, including
			// PVCs, SSH/token env, resources, sidecars and user annotations.
			var original object
			_ = json.Unmarshal(before, &original)
			original["metadata"].(object)["resourceVersion"] = stored["metadata"].(object)["resourceVersion"]
			if !reflect.DeepEqual(original, stored) {
				t.Fatal("mapping update changed unrelated Deployment fields")
			}
		})
	}
}

func TestConfigurationRevisionDoesNotExposeValues(t *testing.T) {
	items := testConfigurationItems()
	before := configurationRevision(items)
	if strings.Contains(before, "random-revision") || len(before) != 64 {
		t.Fatal("invalid revision")
	}
	items[0].Configuration.Version = "new-random-version"
	if before == configurationRevision(items) {
		t.Fatal("value replacement would not restart workspace")
	}
	items[0].Mapping.Env[0].Name = "OTHER_TOKEN"
	if before == configurationRevision(items) {
		t.Fatal("mapping edit did not change revision")
	}
}

func TestConfigurationMappingOrderIsStable(t *testing.T) {
	items := testConfigurationItems()
	first := items[0].Configuration
	second := first
	second.Name = "other"
	one := items[0].Mapping
	two := one
	two.Configuration = second.Name
	two.Files = []configurationFile{{Key: "token", Path: "/mnt/secrets/other/token"}}
	two.Env = []configurationEnv{{Key: "token", Name: "OTHER_TOKEN"}}
	left, err := resolveConfigurationCatalog([]configuration{first, second}, "alice", []configurationMapping{one, two})
	if err != nil {
		t.Fatal(err)
	}
	right, err := resolveConfigurationCatalog([]configuration{first, second}, "alice", []configurationMapping{two, one})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(left, right) || configurationRevision(left) != configurationRevision(right) {
		t.Fatal("equivalent mappings would restart runtime twice")
	}
}
