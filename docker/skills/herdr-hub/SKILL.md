---
name: herdr-hub
description: Run resource-limited supporting containers from a Herdr workspace using the herdr-hub CLI. Use for launching services, connecting parent and child workloads, Docker-style volumes, logs, status, non-interactive custom commands, and safe cleanup without a Docker daemon.
---

# Herdr Hub child workloads

## What the command does

`herdr-hub` is the workspace's Go CLI for launching supporting containers through
the Hub. It is **not** the `herdr` editor command, Docker Engine, or a Docker API
adapter. It does not make Docker Compose or Testcontainers work unchanged.

The CLI sends authenticated HTTPS requests to `/api/workload/v1`. The Hub derives
the parent workspace from a scoped credential, reserves its resource budget, and
creates Kubernetes Pods, internal Services, NetworkPolicies and PVCs. The workspace
does not receive Kubernetes credentials or access to a Docker socket.

Treat launch, exec, writable mounts and deletion as real operations requiring the
user's intent. Use dry runs for explanation or request inspection, not as evidence
that a container actually started.

## Check prerequisites without exposing credentials

```sh
command -v herdr-hub
herdr-hub --help
```

Live commands require:

- An administrator-enabled workload API and enforced NetworkPolicy.
- An administrator-defined budget for this parent workspace.
- `HERDR_HUB_URL`: the Hub's HTTPS origin, with a trusted certificate.
- `HERDR_HUB_TOKEN_FILE`: a regular, non-symlink, owner-only file (mode `0600`)
  containing a separate scoped `hhw_` credential.

Do not print, copy into command arguments, commit, or include the credential in
logs. Do not substitute a Hub administrator token, a workspace web token or
Kubernetes credentials. If access is missing, ask the user/administrator to
configure it; do not search unrelated files for credentials, broaden permissions
or disable TLS verification.

An owner/admin can issue a scoped token through
`POST /api/workspaces/NAME/workload-token` after an administrator configures
`PUT /api/workspaces/NAME/workload-access`. Issuance replaces the previous token;
tokens expire after 24 hours. `DELETE .../workload-token` revokes access.
These control endpoints use normal Hub authentication, not the scoped CLI token.

## Start and inspect a service

All run options must precede IMAGE. `--name` is required and names are lowercase
DNS labels, at most 40 characters. Live launches currently require `-d`.

```sh
# Validate locally without contacting the Hub.
herdr-hub run --dry-run --name web --cpu 250m --memory 256Mi \
  --expose 8080 python:3.13-alpine python -m http.server 8080

# Actually reserve resources and start the service.
herdr-hub run -d --name web --cpu 250m --memory 256Mi \
  --expose 8080 python:3.13-alpine python -m http.server 8080

herdr-hub ps
herdr-hub inspect web
herdr-hub logs web
```

Object commands output JSON. A successful launch means the request was submitted,
not necessarily that the application is ready. Inspect `status` and logs before
claiming readiness. Creation failures may retain a reservation: inspect/list
before retrying instead of launching another copy.

`--expose PORT` creates a **private TCP** Service. Read `hostname` from `inspect`
and connect to that hostname and the declared port from the parent or siblings.
No Service exists when no ports were declared. This is not Docker's `-p`, a
localhost port forward, or a public export. Unrelated workspace groups are denied.
Children can reach their own parent; private-egress exceptions configured for the
parent are not automatically inherited by children.

## Execute custom commands

```sh
herdr-hub exec web python -c 'print("hello from child")'
herdr-hub exec web sh -c 'printf "%s\n" "two words"; id'
```

Exec preserves argument boundaries, stdout, stderr and remote exit codes.
There is no implicit shell: use `sh -c` explicitly for pipelines, expansion or
multiple commands. Run options `--entrypoint EXECUTABLE` replace the image's
ENTRYPOINT; arguments after IMAGE replace CMD while retaining an existing
ENTRYPOINT unless explicitly overridden.

Exec is non-interactive. I/O sessions have a 30-second timeout, 1MiB output bound
and four-session global concurrency limit. Timeout/disconnect does **not**
guarantee that the remote process stops; Pod resource/lifetime limits still apply.
Logs are snapshots of at most 1MiB/10000 lines, not `logs -f`.

