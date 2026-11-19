package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Werner2005/herdr-hub/internal/workload"
)

// parseMount resolves workspace paths locally for useful diagnostics. These
// checks are NOT authorization: the Hub must independently resolve them against
// its trusted parent PVC and defend against symlink changes at mount time.
func parseMount(raw, root, cwd string) (workload.Mount, error) {
	parts := strings.Split(raw, ":")
	m := workload.Mount{Kind: "anonymous"}
	switch len(parts) {
	case 1:
		m.Target = parts[0]
	case 2, 3:
		m.Source, m.Target = parts[0], parts[1]
		if m.Source == "" {
			return m, fmt.Errorf("volume source must not be empty")
		}
		m.Kind = "named"
		if len(parts) == 3 {
			switch parts[2] {
			case "ro":
				m.ReadOnly = true
			case "rw":
			default:
				return m, fmt.Errorf("volume mode must be ro or rw; other Docker mount options are unsupported")
			}
		}
		if filepath.IsAbs(m.Source) || strings.HasPrefix(m.Source, ".") || strings.Contains(m.Source, "/") {
			m.Kind = "workspace"
			source := m.Source
			if !filepath.IsAbs(source) {
				source = filepath.Join(cwd, source)
			}
			resolvedRoot, err := filepath.EvalSymlinks(root)
			if err != nil {
				return m, fmt.Errorf("cannot resolve authorized workspace root")
			}
			resolved, err := filepath.EvalSymlinks(source)
			if err != nil {
				return m, fmt.Errorf("workspace volume source must already exist")
			}
			relative, err := filepath.Rel(resolvedRoot, resolved)
			if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				return m, fmt.Errorf("volume source is outside the authorized workspace root")
			}
			info, err := os.Stat(resolved)
			if err != nil || !info.IsDir() {
				return m, fmt.Errorf("workspace volume source must be a directory; single-file mounts are not supported yet")
			}
			m.Source = filepath.ToSlash(relative)
		}
	default:
		return m, fmt.Errorf("use -v SOURCE:TARGET[:ro|rw] or -v TARGET")
	}
	return m, m.Validate()
}
