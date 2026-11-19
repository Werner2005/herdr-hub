# Experimental `herdr-hub` workload CLI

## Status and scope

The Go CLI and default-off Hub backend implement scoped workload creation,
inspection, removal, private networking, PVCs, log snapshots and bounded
non-interactive exec. The workspace image includes the `herdr-hub` CLI.
Do not put an administrator or workspace web token into this CLI.

Workspace home initialization also installs a bundled **`herdr-hub` agent skill**
at `~/.agents/skills/herdr-hub/SKILL.md` when missing. It explains commands,
authentication, private connectivity, volumes, cleanup and current limitations.
It is added to existing persistent homes on startup with the new image, without
overwriting user-edited skills. Restart the coding agent after installation so it
discovers the skill. Installing the skill does not enable API access or deploy
anything automatically.

The backend uses the existing namespace with parent-scoped database reservations,
immutable random resource IDs and Kubernetes limits. It does not provision
namespace-per-group quotas. Integration tests use dedicated PostgreSQL and a
simulated Kubernetes API (including real WebSocket exec framing); production
admission, storage and CNI behavior still need isolated-cluster validation.
Source changes do not enable the feature or deploy anything.

## Enable and authorize (administrator)

Enable `workloads.enabled: true` in Helm values. Enforced NetworkPolicy is required;
Helm refuses `networkPolicy.enabled: false` with this feature enabled.
Enabling adds namespace Pod create/get/delete, Pod log get, and Pod exec get/create
permissions to the Hub, and permits workspace-to-Gateway HTTPS TCP 443 traffic. It does not give
workspaces Kubernetes credentials.

Through the normal authenticated Hub API:

1. `PUT /api/workspaces/NAME/workload-access` (admin only):
   ```json
   {"enabled":true,"max_workloads":5,"cpu":"2","memory":"4Gi","storage_gi":20,"max_volumes":10}
   ```
2. `POST /api/workspaces/NAME/workload-token` (owner or admin). Store the returned
   `token` in a regular mode-0600 file inside the workspace. It expires after
   24 hours and replaces any previous credential. No automatic Secret injection
   into an existing workspace is performed.
3. Set `HERDR_HUB_URL` to the Hub's HTTPS origin and `HERDR_HUB_TOKEN_FILE` to
   that private file. The gateway must present a trusted certificate. The CNI must
   match the post-DNAT gateway pods; outside-cluster proxies may need an explicit
   administrator-managed egress rule. The gateway allowance is L3/L4: it permits
   all hostnames on that gateway's port 443, not just the Hub hostname.
   For a private CA, install its certificate in the workspace trust store; do not
   disable certificate verification.

`DELETE .../workload-token` revokes access. Disabling the budget revokes its token
and pauses pending launches, but does not kill running children or delete data.
Cleanup and expiry reconciliation continue. Remove all children and child volumes
before disabling the Helm feature, transferring, deleting, stopping or rotating
the parent's credentials. These parent operations refuse retained child resources.
Ownership transfer revokes scoped credentials and removes the old budget.
To manually clean up after disabling a budget, an administrator must re-enable it
and issue a new scoped credential.

With access configured:

```sh
herdr-hub run -d --name web --expose 8080 \
  python:3.13-alpine python -m http.server 8080
herdr-hub inspect web                 # returns the private Service hostname
herdr-hub exec web python -c 'print("hello from child")'
herdr-hub logs web
herdr-hub rm web
```

## Build and local validation

From the repository root, using Go 1.27.4 or newer (the patched toolchain in
`go.mod`, Docker builders and CI):

```sh
go build -o herdr-hub ./cmd/herdr-hub
go test ./cmd/herdr-hub ./internal/...
./herdr-hub --help
./herdr-hub run --dry-run --name database \
  --cpu 500m --memory 512Mi \
  -v db-data:/var/lib/postgresql/data postgres:17
```

The binary is named `herdr-hub`, not `herdr`. No Docker daemon is used.
This is not a Docker API implementation and does not make Compose or
Testcontainers work unchanged.

Options precede IMAGE. Remaining arguments are preserved as an argument array,
including spaces and shell metacharacters. They are never implicitly executed
through a shell. `--entrypoint /bin/sh IMAGE -c '...'` explicitly requests one.
`--name` is required in this first slice.

