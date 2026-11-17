# Herdr Hub contributor instructions

- `Dockerfile` builds the workspace; `Dockerfile.app` builds the Go Hub and React dashboard.
- `app/main.go` initializes PostgreSQL schema at startup. There is no migration runner.
- Kubernetes integration uses a direct REST client, not client-go. Hub requires
  in-cluster ServiceAccount credentials; local database credentials alone cannot start it.
- Keep Hub at one replica with `Recreate`; mutations use a process-local mutex.
- Runtime resource names are `herdr-{name}`. Ownership requires
  `app.kubernetes.io/managed-by=herdr-hub`. Retry is create-only.
- Deleting a runtime retains its home PVC and SSH host key. Preserve UID/fsGroup 1001,
  `automountServiceAccountToken: false` and the authorized-keys single-file subPath mount.
- Image, configuration mappings, egress and export reconcilers must preserve unrelated
  Deployment fields, credentials, PVCs and resourceVersion safety; never recreate
  missing/deleting runtimes as a side effect.
- Configuration values live only in Kubernetes ConfigMaps/Secrets; PostgreSQL must
  never store their contents. Private mappings require workspace-owner authorization.
- Egress exceptions are additive NetworkPolicies. Retain failed deletions for retry.
- Exported ports retain allocations across undeploy and have no Hub authentication.
- Workspace startup uses the pinned checksum-verified official Ovis installer.
  Preserve persistent homes, user-managed toolchains, legacy migration and stale-lock cleanup.
- Ovis alone receives the preload through both env files. Do not modify tracked
  updater files to bypass clean-tree checks. Run Bun hook tests on `$BUILDPLATFORM`,
  not QEMU. Keep Dockerfile.app's Go/Node builders on `$BUILDPLATFORM`.
- Production assets are served from `WEB_DIST_DIR`, not embedded.
- Verification: `go test ./app`, `go vet ./app`, `node --test web/src/*.test.js`,
  `npm --prefix web ci`, `npm --prefix web run build`, and Helm lint/template with
  `-f examples/values.yaml`. Bare chart defaults intentionally lack installation settings.
- `TestWorkspaceLifecycle` truncates tables: only use a dedicated test database.
- Keep application/chart versions and default image tags aligned for tagged releases.
- No cluster-specific domains, credentials or deployment workflows belong here.
