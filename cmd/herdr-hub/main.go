// herdr-hub is the experimental CLI for Hub-managed child workloads.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/Werner2005/herdr-hub/internal/hubclient"
	"github.com/Werner2005/herdr-hub/internal/workload"
)

const help = `herdr-hub: experimental Hub-managed workload CLI

This is a client/API-contract foundation. The current Hub has no workload API.
Only run --dry-run works without the future scoped workload backend.

Commands:
  run --name NAME [OPTIONS] IMAGE [COMMAND [ARG...]]
  ps
  inspect NAME
  rm NAME
  logs NAME                         bounded log snapshot, not follow
  exec NAME COMMAND [ARG...]        bounded non-interactive exec
  volume create NAME --size 5Gi
  volume ls
  volume inspect NAME
  volume rm NAME

Run options (before IMAGE):
  -d, --detach       return after creation (required for live run in this slice)
  --cpu 500m        finite CPU limit; server must enforce group budget
  --memory 512Mi    finite memory limit
  -e KEY=VALUE      repeatable; avoid credentials, use future secret references
  -v SRC:DST[:ro|rw] repeatable named volume or authorized workspace directory
  -v /DST           anonymous volume
  -w /DIR           working directory
  --entrypoint CMD  override image entrypoint with one executable
  --expose PORT     repeatable private TCP service port, not public publishing
  --rm              request anonymous-volume cleanup after completion
  -i, -t, -it       describe stdin/TTY in dry runs; streaming not implemented yet
  --dry-run         print validated request JSON; does not contact Hub

Configuration:
  HERDR_HUB_URL             HTTPS origin
  HERDR_HUB_TOKEN_FILE      regular private file (0600) with scoped hhw_ token
  HERDR_HUB_WORKSPACE_ROOT  local mount-validation root (default /home/dev);
                           never grants server-side mount permissions

Privileged mode, host mounts/networking, -p and arbitrary Kubernetes options
are intentionally unsupported. Existing Hub admin/web tokens are not accepted.
`

type stringsFlag []string

func (s *stringsFlag) String() string { return strings.Join(*s, ",") }
func (s *stringsFlag) Set(v string) error {
	*s = append(*s, v)
	return nil
}

type apiClient interface {
	Do(context.Context, string, string, any, any) error
}

type cli struct {
	out, errOut io.Writer
	getenv      func(string) string
	cwd         string
	newClient   func() (apiClient, error)
}

type exitError int

func (e exitError) Error() string { return fmt.Sprintf("remote command exited with status %d", e) }

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "herdr-hub: cannot determine working directory")
		os.Exit(1)
	}
	c := cli{out: os.Stdout, errOut: os.Stderr, getenv: os.Getenv, cwd: cwd}
	c.newClient = func() (apiClient, error) {
		token, err := readToken(os.Getenv("HERDR_HUB_TOKEN_FILE"))
		if err != nil {
			return nil, err
		}
		return hubclient.New(os.Getenv("HERDR_HUB_URL"), token, nil)
	}
	if err = c.run(ctx, os.Args[1:]); err != nil {
		var status exitError
		if errors.As(err, &status) {
			os.Exit(int(status))
		}
		fmt.Fprintln(os.Stderr, "herdr-hub:", err)
		os.Exit(1)
	}
}

func readToken(filename string) (string, error) {
	if filename == "" {
		return "", fmt.Errorf("set HERDR_HUB_TOKEN_FILE to a scoped workload credential file")
	}
	info, err := os.Lstat(filename)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return "", fmt.Errorf("workload token file must be a regular, non-symlink file accessible only to its owner (0600)")
	}
	f, err := os.Open(filename)
	if err != nil {
		return "", fmt.Errorf("cannot read workload token file")
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) || opened.Mode().Perm()&0077 != 0 {
		return "", fmt.Errorf("workload token file changed while opening")
	}
	b, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil || len(b) > 4096 {
		return "", fmt.Errorf("invalid workload token file")
	}
	return strings.TrimSpace(string(b)), nil
}

