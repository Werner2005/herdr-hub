package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseMount(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		raw, kind, source, target string
		ro                        bool
	}{
		{"db-data:/data", "named", "db-data", "/data", false},
		{"db-data:/data:ro", "named", "db-data", "/data", true},
		{"db-data:/data:rw", "named", "db-data", "/data", false},
		{"/scratch", "anonymous", "", "/scratch", false},
		{"./project:/app", "workspace", "project", "/app", false},
		{project + ":/app:ro", "workspace", "project", "/app", true},
		{".:/app", "workspace", ".", "/app", false},
	} {
		t.Run(test.raw, func(t *testing.T) {
			m, err := parseMount(test.raw, root, root)
			if err != nil {
				t.Fatal(err)
			}
			if m.Kind != test.kind || m.Source != test.source || m.Target != test.target || m.ReadOnly != test.ro {
				t.Fatalf("unexpected mount %+v", m)
			}
		})
	}
}

func TestRejectUnsafeMountSources(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("hello"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		outside + ":/data", "./escape:/data", "../:/data", "./missing:/data", "./file:/data",
		":/data", "data:/data:z", "data:/data:ro:extra", "data:relative", "data:/data/../proc",
		"/var/run/docker.sock:/var/run/docker.sock", "data:/dev",
	} {
		t.Run(raw, func(t *testing.T) {
			if m, err := parseMount(raw, root, root); err == nil {
				t.Fatalf("accepted unsafe mount %+v", m)
			}
		})
	}
}
