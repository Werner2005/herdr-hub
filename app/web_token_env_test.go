package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Exercise the actual startup shell block without installing/running the UI.
func TestWebTokenEnvSynchronization(t *testing.T) {
	entrypoint, err := os.ReadFile("../docker/entrypoint.sh")
	if err != nil {
		t.Fatal(err)
	}
	text := string(entrypoint)
	start, end := strings.Index(text, "umask 077"), strings.Index(text, "cd /home/dev/workspace")
	if start < 0 || end < start {
		t.Fatal("credential startup block missing")
	}
	script := "set -euo pipefail\n" + text[start:end]
	for _, scenario := range []string{"missing", "existing", "dotenv", "symlink", "dotenv-symlink", "directory-symlink"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			file := filepath.Join(dir, "env")
			if scenario == "existing" || scenario == "dotenv" {
				if scenario == "dotenv" {
					file = filepath.Join(dir, ".env")
				}
				if err := os.WriteFile(file, []byte("CUSTOM_SETTING=keep\nHOST=old\nexport PORT=old\n HERDR_WEB_TOKEN = stale\nOVIS_TOKEN=stale\nOVIS_STATE_DIR=stale\nHERDR_WEB_STATE_DIR=stale\nBUN_OPTIONS=stale\n"), 0644); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "dotenv-symlink" {
				file = filepath.Join(dir, ".env")
			}
			if scenario == "symlink" || scenario == "dotenv-symlink" {
				if err := os.Symlink(filepath.Join(dir, "target"), file); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "directory-symlink" {
				link := filepath.Join(t.TempDir(), "config")
				if err := os.Symlink(dir, link); err != nil {
					t.Fatal(err)
				}
				dir = link
			}
			for _, token := range []string{"first-token", "rotated-token"} {
				cmd := exec.Command("bash", "-c", script)
				cmd.Env = append(os.Environ(), "plugin_config_dir="+dir, "HOST=0.0.0.0", "PORT=7317", "OVIS_TOKEN="+token, "OVIS_STATE_DIR=/home/dev/.config/ovis")
				out, err := cmd.CombinedOutput()
				if scenario == "symlink" || scenario == "dotenv-symlink" || scenario == "directory-symlink" {
					if err == nil {
						t.Fatal("symlink accepted")
					}
					return
				}
				if err != nil {
					t.Fatalf("startup block: %v: %s", err, out)
				}
				data, err := os.ReadFile(file)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Count(string(data), "OVIS_TOKEN=") != 1 || !strings.Contains(string(data), "OVIS_TOKEN="+token+"\n") || strings.Contains(string(data), "stale") || !strings.Contains(string(data), "HERDR_WEB_TOKEN="+token+"\n") {
					t.Fatalf("stale/duplicate credential: %s", data)
				}
				if !strings.Contains(string(data), "OVIS_STATE_DIR=/home/dev/.config/ovis\n") {
					t.Fatal("state directory not synchronized")
				}
				if strings.Count(string(data), "BUN_OPTIONS=") != 1 || !strings.Contains(string(data), "BUN_OPTIONS=--preload=/usr/local/share/herdr/ovis-preload.ts\n") {
					t.Fatal("Ovis timeout preload not synchronized")
				}
				if (scenario == "existing" || scenario == "dotenv") && !strings.Contains(string(data), "CUSTOM_SETTING=keep\n") {
					t.Fatal("unrelated settings lost")
				}
				info, err := os.Stat(file)
				if err != nil || info.Mode().Perm() != 0600 {
					t.Fatal("credential file must be 0600", err)
				}
			}
		})
	}
}
