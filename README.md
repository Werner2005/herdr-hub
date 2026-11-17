# Herdr Hub

Herdr Hub provisions persistent AI development workspaces on Kubernetes.
Its Go API and React dashboard manage users, workspace ownership, storage,
SSH keys, configuration mappings, network access and exported application ports.
Each workspace provides an [Ovis](https://github.com/devswha/ovis) web terminal,
SSH, development tools and a persistent home directory.

## Public artifacts

| Artifact | Location |
| --- | --- |
| Workspace image | `ghcr.io/werner2005/herdr-hub/workspace:v0.1` |
| Hub image | `ghcr.io/werner2005/herdr-hub/hub:v0.1` |
| Helm chart | `oci://ghcr.io/werner2005/herdr-hub/charts/herdr-hub` |

Images support Linux **amd64 and arm64**. The first release is **v0.1**;
its Helm chart version is **0.1.0**. Published artifacts are public and require
no registry credentials. SHA tags identify tested builds; `main` tracks the
development branch and `latest` tracks the most recently published stable release.
Prefer an explicit release or digest in production.

## Run a standalone workspace

Docker is sufficient for a single workspace (the Hub requires Kubernetes):

```sh
export OVIS_TOKEN="$(openssl rand -hex 32)"
docker run -d --name herdr-workspace \
  -p 127.0.0.1:7317:7317 -p 127.0.0.1:2222:2222 \
  -e OVIS_TOKEN \
  -v herdr-home:/home/dev \
  ghcr.io/werner2005/herdr-hub/workspace:v0.1
```

Retain your token securely and use it to sign in at <http://localhost:7317>.
The token grants terminal access: keep it secret
and do not expose the service without HTTPS and appropriate access controls.
SSH requires an authorized-keys file mounted at
`/run/ssh-keys/authorized_keys`. Workspace UID/GID is 1001.

The persistent home contains `/home/dev/workspace`, installed tools and Ovis
state. Startup installs the pinned Ovis plugin when missing, synchronizes its
token, restores saved APT packages through `~/startup.sh`, and applies OS updates.
Startup requires outbound access to package repositories. Existing homes and
user-managed tools are preserved; deleting a container does not delete its volume.

## Install Hub on Kubernetes

### Prerequisites

- Kubernetes with a NetworkPolicy-enforcing CNI and a suitable StorageClass.
  Enable volume expansion if users should expand workspace disks.
- PostgreSQL, provisioned separately (for example with CloudNativePG).
- Gateway API CRDs, including TCPRoute and UDPRoute for the corresponding
  exports, and a compatible controller such as Envoy Gateway.
- A GatewayClass, wildcard DNS, and a trusted HTTPS certificate covering
  the Hub and workspace hosts. The example uses cert-manager with an existing
  ClusterIssuer; alternatively disable certificate creation and supply a TLS Secret.
- Helm 3 with OCI support and kubectl access to the cluster.

The chart does **not** install PostgreSQL, operators, CRDs, a StorageClass or a
GatewayClass. Hub uses an in-cluster ServiceAccount; running it locally with only
database credentials is not sufficient.

### Configure and install

Download [examples/values.yaml](examples/values.yaml), save it as `values.yaml`
and replace its domain, StorageClass, database settings, GatewayClass and issuer.
The example assumes installation into namespace `herdr`.

```sh
kubectl create namespace herdr

# Export your database password and a strong bootstrap admin password first.
# The admin password must be 12–72 bytes.
kubectl create secret generic herdr-db-app -n herdr \
  --from-literal=password="$DATABASE_PASSWORD"
kubectl create secret generic herdr-hub-auth -n herdr \
  --from-literal=token="$(openssl rand -hex 32)" \
  --from-literal=admin-password="$HUB_ADMIN_PASSWORD"

helm upgrade --install herdr-hub \
  oci://ghcr.io/werner2005/herdr-hub/charts/herdr-hub \
  --version 0.1.0 --namespace herdr \
  -f values.yaml --wait --timeout 10m

kubectl wait -n herdr certificate/hub-gateway-tls \
  --for=condition=Ready --timeout=3m
helm test herdr-hub -n herdr --logs
```

For an existing installation, preserve existing Secret keys. Bootstrap creates
the admin only if missing; changing its Secret does not reset an existing password.
With the example's DNS, open `https://hub.apps.example.com` and sign in as `admin`.

Point `hub.apps.example.com`, `*.apps.example.com` and `ssh.apps.example.com`
at the Gateway. Expose HTTPS plus configured SSH (2201–2299) and application
export (2300–2399) port ranges through the load balancer and firewall.
Keep `gateway.listenerHostname` empty when the listener must accept both Hub
and workspace hosts. Hub adds listeners dynamically; the Gateway API listener
limit also limits how many SSH/export listeners can coexist.

See [chart values](charts/herdr-hub/values.yaml) for all settings. Configure
NetworkPolicy selectors to match your actual DNS, Gateway and database pods;
additional policies are additive and can weaken isolation.

### Upgrade

Back up PostgreSQL and workspace volumes first. Review release notes, then
upgrade with the new chart version and matching workspace/Hub image tags.
Hub performs schema initialization at startup; there is no separate migration
runner. Keep **one Hub replica with Recreate**, as API operations use a
process-local mutex.

Deleting a runtime retains its home PVC; recreating the same workspace name
reattaches it. Configuration/Secret mappings, egress edits and exported ports
reconcile without recreating runtime Deployments. Image updates reconcile
separately. Do not assume that deleting a user revokes independent workspace
credentials, or that transferring a retained home sanitizes its contents.
Exported application traffic has no Hub authentication.

## Develop

```sh
git clone https://github.com/Werner2005/herdr-hub.git
cd herdr-hub
go test ./app
go vet ./app
npm --prefix web ci
node --test web/src/*.test.js
npm --prefix web run build
helm lint charts/herdr-hub -n herdr -f examples/values.yaml
```

Use the Go version in `go.mod` and Node 22 for the frontend build.
`TestWorkspaceLifecycle` needs a **dedicated disposable PostgreSQL database**:
it truncates application tables. Set `TEST_DATABASE_URL` to enable it.
Vite proxies `/api` and `/healthz` to port 8080; production Go serves
`web/dist` through `WEB_DIST_DIR`. Swagger/OpenAPI documentation is available
under `/docs` when `hub.docsEnabled` is enabled.

```sh
docker build -t herdr-workspace:local .
docker build -f Dockerfile.app -t herdr-hub:local .
```

The devcontainer uses the workspace Dockerfile. Workspace image builds also
run the Ovis compatibility hook's Bun tests on the native build platform.

## CI and releases

GitHub Actions runs Go race/integration tests, vet, module verification,
govulncheck, frontend tests/build/audit, and Helm lint/render checks.
Native GitHub-hosted amd64/arm64 runners build both images. Trivy scans each
architecture for fixable HIGH/CRITICAL vulnerabilities before combined
SHA, branch or release manifests are published. Intermediate `staging-*`
tags are not approved releases. Images include SBOM and provenance attestations.

To release:

1. Update chart `version` and `appVersion`, and both default image tags.
2. Commit, push, and wait for verification.
3. Push a version tag, for example:

   ```sh
   git tag -a v0.1 -m "Herdr Hub v0.1"
   git push origin v0.1
   ```

Tags use `vMAJOR.MINOR` or `vMAJOR.MINOR.PATCH`; Helm always uses three components.
The tag pipeline validates version agreement, publishes the two multiarchitecture
images and OCI chart, and creates a GitHub release with the packaged chart.
Do not move published tags. Re-running a failed release uses the same source.
Publishing uses the workflow's `GITHUB_TOKEN`, not a personal token.

**Maintainer setup:** GHCR packages initially default to private. After the
first publication, change the workspace, Hub and chart packages to **public**
in GitHub package settings. The source repository being public is not sufficient.

GitHub does not deploy into any cluster. Cluster provisioning, secrets and
deployment automation belong in your own infrastructure repository.

## License

Herdr Hub's original code is licensed under the [MIT License](LICENCE.md),
copyright 2026 Andreas Werner. Bundled Herdr remains Apache-2.0 licensed;
Ovis remains MIT licensed. These components can be distributed alongside
Herdr Hub, provided their respective license and notice requirements are
preserved. See [third-party licensing](THIRD_PARTY_NOTICES.md) for the pinned
versions, upstream sources, retained notices and distribution scope.
