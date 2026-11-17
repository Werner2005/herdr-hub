package main

import (
	"slices"
	"testing"
)

func TestWorkspaceSecurityContext(t *testing.T) {
	t.Setenv("WORKSPACE_RUNTIME_CLASS", "sandbox")
	k := &kubernetes{}
	pod := k.resources(workspace{Name: "demo"}, "token")[2]["spec"].(object)["template"].(object)["spec"].(object)
	if pod["automountServiceAccountToken"] != false || pod["runtimeClassName"] != "sandbox" {
		t.Fatal("missing workspace isolation settings")
	}
	if pod["securityContext"].(object)["seccompProfile"].(object)["type"] != "RuntimeDefault" {
		t.Fatal("seccomp missing")
	}
	if security := pod["securityContext"].(object); security["fsGroup"] != 1001 || security["fsGroupChangePolicy"] != "OnRootMismatch" {
		t.Fatal("workspace must retain fsGroup 1001 and avoid repeated recursive volume permission scans")
	}
	container := pod["containers"].([]object)[0]
	security := container["securityContext"].(object)
	if security["runAsUser"] != 1001 || security["runAsGroup"] != 1001 {
		t.Fatal("wrong workspace identity")
	}
	caps := security["capabilities"].(object)
	if !slices.Equal(caps["drop"].([]string), []string{"ALL"}) {
		t.Fatal("default capabilities not dropped")
	}
	if !slices.Contains(caps["add"].([]string), "AUDIT_WRITE") {
		t.Fatal("Ubuntu sshd requires AUDIT_WRITE to open interactive PTY sessions")
	}
	for _, forbidden := range []string{"SYS_ADMIN", "SYS_PTRACE", "NET_ADMIN", "NET_RAW", "MKNOD"} {
		if slices.Contains(caps["add"].([]string), forbidden) {
			t.Fatalf("excess capability %s", forbidden)
		}
	}
	if security["allowPrivilegeEscalation"] == false {
		t.Fatal("workspace sudo was disabled")
	}
	init := pod["initContainers"].([]object)[0]["securityContext"].(object)
	if init["allowPrivilegeEscalation"] != false || init["readOnlyRootFilesystem"] != true {
		t.Fatal("init container not hardened")
	}
}
