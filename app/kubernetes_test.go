package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestWorkspaceResources(t *testing.T) {
	k := &kubernetes{namespace: "hub", image: "registry/herdr:ARM", domain: "apps.test", gateway: "public", gatewayNamespace: "networking", webListener: "https", storageSize: "10Gi"}
	objects := k.resources(workspace{Name: "project-a", SSHKey: "public-key", SSHListener: "ssh-2201"}, "web-token")
	if len(objects) != 6 {
		t.Fatal("expected six resources")
	}
	// Marshal/unmarshal exercises the exact wire representation.
	b, _ := json.Marshal(objects)
	var wire []map[string]any
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatal(err)
	}
	deployment := wire[2]["spec"].(map[string]any)
	pod := deployment["template"].(map[string]any)["spec"].(map[string]any)
	if pod["automountServiceAccountToken"] != false {
		t.Fatal("workspace must not inherit Hub credentials")
	}
	container := pod["containers"].([]any)[0].(map[string]any)
	if container["image"] != "registry/herdr:ARM" {
		t.Fatal("wrong image")
	}
	mounts := container["volumeMounts"].([]any)
	if mounts[0].(map[string]any)["mountPath"] != "/home/dev" {
		t.Fatal("home not persisted")
	}
	keyMount := mounts[1].(map[string]any)
	if keyMount["mountPath"] != "/run/ssh-keys/authorized_keys" || keyMount["subPath"] != "authorized_keys" || keyMount["readOnly"] != true {
		t.Fatal("mount the key file without exposing the projected Secret directory to OpenSSH StrictModes")
	}
	route := wire[4]["spec"].(map[string]any)
	if route["hostnames"].([]any)[0] != "project-a.apps.test" {
		t.Fatal("wrong hostname")
	}
	tcp := wire[5]["spec"].(map[string]any)
	if tcp["parentRefs"].([]any)[0].(map[string]any)["sectionName"] != "ssh-2201" {
		t.Fatal("wrong TCP listener")
	}
	if len(k.resources(workspace{Name: "no-ssh"}, "token")) != 5 {
		t.Fatal("unexpected SSH route")
	}
}

func TestValidation(t *testing.T) {
	for _, s := range []string{"", "../escape", "Uppercase", "a.b", "a-"} {
		if validName(s) {
			t.Fatalf("accepted %q", s)
		}
	}
	if !validName("project-1") {
		t.Fatal("valid name rejected")
	}
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	sshPub, _ := ssh.NewPublicKey(pub)
	if !validPublicKey(string(ssh.MarshalAuthorizedKey(sshPub))) {
		t.Fatal("valid public key rejected")
	}
	if validPublicKey("ssh-ed25519 invalid") || validPublicKey("-----BEGIN OPENSSH PRIVATE KEY-----") {
		t.Fatal("bad key accepted")
	}
}

func TestEnsureAndOwnership(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	_ = os.WriteFile(tokenFile, []byte("test-token"), 0600)
	var stored object
	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("missing service account authentication")
		}
		if r.Method == "POST" {
			posts++
			_ = json.NewDecoder(r.Body).Decode(&stored)
			writeJSON(w, 201, stored)
			return
		}
		if stored == nil {
			w.WriteHeader(404)
			return
		}
		writeJSON(w, 200, stored)
	}))
	defer server.Close()
	k := &kubernetes{client: server.Client(), base: server.URL, tokenFile: tokenFile, namespace: "hub"}
	secret := k.resources(workspace{Name: "demo"}, "original")[0]
	if err := k.ensure(context.Background(), secret); err != nil {
		t.Fatal(err)
	}
	if err := k.ensure(context.Background(), k.resources(workspace{Name: "demo"}, "replacement")[0]); err != nil {
		t.Fatal(err)
	}
	if posts != 1 {
		t.Fatal("retry replaced secret")
	}
	stored["metadata"].(map[string]any)["labels"] = map[string]any{}
	if k.ensure(context.Background(), secret) == nil {
		t.Fatal("accepted unmanaged resource")
	}
	if k.remove(context.Background(), "secrets", "herdr-demo") == nil {
		t.Fatal("deleted unmanaged resource")
	}
}

func TestAPIRequiresToken(t *testing.T) {
	a := &application{token: "test-token"}
	for _, header := range []string{"", "test-token", "Bearer wrong-token"} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/api/workspaces", nil)
		r.Header.Set("Authorization", header)
		a.api(w, r)
		if w.Code != 401 {
			t.Fatal(w.Code)
		}
	}
}
