package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Exercise the real home initializer with isolated fixture paths. Installing a
// new bundled skill must work for existing homes without replacing user files.
func TestBundledWorkspaceSkills(t *testing.T) {
	script, err := os.ReadFile("../docker/init-home.sh")
	if err != nil {
		t.Fatal(err)
	}
	skills, err := os.ReadDir("../docker/skills")
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"fresh", "existing", "symlink"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			home, bundle, skel := filepath.Join(root, "home"), filepath.Join(root, "bundle"), filepath.Join(root, "skel")
			write := func(name, text string) {
				t.Helper()
				if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(name, []byte(text), 0644); err != nil {
					t.Fatal(err)
				}
			}
			for _, entry := range skills {
				if !entry.IsDir() {
					continue
				}
				content, err := os.ReadFile(filepath.Join("../docker/skills", entry.Name(), "SKILL.md"))
				if err != nil {
					t.Fatal(err)
				}
				write(filepath.Join(bundle, "skills", entry.Name(), "SKILL.md"), string(content))
			}
			for _, name := range []string{".profile", ".bashrc", ".bash_logout"} {
				write(filepath.Join(skel, name), "# fixture shell profile\n")
			}
			write(filepath.Join(bundle, "startup.sh"), "# fixture startup\n")
			for _, name := range []string{"codex", "opencode", "gemini", "claude", "uv", "uvx", "herdr", "bun"} {
				write(filepath.Join(bundle, "home-bin", name), "# fixture tool\n")
			}
			destination := filepath.Join(home, ".agents", "skills", "herdr-hub", "SKILL.md")
			oldSkill := filepath.Join(home, ".agents", "skills", "permanent-software", "SKILL.md")
			if scenario != "fresh" {
				write(oldSkill, "# existing user-edited software skill\n")
			}
			if scenario == "symlink" {
				if err := os.MkdirAll(filepath.Dir(destination), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("missing-user-skill.md", destination); err != nil {
					t.Fatal(err)
				}
			}
			text := strings.ReplaceAll(string(script), "/usr/local/share/herdr/", bundle+"/")
			text = strings.ReplaceAll(text, "/etc/skel/", skel+"/")
			run := func() {
				t.Helper()
				cmd := exec.Command("bash", "-c", text)
				cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + home}
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("home initialization failed: %v: %s", err, out)
				}
			}
			run()
			if scenario == "symlink" {
				target, err := os.Readlink(destination)
				if err != nil || target != "missing-user-skill.md" {
					t.Fatal("initializer replaced an existing user symlink", err)
				}
			} else {
				got, err := os.ReadFile(destination)
				want, readErr := os.ReadFile("../docker/skills/herdr-hub/SKILL.md")
				if err != nil || readErr != nil || string(got) != string(want) {
					t.Fatal("bundled workload skill was not installed", err, readErr)
				}
				write(destination, "# customized workload skill\n")
			}
			run()
			if scenario != "fresh" {
				got, err := os.ReadFile(oldSkill)
				if err != nil || string(got) != "# existing user-edited software skill\n" {
					t.Fatal("initializer overwrote an existing unrelated skill", err)
				}
			}
			if scenario != "symlink" {
				got, err := os.ReadFile(destination)
				if err != nil || string(got) != "# customized workload skill\n" {
					t.Fatal("repeated startup overwrote the customized workload skill", err)
				}
			} else if target, err := os.Readlink(destination); err != nil || target != "missing-user-skill.md" {
				t.Fatal("repeated startup overwrote user symlink", err)
			}
		})
	}
}
