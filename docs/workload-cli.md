# Experimental `herdr-hub` workload CLI

## Status and scope

This is the first implementation slice: a Go CLI, a typed workload contract and
an HTTPS client with mock-server tests. **There is no workload backend yet.**
The current Hub returns 404 for these endpoints and cannot issue workload
credentials. Do not put an administrator or workspace web token into this CLI.
There are no new RBAC permissions, Helm switches, database tables or cluster
changes. The CLI is not automatically included in workspace images or releases.

The immediate executable feature is `run --dry-run`. Other commands implement
requests against the proposed API and are tested with fake/TLS HTTP servers.
They do not launch real containers until the backend is implemented.

## Build and local validation

From the repository root, using the Go version in `go.mod`:

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
- Directory sources must be PVC-backed and independently resolved by the Hub.
  Container-layer files and directories are not shared automatically.
- Initial syntax supports directories, not individual file mounts.
- Targets must be clean absolute paths, not `/` or reserved kernel/service-account
  directories. Duplicate and overlapping mount targets are rejected.
- Mode defaults to `rw`; `ro` is explicit. SELinux labels, propagation options
  and other Docker-specific suffixes are unsupported and rejected.
- Anonymous volumes are intended to be dedicated quota-accounted PVCs; `--rm`
  will remove them after confirmed workload cleanup. Named volumes survive.
  This is a contract, not an implemented PVC lifecycle yet.

Future backend volume commands:

```sh
herdr-hub volume create db-data --size 5Gi
herdr-hub volume ls
herdr-hub volume inspect db-data
herdr-hub volume rm db-data
```

The backend must refuse removal of referenced volumes, reserve storage before
provisioning, verify access modes and scheduling compatibility, and avoid deleting
named volumes when a workload is removed. `rm -v` is not implemented yet.

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

Types are defined in `internal/workload`. The backend must reject unknown fields
and enforce strict bounded decoding; the contract's structural validation alone
is insufficient for authorization, image policy, quantity parsing or quotas.

The intended CLI configuration is:

```sh
export HERDR_HUB_URL=https://hub.apps.example.com
export HERDR_HUB_TOKEN_FILE=/run/herdr-hub/workload-token
```

Credentials use a distinct `hhw_` prefix plus 32 random bytes encoded as 64 hex
characters. **Issuance is not implemented.** Files must be regular non-symlink
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

The future backend must bound captured output, runtime and concurrent executions.
Interactive exec/attach, TTY resize, log following, start/stop, port forwarding,
secret references and automated binary distribution remain follow-on work.

## Backend implementation gates

Before enabling real workload creation:

1. **Scoped authentication:** issue/revoke separate workspace credentials; hash
   them in storage; reject them on all existing administrative APIs. Revalidate
   owner/account/workspace state at each operation. Rotation, transfer and parent
   deletion must revoke credentials and handle children before ownership changes.
2. **Persistent intent and budgets:** store lifecycle state and reserve combined
   CPU, memory, count, storage and anonymous-volume budgets atomically. Include
   pending/failed/deleting workloads until Kubernetes cleanup is confirmed.
   Choose namespace-per-group vs shared-namespace explicitly; enforce group
   quotas in Kubernetes where possible.
3. **Safe admission:** no host namespace/mounts, privileged containers, runtime
   sockets, arbitrary service accounts or token automount. Default to non-root,
   dropped capabilities and RuntimeDefault seccomp. Validate compatibility with
   images requiring a different UID rather than silently granting root access.
4. **Storage authorization:** derive PVCs from trusted database relationships;
   resolve directories against allowed parent mounts with race-safe symlink
   handling. Never trust a client-relative path alone or widen sharing to the
   entire home/Secrets. Handle root-squash, UID/fsGroup and RWO/RWX constraints.
5. **Networking:** enforce parent/child group isolation using Hub-owned labels
   and NetworkPolicy, permit only declared ports, preserve existing private
   egress restrictions. Narrowly permit the parent to reach the scoped Hub API.
   No public routes or exports by default. Test CNI behavior, including IPv6.
6. **Lifecycle reconciliation:** recover partial creates/deletes after restart;
   never release reservations before confirmed cleanup; maintain exit status;
   preserve named volumes; make `--rm` independent of CLI connectivity.
7. **Streaming:** implement Kubernetes exec/attach protocol negotiation, then
   Hub transport with stdin/stdout/stderr/status/resize framing, cancellation,
   timeouts, concurrency bounds and authorization. Do not hold the global
   mutation lock for long-running sessions.
8. **Verification:** use a dedicated test database and isolated cluster to test
   cross-owner denial, concurrent quota exhaustion, image failures, restart
   recovery, network isolation, mount escapes, readonly mounts, exec disconnects
   and remote exit codes.

Source development does not imply deployment or any private-cluster changes.
