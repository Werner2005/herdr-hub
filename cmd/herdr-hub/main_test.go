package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Werner2005/herdr-hub/internal/workload"
)

type fakeAPI struct {
	calls int
	call  func(string, string, any, any) error
}

func (f *fakeAPI) Do(_ context.Context, method, path string, input, output any) error {
	f.calls++
	return f.call(method, path, input, output)
}

func testCLI(t *testing.T, api *fakeAPI) (cli, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	c := cli{out: out, errOut: errOut, getenv: func(string) string { return "" }, cwd: t.TempDir()}
	c.newClient = func() (apiClient, error) {
		if api == nil {
			t.Fatal("unexpected network access")
		}
		return api, nil
	}
	return c, out, errOut
}

func TestDryRun(t *testing.T) {
	c, out, _ := testCLI(t, nil)
	err := c.run(context.Background(), []string{
		"run", "--dry-run", "--name", "demo", "-it", "--rm", "--cpu", "250m", "--memory", "1Gi",
		"-v", "data:/data:ro", "-v", "/scratch", "-e", "MESSAGE=a b",
		"--expose", "8080", "-w", "/app", "--entrypoint", "/bin/sh", "alpine:3.22", "-c", "printf '%s' \"$MESSAGE\"",
	})
	if err != nil {
		t.Fatal(err)
	}
	var spec workload.Spec
	if err := json.Unmarshal(out.Bytes(), &spec); err != nil {
		t.Fatal(err)
	}
	if !spec.Stdin || !spec.TTY || !spec.AutoRemove || spec.CPU != "250m" || spec.Memory != "1Gi" || spec.Env["MESSAGE"] != "a b" {
		t.Fatalf("incorrect request: %+v", spec)
	}
	if !reflect.DeepEqual(spec.Command, []string{"-c", "printf '%s' \"$MESSAGE\""}) || !reflect.DeepEqual(spec.Entrypoint, []string{"/bin/sh"}) {
		t.Fatalf("command arguments changed: %+v", spec)
	}
	if len(spec.Mounts) != 2 || !spec.Mounts[0].ReadOnly || spec.Mounts[1].Kind != "anonymous" {
		t.Fatalf("incorrect mounts: %+v", spec.Mounts)
	}
}

func TestRejectBeforeNetwork(t *testing.T) {
	for _, args := range [][]string{
		{"run", "--name", "demo", "--privileged", "alpine"},
		{"run", "--name", "demo", "-p", "8080:80", "alpine"},
		{"run", "--name", "../other", "-d", "alpine"},
		{"run", "--name", "demo", "-d", "-e", "COPY_HOST_SECRET", "alpine"},
		{"run", "--name", "demo", "-v", "data:/proc", "-d", "alpine"},
		{"run", "--name", "demo", "-it", "alpine"},
		{"run", "--name", "demo", "alpine"},
		{"run", "--name", "demo", "--dry-run"},
		{"exec", "-it", "demo", "sh"},
		{"exec", "demo"},
		{"rm", "../other"},
		{"rm", "demo", "ignored"},
		{"ps", "--all"},
		{"volume", "create", "data", "--size", "0Gi"},
		{"volume", "create", "data", "--size", "01Gi"},
		{"volume", "rm", "data", "ignored"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			c, _, _ := testCLI(t, nil)
			if c.run(context.Background(), args) == nil {
				t.Fatal("accepted invalid or unsupported command")
			}
		})
	}
}

func TestRunRequest(t *testing.T) {
	api := &fakeAPI{call: func(method, path string, input, output any) error {
		if method != "POST" || path != "/workloads" {
			t.Fatalf("unexpected endpoint: %s %s", method, path)
		}
		spec := input.(workload.Spec)
		if !reflect.DeepEqual(spec.Command, []string{"echo", "a b", "$(touch /tmp/unsafe)"}) {
			t.Fatal("command arguments were shell-expanded or changed")
		}
		*output.(*workload.Item) = workload.Item{Name: spec.Name, Status: "pending"}
		return nil
	}}
	c, out, _ := testCLI(t, api)
	if err := c.run(context.Background(), []string{"run", "-d", "--name", "demo", "alpine", "echo", "a b", "$(touch /tmp/unsafe)"}); err != nil {
		t.Fatal(err)
	}
	if api.calls != 1 || !strings.Contains(out.String(), `"pending"`) {
		t.Fatal("missing request or output")
	}
}

func TestExecPreservesArgumentsAndExitCode(t *testing.T) {
	api := &fakeAPI{call: func(method, path string, input, output any) error {
		if method != "POST" || path != "/workloads/database/exec" {
			t.Fatalf("unexpected endpoint %s %s", method, path)
		}
		if !reflect.DeepEqual(input.(workload.ExecRequest).Command, []string{"sh", "-c", "echo 'a b'; exit 7"}) {
			t.Fatal("exec arguments changed")
		}
		*output.(*workload.ExecResult) = workload.ExecResult{Stdout: "hello\n", Stderr: "warning\n", ExitCode: 7}
		return nil
	}}
	c, out, errOut := testCLI(t, api)
	err := c.run(context.Background(), []string{"exec", "database", "sh", "-c", "echo 'a b'; exit 7"})
	var exit exitError
	if !errors.As(err, &exit) || exit != 7 || out.String() != "hello\n" || errOut.String() != "warning\n" {
		t.Fatalf("unexpected exec result: %v, %q, %q", err, out, errOut)
	}
}

func TestSimpleCommands(t *testing.T) {
	for _, test := range []struct {
		args         []string
		method, path string
	}{
		{[]string{"ps"}, "GET", "/workloads"},
		{[]string{"inspect", "demo"}, "GET", "/workloads/demo"},
		{[]string{"rm", "demo"}, "DELETE", "/workloads/demo"},
		{[]string{"logs", "demo"}, "GET", "/workloads/demo/logs"},
		{[]string{"volume", "ls"}, "GET", "/volumes"},
		{[]string{"volume", "inspect", "data"}, "GET", "/volumes/data"},
		{[]string{"volume", "rm", "data"}, "DELETE", "/volumes/data"},
		{[]string{"volume", "create", "data", "--size", "5Gi"}, "POST", "/volumes"},
	} {
		t.Run(strings.Join(test.args, " "), func(t *testing.T) {
			api := &fakeAPI{call: func(method, path string, input, output any) error {
				if method != test.method || path != test.path {
					t.Fatalf("unexpected endpoint: %s %s", method, path)
				}
				if method == "POST" && input.(workload.Volume) != (workload.Volume{Name: "data", Size: "5Gi"}) {
					t.Fatal("incorrect volume request")
				}
				return nil
			}}
			c, _, _ := testCLI(t, api)
			if err := c.run(context.Background(), test.args); err != nil || api.calls != 1 {
				t.Fatalf("command failed: %v", err)
			}
		})
	}
}

func TestTokenFile(t *testing.T) {
	dir := t.TempDir()
	filename := filepath.Join(dir, "token")
	token := "hhw_" + strings.Repeat("0", 64)
	if err := os.WriteFile(filename, []byte(token+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := readToken(filename); err != nil || got != token {
		t.Fatalf("could not read private token: %v", err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(filename, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"", dir, link, filepath.Join(dir, "missing")} {
		if _, err := readToken(path); err == nil {
			t.Fatalf("accepted unsafe token file %q", path)
		}
	}
	if err := os.Chmod(filename, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := readToken(filename); err == nil {
		t.Fatal("accepted world-readable token")
	}
	if err := os.Chmod(filename, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte(strings.Repeat("x", 4097)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readToken(filename); err == nil {
		t.Fatal("accepted oversized token")
	}
}