## Docker-style volumes

```sh
# Create durable named storage with an explicit size.
herdr-hub volume create data --size 5Gi
herdr-hub run -d --name worker -v data:/data \
  alpine:3.22 sleep 3600

# Share an existing selected project directory read-only.
herdr-hub run -d --name preview --expose 8080 \
  -v ./public:/app:ro -w /app \
  python:3.13-alpine python -m http.server 8080

# Anonymous storage, removed together with this workload.
herdr-hub run -d --rm --name scratch -v /scratch \
  alpine:3.22 sleep 60

herdr-hub volume ls
herdr-hub volume inspect data
```

- Repeat `-v SOURCE:TARGET[:ro|rw]`; the default mode is writable (`rw`).
- A simple source name refers to this parent's named volume. Missing named
  volumes and anonymous mounts reserve `1Gi` each by default.
- Sources beginning with `.` or `/`, or containing `/`, refer to existing
  directories in the authorized parent home PVC, normally `/home/dev`.
  Relative paths resolve against the CLI's working directory.
- Paths are **not** node paths or arbitrary PVCs. Container-layer files are not
  shareable this way. Single-file mounts are not supported by the CLI.
- `HERDR_HUB_WORKSPACE_ROOT` only changes local validation, not server permission.
  Symlink escapes are rejected; kubelet may reject symlink subpaths at startup.
- Mount only the needed directory. Prefer `:ro` for source/configuration and
  avoid sharing the entire home, SSH files or credential directories.
- Targets must be clean absolute paths; reserved system paths, duplicate and
  overlapping targets, SELinux labels and propagation suffixes are rejected.
- Child Pods are scheduled on the parent's node for RWO compatibility; actual
  storage-backend constraints can still prevent startup.

## Resources, persistence and cleanup

Default CPU/memory limits are `500m` and `512Mi`; customize with `--cpu` and
`--memory` within the parent's combined budget. Requests equal limits.
All states, including completed/failed/deleting workloads, retain reservations
until cleanup is confirmed. Storage has separate size/count budgets.

Pods run as UID/GID `1001`, without capabilities, privilege escalation or
service-account tokens. Images requiring root or another UID may fail; do not
work around that by granting privileges. Ephemeral storage has a `1Gi` limit;
disk eviction and PID restrictions depend on cluster configuration.

Workloads expire after **eight hours** and do not restart automatically.
Only mounted persistent storage survives workload removal. `--rm` requests
automatic removal after completion; named volumes survive both `--rm` and `rm`.
Missing executed Pods or previously provisioned PVCs are not silently recreated.

```sh
# Removes the workload and its anonymous storage, not its named volumes.
herdr-hub rm worker

# Destructive: explicitly delete named data only when the user wants that.
herdr-hub volume rm data
```

Volume removal is refused while any workload references it. Partial deletion
retains reservations and retries in the background; inspect status rather than
claiming cleanup succeeded after an error. Parent stopping, deletion, credential
rotation and ownership transfer require child workloads and child volumes to be
removed first. Do not delete data merely to bypass such a guard without approval.

## Limits and troubleshooting

- No interactive exec/attach, live `-it`, foreground run/wait, start/stop,
  port forwarding, Compose, image builds or public `-p` publishing yet.
- No privileged mode, host networking/devices/mounts or runtime sockets.
- `-e KEY=VALUE` supplies environment through a Kubernetes Secret, not PostgreSQL.
  Avoid secrets in shell history, arguments or dry-run output. Dedicated secret
  reference flags are not implemented.
- For authentication errors, check configured access/expiry and request a new
  scoped token. For TLS errors, install the approved CA rather than bypassing it.
- For budget conflicts, inspect existing reservations and obtain permission
  before deleting workloads/data or requesting a larger administrator budget.
- For `starting`, `error` or `lost`, inspect status/error and logs. Do not claim
  that retrying will preserve a missing PVC's data or safely repeat a command.

This skill is installed when missing at
`~/.agents/skills/herdr-hub/SKILL.md` during workspace home initialization,
including existing persistent homes on a new image startup. User edits are
preserved. Restart the coding agent after first installation for discovery.
