package main

import (
	"reflect"
	"testing"
)

func TestWorkspaceResourceValidation(t *testing.T) {
	for _, test := range []struct {
		name      string
		resources workspaceResources
		valid     bool
	}{
		{"standard", workspaceResources{"500m", "2", "1Gi", "4Gi"}, true},
		{"equivalent", workspaceResources{"0.5", "500m", "1024Mi", "1Gi"}, true},
		{"leading zeros are decimal", workspaceResources{"010m", "9m", "1Gi", "4Gi"}, false},
		{"no CPU cap", legacyWorkspaceResources(), true},
		{"bytes", workspaceResources{"1", "2", "1024", "1Ki"}, true},
		{"CPU request exceeds limit", workspaceResources{"2001m", "2", "1Gi", "4Gi"}, false},
		{"memory request exceeds limit", workspaceResources{"500m", "2", "1025Mi", "1Gi"}, false},
		{"missing request", workspaceResources{"", "2", "1Gi", "4Gi"}, false},
		{"missing memory limit", workspaceResources{"500m", "2", "1Gi", ""}, false},
		{"zero", workspaceResources{"0", "2", "1Gi", "4Gi"}, false},
		{"negative", workspaceResources{"-1", "2", "1Gi", "4Gi"}, false},
		{"fractional millicores", workspaceResources{"0.0001", "2", "1Gi", "4Gi"}, false},
		{"bad CPU unit", workspaceResources{"1Gi", "2", "1Gi", "4Gi"}, false},
		{"bad memory unit", workspaceResources{"500m", "2", "1GB", "4Gi"}, false},
		{"overflow", workspaceResources{"500m", "2", "999999999999999999999Ti", "4Gi"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.resources.validate(); (err == nil) != test.valid {
				t.Fatalf("validate() = %v, valid = %v", err, test.valid)
			}
		})
	}
}

func TestWorkspaceResourceManifest(t *testing.T) {
	settings := workspaceResources{"250m", "1500m", "512Mi", "2Gi"}
	k := &kubernetes{}
	for _, obj := range k.resources(workspace{Name: "demo", Resources: settings}, "token") {
		if obj["kind"] != "Deployment" {
			continue
		}
		pod := obj["spec"].(object)["template"].(object)["spec"].(object)
		got := pod["containers"].([]object)[0]["resources"]
		want := object{"requests": object{"cpu": "250m", "memory": "512Mi"}, "limits": object{"cpu": "1500m", "memory": "2Gi"}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("resources = %#v, want %#v", got, want)
		}
		return
	}
	t.Fatal("Deployment missing")
}