func (c cli) flags(name string) *flag.FlagSet {
	f := flag.NewFlagSet(name, flag.ContinueOnError)
	f.SetOutput(c.errOut)
	return f
}

func (c cli) print(v any) error {
	enc := json.NewEncoder(c.out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func (c cli) request(ctx context.Context, method, endpoint string, input, output any) error {
	client, err := c.newClient()
	if err != nil {
		return err
	}
	return client.Do(ctx, method, endpoint, input, output)
}

func (c cli) run(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		_, err := io.WriteString(c.out, help)
		return err
	}
	switch args[0] {
	case "run":
		return c.runWorkload(ctx, args[1:])
	case "volume":
		return c.volume(ctx, args[1:])
	case "ps":
		if len(args) != 1 {
			return fmt.Errorf("usage: herdr-hub ps")
		}
		var items []workload.Item
		if err := c.request(ctx, "GET", "/workloads", nil, &items); err != nil {
			return err
		}
		return c.print(items)
	case "inspect", "rm", "logs", "exec":
		if len(args) < 2 || !workload.ValidName(args[1]) {
			return fmt.Errorf("provide a valid workload name")
		}
		endpoint := "/workloads/" + args[1]
		if args[0] == "exec" {
			if len(args) < 3 {
				return fmt.Errorf("usage: herdr-hub exec NAME COMMAND [ARG...] (interactive exec is not implemented yet)")
			}
			command := args[2:]
			// Reuse command validation without constructing a shell string.
			if err := (workload.Spec{Name: "exec", Image: "exec", CPU: "1", Memory: "1Mi", Command: command}).Validate(); err != nil {
				return err
			}
			var result workload.ExecResult
			if err := c.request(ctx, "POST", endpoint+"/exec", workload.ExecRequest{Command: command}, &result); err != nil {
				return err
			}
			if result.ExitCode < 0 || result.ExitCode > 255 {
				return fmt.Errorf("invalid remote exit code")
			}
			if _, err := io.WriteString(c.out, result.Stdout); err != nil {
				return err
			}
			if _, err := io.WriteString(c.errOut, result.Stderr); err != nil {
				return err
			}
			if result.ExitCode != 0 {
				return exitError(result.ExitCode)
			}
			return nil
		}
		if len(args) != 2 {
			return fmt.Errorf("usage: herdr-hub %s NAME", args[0])
		}
		if args[0] == "rm" {
			return c.request(ctx, "DELETE", endpoint, nil, nil)
		}
		if args[0] == "logs" {
			var result struct {
				Logs string `json:"logs"`
			}
			if err := c.request(ctx, "GET", endpoint+"/logs", nil, &result); err != nil {
				return err
			}
			_, err := io.WriteString(c.out, result.Logs)
			return err
		}
		var item workload.Item
		if err := c.request(ctx, "GET", endpoint, nil, &item); err != nil {
			return err
		}
		return c.print(item)
	default:
		return fmt.Errorf("unknown command %q; use herdr-hub --help", args[0])
	}
}

func (c cli) runWorkload(ctx context.Context, args []string) error {
	f := c.flags("run")
	var spec workload.Spec
	var envs, mounts, ports stringsFlag
	var dryRun, detach, it bool
	var entrypoint string
	f.StringVar(&spec.Name, "name", "", "workload name")
	f.StringVar(&spec.CPU, "cpu", "500m", "finite CPU limit")
	f.StringVar(&spec.Memory, "memory", "512Mi", "finite memory limit")
	f.StringVar(&spec.Workdir, "w", "", "working directory")
	f.StringVar(&spec.Workdir, "workdir", "", "working directory")
	f.StringVar(&entrypoint, "entrypoint", "", "entrypoint executable")
	f.BoolVar(&dryRun, "dry-run", false, "print request without contacting Hub")
	f.BoolVar(&detach, "d", false, "detached")
	f.BoolVar(&detach, "detach", false, "detached")
	f.BoolVar(&spec.AutoRemove, "rm", false, "remove after completion")
	f.BoolVar(&spec.Stdin, "i", false, "stdin (dry run only)")
	f.BoolVar(&spec.Stdin, "interactive", false, "stdin (dry run only)")
	f.BoolVar(&spec.TTY, "t", false, "TTY (dry run only)")
	f.BoolVar(&spec.TTY, "tty", false, "TTY (dry run only)")
	f.BoolVar(&it, "it", false, "stdin and TTY (dry run only)")
	f.Var(&envs, "e", "KEY=VALUE")
	f.Var(&envs, "env", "KEY=VALUE")
	f.Var(&mounts, "v", "SOURCE:TARGET[:ro|rw]")
	f.Var(&mounts, "volume", "SOURCE:TARGET[:ro|rw]")
	f.Var(&ports, "expose", "private TCP port")
	if err := f.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if f.NArg() < 1 {
		return fmt.Errorf("usage: herdr-hub run --name NAME [OPTIONS] IMAGE [COMMAND [ARG...]]")
	}
	spec.Image, spec.Command = f.Arg(0), f.Args()[1:]
	if entrypoint != "" {
		spec.Entrypoint = []string{entrypoint}
	}
	if it {
		spec.Stdin, spec.TTY = true, true
	}
	spec.Env = map[string]string{}
	for _, assignment := range envs {
		key, value, ok := strings.Cut(assignment, "=")
		if !ok {
			return fmt.Errorf("environment options require KEY=VALUE; implicit host environment copying is unsupported")
		}
		spec.Env[key] = value
	}
	root := c.getenv("HERDR_HUB_WORKSPACE_ROOT")
	if root == "" {
		root = "/home/dev"
	}
	for _, raw := range mounts {
		m, err := parseMount(raw, root, c.cwd)
		if err != nil {
			return err
		}
		spec.Mounts = append(spec.Mounts, m)
	}
	for _, raw := range ports {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return fmt.Errorf("--expose requires a private TCP port number, not host-port publishing")
		}
		spec.Ports = append(spec.Ports, n)
	}
	if err := spec.Validate(); err != nil {
		return err
	}
	if dryRun {
		return c.print(spec)
	}
	if !detach || spec.Stdin || spec.TTY {
		return fmt.Errorf("live run currently requires -d without stdin/TTY; attach streaming is not implemented yet (use --dry-run to inspect requests)")
	}
	var item workload.Item
	if err := c.request(ctx, "POST", "/workloads", spec, &item); err != nil {
		return err
	}
	return c.print(item)
}