`run` supports `-e/--env KEY=VALUE`, `-v/--volume`, `-w/--workdir`, `--entrypoint`,
`--cpu`, `--memory`, `--expose PORT`, `--rm`, and `-d/--detach`. Limits default to
500m CPU and 512Mi memory. The contract checks a restricted positive quantity
syntax (CPU cores with up to three decimal places or whole millicores, memory
as whole bytes or Ki/Mi/Gi/Ti/Pi/Ei/k/M/G/T/P/E units).
The backend must parse quantities exactly and enforce administrator-bounded limits.
`--expose` declares private TCP service ports, not public or node-port exposure.
`-p`, privileged mode, host devices/network/PID/IPC and arbitrary PodSpec options
are deliberately rejected.

`-i`, `-t` and `-it` can describe a future interactive session in a dry run.
Live requests currently require `-d` without stdin/TTY because attach streaming
has not been implemented.

**Dry-run output contains supplied environment values and commands.** Do not
use it to print secrets into shared terminals or CI logs.

## Volumes: `-v SOURCE:TARGET[:ro|rw]`

```sh
# Named volume; lifecycle independent of containers.
./herdr-hub run --dry-run --name db -v db-data:/data postgres:17

# Directory in the authorized parent workspace; same files, different path.
# The CLI root is a diagnostic setting, not an authorization boundary.
HERDR_HUB_WORKSPACE_ROOT="$HOME" ./herdr-hub run --dry-run \
  --name test -v ./project:/app:ro -w /app node:24 npm test

# Anonymous volume.
./herdr-hub run --dry-run --name scratch -v /scratch alpine:3.22 sleep 60
```

- Named sources are group-local lowercase DNS labels, at most 40 characters.
- Sources beginning with `.` or `/`, or containing `/`, are directory sources.
  They must already exist under the parent's authorized workspace root.
- Relative directories are resolved against the CLI working directory. Absolute
  directories are allowed only under that same root. Symlink escapes are rejected
  locally. The wire contract sends only a root-relative path, never a node path.
- Default local validation root is `/home/dev`; override with
  `HERDR_HUB_WORKSPACE_ROOT` for diagnostics. A caller changing this environment
  variable must never change what the Hub authorizes.
- Directory sources are independently resolved against the parent's home PVC.
  Container-layer files and directories are not shared automatically.
- The CLI supports directories, not individual file mounts. The Hub uses
  kubelet's secure `subPath` handling; symlink sources may be rejected at startup.
  It never resolves a client path on the Hub/node filesystem.
- Targets must be clean absolute paths, not `/` or reserved kernel/service-account
  directories. Duplicate and overlapping mount targets are rejected.
- Mode defaults to `rw`; `ro` is explicit. SELinux labels, propagation options
  and other Docker-specific suffixes are unsupported and rejected.
- Anonymous volumes are dedicated budget-accounted PVCs. Missing named volumes
  and each anonymous mount reserve 1Gi by default. Use `volume create --size`
  for larger named volumes. Anonymous volumes are removed with their workload;
  `--rm` also removes completed workloads asynchronously. Named volumes survive.
  All child Pods are scheduled on the parent's node for RWO compatibility;
  storage backend restrictions may still prevent a mount.

Volume commands:

```sh
herdr-hub volume create db-data --size 5Gi
herdr-hub volume ls
herdr-hub volume inspect db-data
herdr-hub volume rm db-data
```

The backend refuses removal of referenced volumes and reserves storage before
provisioning. Once provisioned, a missing PVC is not recreated automatically.
`rm -v` is not implemented; explicitly remove named volumes with `volume rm`.

## HTTP contract

The API prefix is `/api/workload/v1`. The parent workspace and owner are always
derived from a scoped credential, never supplied in requests.

| Method | Endpoint | Body / result |
| --- | --- | --- |
| POST | `/workloads` | `workload.Spec` / `workload.Item` (201) |
| GET | `/workloads` | `[]workload.Item` |
| GET | `/workloads/{name}` | `workload.Item` |
| DELETE | `/workloads/{name}` | 204 after confirmed cleanup |
| GET | `/workloads/{name}/logs` | `{"logs":"..."}` bounded snapshot |
| POST | `/workloads/{name}/exec` | `{"command":["sh","-c","..."]}` / `workload.ExecResult` |
| POST | `/volumes` | `workload.Volume` / `workload.Volume` (201) |
| GET | `/volumes` | `[]workload.Volume` |
| GET | `/volumes/{name}` | `workload.Volume` |
| DELETE | `/volumes/{name}` | 204 after confirmed cleanup |

