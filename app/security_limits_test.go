package main

import "testing"

func TestSecurityLimits(t *testing.T) {
	limits := defaultSecurityLimits()
	if err := limits.validate(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		resources workspaceResources
		allowed   bool
	}{
		{workspaceResources{"500m", "4", "1Gi", "8Gi"}, true},
		{workspaceResources{"500m", "4000m", "1Gi", "8192Mi"}, true},
		{workspaceResources{"500m", "4001m", "1Gi", "8Gi"}, false},
		{workspaceResources{"500m", "4", "1Gi", "9Gi"}, false},
		{legacyWorkspaceResources(), false},
	} {
		if err := limits.checkResources(tc.resources); (err == nil) != tc.allowed {
			t.Fatalf("%+v: %v", tc.resources, err)
		}
	}
	for _, mutate := range []func(*securityLimits){
		func(v *securityLimits) { v.CPU = "" },
		func(v *securityLimits) { v.Memory = "0" },
		func(v *securityLimits) { v.VolumeGi = 0 },
		func(v *securityLimits) { v.UserStorageGi = 99 },
		func(v *securityLimits) { v.UserVolumes = 0 },
	} {
		v := limits
		mutate(&v)
		if v.validate() == nil {
			t.Fatalf("invalid limits accepted: %+v", v)
		}
	}
}
