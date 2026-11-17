package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestRotateWorkspaceToken(t *testing.T) {
	for _, scenario := range []string{"rotate", "clear-ssh", "conflict", "unmanaged", "wrong-workspace", "deleting", "conflicts-exhausted"} {
		t.Run(scenario, func(t *testing.T) {
			secret := object{"metadata": object{"resourceVersion": "1", "labels": object{"app.kubernetes.io/managed-by": "herdr-hub", "herdr-hub/workspace": "demo"}}, "data": object{"token": "old", "authorized_keys": "keep", "other": "preserved"}}
			meta := secret["metadata"].(object)
			switch scenario {
			case "unmanaged":
				meta["labels"].(object)["app.kubernetes.io/managed-by"] = "other"
			case "wrong-workspace":
				meta["labels"].(object)["herdr-hub/workspace"] = "other"
			case "deleting":
				meta["deletionTimestamp"] = "now"
			}
			puts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/namespaces/test/secrets/herdr-demo" {
					t.Errorf("wrong path: %s", r.URL.Path)
				}
				if r.Method == "PUT" {
					puts++
					if scenario == "conflicts-exhausted" || scenario == "conflict" && puts == 1 {
						writeError(w, 409, "conflict")
						return
					}
					if err := json.NewDecoder(r.Body).Decode(&secret); err != nil {
						t.Error(err)
					}
				}
				writeJSON(w, 200, secret)
			}))
			defer server.Close()
			file := filepath.Join(t.TempDir(), "token")
			if err := os.WriteFile(file, []byte("kube-token"), 0600); err != nil {
				t.Fatal(err)
			}
			k := &kubernetes{client: server.Client(), base: server.URL, namespace: "test", tokenFile: file}
			err := k.rotateToken(context.Background(), "demo", scenario == "clear-ssh")
			blocked := scenario == "unmanaged" || scenario == "wrong-workspace" || scenario == "deleting" || scenario == "conflicts-exhausted"
			if (err != nil) != blocked {
				t.Fatalf("unexpected error: %v", err)
			}
			if blocked {
				return
			}
			data := secret["data"].(map[string]any)
			token, err := base64.StdEncoding.DecodeString(data["token"].(string))
			if err != nil || len(token) != 64 || data["other"] != "preserved" {
				t.Fatal("invalid token or unrelated data changed")
			}
			want := "keep"
			if scenario == "clear-ssh" {
				want = base64.StdEncoding.EncodeToString([]byte("\n"))
			}
			if data["authorized_keys"] != want {
				t.Fatal("unexpected SSH key change")
			}
		})
	}
}
