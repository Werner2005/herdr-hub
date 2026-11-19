// Package workload defines the experimental child-workload API contract.
// Validation here does not replace Hub-side authorization, quotas or admission.
package workload

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

const APIPrefix = "/api/workload/v1"

var namePattern = regexp.MustCompile(`^[a-z]([a-z0-9-]*[a-z0-9])?$`)
var envPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var cpuPattern = regexp.MustCompile(`^[0-9]+(m|\.[0-9]{1,3})?$`)
var memoryPattern = regexp.MustCompile(`^[0-9]+(Ki|Mi|Gi|Ti|Pi|Ei|k|M|G|T|P|E)?$`)

// Use an intentionally small positive quantity grammar. The backend still
// needs exact quantity parsing and quota comparison; this is not Kubernetes'
// complete quantity syntax (for example, exponents are not accepted).
func positiveQuantity(s string, pattern *regexp.Regexp) bool {
	if len(s) > 32 || !pattern.MatchString(s) {
		return false
	}
	for _, c := range s {
		if c >= '1' && c <= '9' {
			return true
		}
	}
	return false
}

func ValidName(s string) bool {
	return len(s) <= 40 && namePattern.MatchString(s)
}

// Spec deliberately has no owner, namespace, host settings, arbitrary PodSpec,
// Secret contents or Kubernetes credentials. The Hub must derive ownership from
// a scoped credential and resolve all storage sources itself.
type Spec struct {
	Name       string            `json:"name"`
	Image      string            `json:"image"`
	Command    []string          `json:"command,omitempty"`
	Entrypoint []string          `json:"entrypoint,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	Workdir    string            `json:"workdir,omitempty"`
	CPU        string            `json:"cpu"`
	Memory     string            `json:"memory"`
	Mounts     []Mount           `json:"mounts,omitempty"`
	Ports      []int             `json:"ports,omitempty"`
	Stdin      bool              `json:"stdin,omitempty"`
	TTY        bool              `json:"tty,omitempty"`
	AutoRemove bool              `json:"auto_remove,omitempty"`
}

// Mount.Source is a group-local volume name or a path relative to the parent's
// authorized workspace root. It is NEVER a node path or an arbitrary PVC name.
type Mount struct {
	Kind     string `json:"kind"` // named, workspace, anonymous
	Source   string `json:"source,omitempty"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only,omitempty"`
}

type Item struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Status   string `json:"status"`
	Error    string `json:"error,omitempty"`
	Hostname string `json:"hostname,omitempty"`
	ExitCode *int   `json:"exit_code,omitempty"`
	Spec     Spec   `json:"spec"`
}

type Volume struct {
	Name string `json:"name"`
	Size string `json:"size"`
}

type ExecRequest struct {
	Command []string `json:"command"`
}

// ExecResult is the bounded, non-interactive exec response. Interactive exec
// needs a separate streaming protocol; it must not be emulated with a shell.
type ExecResult struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}

func validTarget(s string) bool {
	if !strings.HasPrefix(s, "/") || path.Clean(s) != s || s == "/" || strings.ContainsRune(s, 0) {
		return false
	}
	for _, reserved := range []string{"/proc", "/sys", "/dev", "/var/run/secrets/kubernetes.io"} {
		if s == reserved || strings.HasPrefix(s, reserved+"/") {
			return false
		}
	}
	return true
}

func (m Mount) Validate() error {
	if !validTarget(m.Target) {
		return fmt.Errorf("mount target must be a clean absolute path outside reserved system paths")
	}
	switch m.Kind {
	case "named":
		if !ValidName(m.Source) {
			return fmt.Errorf("named volume must be a lowercase DNS label, at most 40 characters")
		}
	case "workspace":
		if m.Source == "" || strings.ContainsRune(m.Source, 0) || path.IsAbs(m.Source) || path.Clean(m.Source) != m.Source || m.Source == ".." || strings.HasPrefix(m.Source, "../") {
			return fmt.Errorf("workspace source must stay within the authorized workspace root")
		}
	case "anonymous":
		if m.Source != "" {
			return fmt.Errorf("anonymous volume cannot specify a source")
		}
	default:
		return fmt.Errorf("unsupported mount kind %q", m.Kind)
	}
	return nil
}

// Validate checks structure and basic quantity syntax. The server must additionally parse quantities,
// enforce finite resource limits and budgets, validate images and authorize
// mounts (including race-safe symlink resolution).
func (s Spec) Validate() error {
	if !ValidName(s.Name) {
		return fmt.Errorf("name must be a lowercase DNS label, at most 40 characters")
	}
	if s.Image == "" || len(s.Image) > 512 || strings.ContainsAny(s.Image, " \t\r\n\x00") || strings.HasPrefix(s.Image, "-") {
		return fmt.Errorf("provide a container image reference")
	}
	if !positiveQuantity(s.CPU, cpuPattern) || !positiveQuantity(s.Memory, memoryPattern) {
		return fmt.Errorf("positive CPU (e.g. 500m or 0.5) and memory (e.g. 512Mi) limits are required")
	}
	if s.Workdir != "" && (!strings.HasPrefix(s.Workdir, "/") || path.Clean(s.Workdir) != s.Workdir || strings.ContainsRune(s.Workdir, 0)) {
		return fmt.Errorf("working directory must be a clean absolute path")
	}
	if s.TTY && !s.Stdin {
		return fmt.Errorf("TTY requires stdin")
	}
	if len(s.Command) > 256 || len(s.Entrypoint) > 256 || len(s.Env) > 128 || len(s.Mounts) > 32 || len(s.Ports) > 64 {
		return fmt.Errorf("workload specification exceeds field count limits")
	}
	for _, args := range [][]string{s.Command, s.Entrypoint} {
		for _, arg := range args {
			if len(arg) > 8192 || strings.ContainsRune(arg, 0) {
				return fmt.Errorf("invalid command argument")
			}
		}
	}
	for k, v := range s.Env {
		if !envPattern.MatchString(k) || len(v) > 8192 || strings.ContainsRune(v, 0) {
			return fmt.Errorf("invalid environment assignment")
		}
	}
	for i, m := range s.Mounts {
		if err := m.Validate(); err != nil {
			return err
		}
		for _, previous := range s.Mounts[:i] {
			if m.Target == previous.Target || strings.HasPrefix(m.Target, previous.Target+"/") || strings.HasPrefix(previous.Target, m.Target+"/") {
				return fmt.Errorf("mount targets must not overlap")
			}
		}
	}
	seen := map[int]bool{}
	for _, port := range s.Ports {
		if port < 1 || port > 65535 || seen[port] {
			return fmt.Errorf("ports must be unique integers between 1 and 65535")
		}
		seen[port] = true
	}
	return nil
}
