package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestOvisStartupSelection(t *testing.T) {
	entrypoint, err := os.ReadFile("../docker/entrypoint.sh")
	if err != nil {
		t.Fatal(err)
	}
	text := string(entrypoint)
	start, end := strings.Index(text, "herdr-install-node\n"), strings.Index(text, "# Plugin credential configuration")
	initEnd := strings.Index(text, "herdr-init-home\n")
	if start < 0 || end < start || initEnd < 0 {
		t.Fatal("startup blocks missing")
	}
	for _, scenario := range []string{"fresh", "legacy", "ovis", "both", "ovis-token", "token-precedence"} {
		t.Run(scenario, func(t *testing.T) {
			home := t.TempDir()
			bin := filepath.Join(home, "bin")
			if err := os.Mkdir(bin, 0755); err != nil {
				t.Fatal(err)
			}
			write := func(name, content string) {
				t.Helper()
				if err := os.WriteFile(name, []byte(content), 0755); err != nil {
					t.Fatal(err)
				}
			}
			plugins := ""
			if scenario == "legacy" || scenario == "both" {
				plugins += "devswha.herdr-web-ui installed\n"
				if err := os.MkdirAll(filepath.Join(home, ".config/herdr-web-ui"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "ovis" || scenario == "both" {
				plugins += "devswha.ovis installed\n"
			}
			write(filepath.Join(bin, "herdr"), "#!/bin/sh\ncase \"$*\" in\n'--version') echo 'herdr 0.9.3';;\n'plugin list') printf '%s' \"$PLUGINS\";;\n*) exit 1;;\nesac\n")
			write(filepath.Join(bin, "bun"), "#!/bin/sh\necho 1.4.3\n")
			write(filepath.Join(bin, "herdr-install-node"), "#!/bin/sh\nexit 0\n")
			write(filepath.Join(home, "ovis-ref"), "pinned-ref\n")
			write(filepath.Join(home, "ovis-install.sh"), "#!/bin/sh\n[ \"$OVIS_REF\" = pinned-ref ] || exit 1\necho installer-ran\n")
			script := text[:initEnd] + strings.ReplaceAll(text[start:end], "/usr/local/share/herdr/", home+"/") + "printf '%s\\n' \"$OVIS_TOKEN\" \"$OVIS_STATE_DIR\"\n"
			cmd := exec.Command("bash", "-c", script)
			// A clean environment also checks the legacy token alias used by Hub.
			cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "HOME=" + home, "PLUGINS=" + plugins, "HOST=0.0.0.0", "PORT=7317", "HERDR_WEB_TOKEN=test-token"}
			token := "test-token"
			if scenario == "ovis-token" {
				cmd.Env = cmd.Env[:len(cmd.Env)-1]
			}
			if scenario == "ovis-token" || scenario == "token-precedence" {
				token = "ovis-test-token"
				cmd.Env = append(cmd.Env, "OVIS_TOKEN="+token)
			}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("startup selection: %v: %s", err, out)
			}
			if strings.Contains(string(out), "installer-ran") != (scenario != "ovis") {
				t.Fatalf("unexpected installer selection: %s", out)
			}
			state := filepath.Join(home, ".config/ovis")
			if scenario == "legacy" || scenario == "both" {
				state = filepath.Join(home, ".config/herdr-web-ui")
			}
			if !strings.HasSuffix(string(out), token+"\n"+state+"\n") {
				t.Fatalf("token/state selection: %s", out)
			}
		})
	}
}