Types are defined in `internal/workload`; the served specification is
`app/openapi.json`. Contract tests check all operations, scoped authentication and
JSON property parity against the Go types. The backend rejects unknown fields and
trailing JSON, parses quantities exactly and checks combined reservations while
holding the Hub's cancellation-aware mutation lock.

CLI configuration:

```sh
export HERDR_HUB_URL=https://hub.apps.example.com
export HERDR_HUB_TOKEN_FILE=/run/herdr-hub/workload-token
```

Credentials use a distinct `hhw_` prefix plus 32 random bytes encoded as 64 hex
characters. Only credential hashes are stored in PostgreSQL. Files must be regular non-symlink
files without group/other access. There is no command-line token option.
The HTTPS client uses normal CA verification, refuses redirects and limits
requests to 64 KiB and decoded responses to 4 MiB. HTTP error bodies are not
echoed because proxy errors may contain secrets. JSON output is used for object
commands; logs and exec output are deliberately raw, like terminal output, and
may contain untrusted terminal control sequences.

Non-interactive `exec` preserves stdout/stderr and exit codes 0–255:

```sh
herdr-hub exec database psql -U postgres
herdr-hub exec database sh -c 'echo hello; exit 7'
```

Exec uses Kubernetes `v4.channel.k8s.io` WebSocket channels. Logs/exec share a
four-session global concurrency bound and a 30-second timeout; captured output is
limited to 1MiB. Log snapshots additionally limit the tail to 10000 lines.
These are text-oriented JSON responses, not a binary streaming interface.
Revocation blocks new requests but an already-authorized operation may finish
within its 30-second bound. Long-running I/O does not hold the mutation lock.
The timeout bounds the I/O session, not guaranteed process termination: a custom
command can keep running after disconnect. It remains inside the Pod's resource
limits and lifetime. Use an explicit process supervisor/timeout when needed.

## Lifecycle and security boundaries

- Child Pods run as UID/GID 1001 with all capabilities dropped, no privilege
  escalation, RuntimeDefault seccomp and no Kubernetes service-account token.
  Images requiring root, privileged setup, devices or a different UID may fail;
  privileges are never silently broadened. An administrator may configure the
  existing workspace sandbox RuntimeClass for stronger isolation.
- Requests equal limits for CPU/memory; ephemeral storage has a 1Gi request/limit.
  Storage eviction is not a hard disk quota, and PID limits depend on the
  cluster's kubelet/runtime configuration. Images are arbitrary executable code:
  use sandboxed nodes and cluster admission/image policy appropriate to your trust.
- Parent and sibling access uses Hub-owned labels and NetworkPolicy. Only exposed
  TCP ports accept inbound connections on children. Children may connect to any
  port on their parent (same trust group). Child egress receives DNS/public
  Internet according to chart settings but not workspace-specific or global
  private-egress exceptions. IPv6 has no general Internet allow rule.
- SQL stores desired lifecycle/resource/mount metadata, not env contents.
  Environment values live only in an immutable Kubernetes Secret. A failed
  initial Secret write leaves a removable error reservation, not a workload
  silently launched with missing environment.
- All states, including completed and deleting workloads, retain CPU/memory/count
  reservations until confirmed cleanup. Named/anonymous PVCs retain storage
  reservations until their deletion is confirmed. `--rm` cleanup is reconciled
  independently of CLI connectivity.
- Workloads expire after eight hours. Persisted launch intent prevents automatic
  re-execution after a crash or external Pod deletion; ambiguous/missing launches
  become `lost` and require explicit removal/recreation. Missing previously
  provisioned PVCs are never silently recreated with empty data.
- The existing one-replica/Recreate Hub requirement remains: application-level
  combined budgets rely on its serialized mutation lock and transactional
  reservations. Namespace quotas are not a replacement for that requirement.

## Verification and remaining work

`go test -race ./...` with a dedicated `TEST_DATABASE_URL` exercises credentials,
ownership isolation, reservations, cleanup failures, retained volumes, missing
Pods/PVCs, expiry, HTTP/client interoperability and WebSocket exec status handling.
Never use a production database: existing integration tests truncate tables.

Before production enablement, validate admission, read-only/subPath mounts, RWO/RWX
scheduling, networking (including CNI/DNAT and IPv6) and exec protocol support in
an isolated cluster. Integration tests simulate Kubernetes, not its kernel or CNI.
Interactive exec/attach, TTY resize, log following, start/stop, port forwarding,
Secret references, registry credentials and standalone binary release downloads
remain follow-on work. This is not full Docker/Compose compatibility.

Source development does not imply deployment or any private-cluster changes.