func (c cli) volume(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: herdr-hub volume create|ls|inspect|rm")
	}
	if args[0] == "ls" && len(args) == 1 {
		var items []workload.Volume
		if err := c.request(ctx, "GET", "/volumes", nil, &items); err != nil {
			return err
		}
		return c.print(items)
	}
	if len(args) < 2 || !workload.ValidName(args[1]) {
		return fmt.Errorf("provide a valid volume name")
	}
	switch args[0] {
	case "create":
		f := c.flags("volume create")
		size := f.String("size", "", "positive whole Gi storage size")
		if err := f.Parse(args[2:]); err != nil {
			return err
		}
		number := strings.TrimSuffix(*size, "Gi")
		n, err := strconv.Atoi(number)
		if err != nil || *size != strconv.Itoa(n)+"Gi" || n < 1 || n > 1048576 || f.NArg() != 0 {
			return fmt.Errorf("use volume create NAME --size with a positive whole Gi value, e.g. --size 5Gi")
		}
		var item workload.Volume
		if err := c.request(ctx, "POST", "/volumes", workload.Volume{Name: args[1], Size: *size}, &item); err != nil {
			return err
		}
		return c.print(item)
	case "inspect":
		if len(args) != 2 {
			return fmt.Errorf("usage: herdr-hub volume inspect NAME")
		}
		var item workload.Volume
		if err := c.request(ctx, "GET", "/volumes/"+args[1], nil, &item); err != nil {
			return err
		}
		return c.print(item)
	case "rm":
		if len(args) != 2 {
			return fmt.Errorf("usage: herdr-hub volume rm NAME")
		}
		return c.request(ctx, "DELETE", "/volumes/"+args[1], nil, nil)
	default:
		return fmt.Errorf("unknown volume command")
	}
}
