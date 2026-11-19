package workload

import (
	"strings"
	"testing"
)

func TestSpecValidation(t *testing.T) {
	valid := func() Spec {
		return Spec{Name: "demo", Image: "alpine:3.22", CPU: "500m", Memory: "512Mi"}
	}
	if err := valid().Validate(); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		change func(*Spec)
	}{
		{"owner-like-name", func(s *Spec) { s.Name = "../other" }},
		{"image-control", func(s *Spec) { s.Image = "alpine\ninjected" }},
		{"no-cpu", func(s *Spec) { s.CPU = "" }},
		{"zero-cpu", func(s *Spec) { s.CPU = "0m" }},
		{"negative-cpu", func(s *Spec) { s.CPU = "-1" }},
		{"sub-millicore-cpu", func(s *Spec) { s.CPU = "0.0001" }},
		{"zero-memory", func(s *Spec) { s.Memory = "0Gi" }},
		{"invalid-memory", func(s *Spec) { s.Memory = "unlimited" }},
		{"relative-workdir", func(s *Spec) { s.Workdir = "../root" }},
		{"unclean-workdir", func(s *Spec) { s.Workdir = "/app/../root" }},
		{"tty-without-stdin", func(s *Spec) { s.TTY = true }},
		{"command-nul", func(s *Spec) { s.Command = []string{"echo", "\x00"} }},
		{"long-argument", func(s *Spec) { s.Command = []string{strings.Repeat("a", 8193)} }},
		{"bad-env", func(s *Spec) { s.Env = map[string]string{"BAD=KEY": "value"} }},
		{"duplicate-port", func(s *Spec) { s.Ports = []int{80, 80} }},
		{"invalid-port", func(s *Spec) { s.Ports = []int{65536} }},
		{"overlapping-mounts", func(s *Spec) {
			s.Mounts = []Mount{{Kind: "named", Source: "one", Target: "/app"}, {Kind: "named", Source: "two", Target: "/app/data"}}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := valid()
			test.change(&s)
			if s.Validate() == nil {
				t.Fatal("accepted invalid specification")
			}
		})
	}
}

func TestQuantityGrammar(t *testing.T) {
	for _, cpu := range []string{"1", "500m", "0.5", "0.001"} {
		for _, memory := range []string{"512Mi", "1Gi", "1024", "500M"} {
			s := Spec{Name: "demo", Image: "alpine", CPU: cpu, Memory: memory}
			if err := s.Validate(); err != nil {
				t.Fatalf("%s/%s: %v", cpu, memory, err)
			}
		}
	}
}

func TestMountValidation(t *testing.T) {
	for _, m := range []Mount{
		{Kind: "named", Source: "db-data", Target: "/data"},
		{Kind: "workspace", Source: "workspace/project", Target: "/app", ReadOnly: true},
		{Kind: "workspace", Source: ".", Target: "/app"},
		{Kind: "anonymous", Target: "/scratch"},
	} {
		if err := m.Validate(); err != nil {
			t.Fatalf("%+v: %v", m, err)
		}
	}
	for _, m := range []Mount{
		{Kind: "hostPath", Source: "/etc", Target: "/data"},
		{Kind: "named", Source: "../other", Target: "/data"},
		{Kind: "named", Source: "data", Target: "/"},
		{Kind: "named", Source: "data", Target: "/proc/sys"},
		{Kind: "named", Source: "data", Target: "/var/run/secrets/kubernetes.io/serviceaccount"},
		{Kind: "anonymous", Source: "data", Target: "/data"},
		{Kind: "workspace", Source: "../outside", Target: "/data"},
		{Kind: "workspace", Source: "/etc", Target: "/data"},
		{Kind: "workspace", Source: "project/../../etc", Target: "/data"},
		{Kind: "workspace", Source: "project\x00", Target: "/data"},
		{Kind: "anonymous", Target: "/app/../proc"},
		{Kind: "anonymous", Target: "relative"},
	} {
		if m.Validate() == nil {
			t.Fatalf("accepted unsafe mount %+v", m)
		}
	}
}
