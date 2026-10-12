<!-- llm-readme-management spec=1 commit=6b2076359eff8df0f1c5d88d4f84cc0158c803d0 template=golang model=qwen3.8-27b-q4 digest=b3d4b07f2e19 generated=2026-10-12T11:21:28Z -->
<a href="https://hauke.cloud" target="_blank"><img src="https://img.shields.io/badge/home-hauke.cloud-brightgreen" alt="hauke.cloud" style="display: block;" /></a>
<a href="https://github.com/hauke-cloud" target="_blank"><img src="https://img.shields.io/badge/github-hauke.cloud-blue" alt="hauke.cloud Github Organisation" style="display: block;" /></a>
<a href="https://github.com/hauke-cloud/llm-readme-management" target="_blank"><img src="https://img.shields.io/badge/template-golang-orange" alt="Repository type - golang" style="display: block;" /></a>


# router-api


<img src="https://raw.githubusercontent.com/hauke-cloud/.github/main/resources/img/organisation-logo-small.png" alt="hauke.cloud logo" width="109" height="123" align="right">


<llm header hint="Name the Go module path and say whether this is a service, a CLI or a library.">

This Go service (module `github.com/hauke-cloud/router-api`) manages routers as Kubernetes custom resources, with VyOS-in-containers on Hetzner Cloud as the current backend. You deploy it as a Helm chart into your cluster to get rolling updates, in-place configuration, and self-healing for a small fleet of edge or home-lab routers. It is a service, not a library or CLI.

</llm>


## :book: Description

<llm description>

router-api manages network routers the way Cluster API manages nodes: you declare router objects as Kubernetes CRDs, and three controller managers reconcile them into running infrastructure. The current backend runs VyOS in containers on Hetzner Cloud servers, bootstrapped automatically (cloud-init installs podman, starts the VyOS container, drops a first-boot script) and configured in place over a pinned-certificate REST API with commit-confirm revert timers so a bad change reverts itself.

You install a Helm chart into your Kubernetes cluster, write a small set of manifests, and the managers handle creating Hetzner servers behind a management firewall, applying configuration one router at a time, and keeping the fleet healthy.

- Rolling updates with Surge or Slots strategy, one router at a time
- In-place VyOS configuration via REST API with automatic revert on failure
- VRRP failover over the Hetzner private network; Floating IPs follow the new master
- Health checks that reboot a stuck router, then replace it if the reboot fails
- Gateway-driven exposure: listener ports from annotated Gateways propagate to firewalls and router config

The project is early-stage: routers have been bootstrapped, configured, and failed over on Hetzner, but no lab has been connected through one yet.

</llm>


## :clipboard: Requirements

<llm requirements hint="Give the Go version from the go directive in go.mod. Mention Docker only if the repository actually builds an image.">

- Go 1.27.1 (the `go` directive in `go.mod`).
- Helm, for installing the chart into a Kubernetes cluster.
- A Kubernetes cluster where the three manager Deployments will run.
- Docker or Podman for `make image` and `make test-vyos`; multi-arch image builds additionally require Docker Buildx.
- The `ghcr.io/hauke-cloud/vyos` container image, for `make test-vyos`.
- `HCLOUD_TOKEN` and a dedicated Hetzner Cloud project, for `make e2e` (the test creates and bills real servers for up to ~25 minutes).
- Network access to download envtest binaries (kube-apiserver and etcd v1.37) when running `make test`.
- pre-commit, mandatory for contributors.
- A Gateway API implementation in the cluster (gateway-api v1 ≥ 1.5) if you use `RouterExposure`-driven firewall and configuration updates.

</llm>


## 🚀 Getting started

<llm getting_started hint="Cover go build, go run and go test with the real package paths. If a Makefile or Taskfile exists, prefer its targets over raw go commands.">

1. Clone the repository and enter it.

```bash
git clone https://github.com/hauke-cloud/router-api.git
cd router-api
```

2. Fetch and verify all Go module dependencies (requires Go 1.27).

```bash
make tidy
```

3. Build the three manager binaries (`core`, `infrastructure-hetzner`, `config-vyos`) into `bin/`.

```bash
make build
```

4. Run the full test suite, which includes the system test that boots all three managers against an envtest API server with a fake Hetzner cloud and fake routers.

```bash
make test
```

If `make test` fails because it cannot find the envtest binaries, download them first:

```bash
make envtest-assets
```

At this point you have three working binaries in `bin/` and a passing test suite. Deploying to a real cluster is a Helm install (`make helm-install`) into a Kubernetes cluster that can reach your routers' management port; see the usage section for that.

</llm>


## :airplane: Usage

<llm usage hint="For a library, show a small import-and-call example using real exported identifiers. For a service or CLI, show how it is started and the flags or subcommands it accepts.">

Install the chart into your Kubernetes cluster:

```bash
helm upgrade --install router-api oci://ghcr.io/hauke-cloud/charts/router-api \
  --namespace router-api --create-namespace
```

The chart deploys three manager Deployments (`core`, `infrastructure-hetzner`, `config-vyos`) from the image `ghcr.io/hauke-cloud/router-api`. Each manager installs its own CRDs at start-up (`crds.install` defaults to `true`). Set `watchNamespace` to confine all managers to a single existing namespace; leave it empty (the default) to watch all namespaces.

Declare routers by applying the CRD manifests. The `examples/home-lab/` directory contains a complete working set: a `HetznerRouterNetwork`, `HetznerMachineTemplate` and `VyOSConfigTemplate`, a `RouterDeployment` (default `replicas: 2`, `strategy.type: Surge`), a `RouterHealthCheck`, and a `RouterExposure`. Once applied, the managers create Hetzner servers, bootstrap VyOS containers, and apply configuration over the pinned-certificate REST API.

Run the test suite from the repository root:

```bash
make test
```

This runs unit tests, envtest-backed integration tests, and the system test (all three managers against a fake Hetzner and fake routers). To exercise a real VyOS container:

```bash
make test-vyos
```

This requires podman (rootless is enough) and the `ghcr.io/hauke-cloud/vyos:dev` image. The `make e2e` target provisions billed Hetzner servers for up to 90 minutes; it needs `HCLOUD_TOKEN` plus the `E2E_*` variables produced by `hack/e2e-hetzner.sh setup`.

</llm>


## :wrench: Configuration

<llm configuration hint="Environment variables and CLI flags, taken from the flag definitions or the config struct.">

The Helm chart (`deployments/helm/router-api/values.yaml`) exposes:

| Name | Type | Default | Required | Description |
|------|------|---------|----------|-------------|
| `image.repository` | string | `ghcr.io/hauke-cloud/router-api` | No | Container image |
| `image.tag` | string | `""` (chart appVersion) | No | Image tag |
| `watchNamespace` | string | `""` (all) | No | Confine managers to one namespace |
| `crds.install` | bool | `true` | No | Install embedded CRDs at start |
| `logLevel` | string | `info` | No | Log level |
| `logFormat` | string | `json` | No | `json` or `text` |
| `managers.<name>.enabled` | bool | `true` | No | Enable a manager |
| `managers.<name>.replicas` | int | `1` | No | Replica count |
| `metrics.port` | int | `8080` | No | Metrics listen port |

Standard Kubernetes fields (`serviceAccount`, `rbac`, `podSecurityContext`, `nodeSelector`, `tolerations`, `affinity`, `priorityClassName`, `imagePullSecrets`, `nameOverride`, `fullnameOverride`, `podAnnotations`, `podLabels`) are also present; see `values.yaml` for the full set.

All three binaries share these flags (`internal/manager/manager.go`):

| Flag | Default | Description |
|------|---------|-------------|
| `-metrics-bind-address` | `:8080` | Metrics; `0` disables |
| `-health-probe-bind-address` | `:8081` | Health probes |
| `-leader-elect` | `false` | Leader election |
| `-namespace` | `""` (all) | Watch one namespace |
| `-install-crds` | `true` | Install embedded CRDs |
| `-log-level` / `-log-format` | `info` / `json` | Logging |
| `-version` | — | Print version and exit |

Test and tooling environment variables:

| Name | Default | Description |
|------|---------|-------------|
| `HCLOUD_TOKEN` | — | Hetzner Cloud token (e2e, required) |
| `VYOS_IMAGE` | `ghcr.io/hauke-cloud/vyos:dev` | Image for `make test-vyos` |
| `KUBEBUILDER_ASSETS` | — | envtest binary path (set by `make test`) |
| `E2E_NETWORK` | — | e2e Hetzner network (required) |
| `E2E_SERVER_IMAGE` | `ubuntu-24.04` | e2e server image |
| `E2E_LOCATION` | `fsn1` | e2e Hetzner location |

Remaining `E2E_*` variables are defined in `test/e2e/e2e_test.go`; `ROUTER_*` variables for manual VyOS runs are in `test/vyos/remote_test.go`.

</llm>


## :hammer: Development

<llm development hint="Include go test, go vet and gofmt only where the CI workflows actually run them.">

Before pushing, run the same checks CI runs:

```bash
make check
```

This runs the format check, `go vet`, golangci-lint, a generated-file drift check, Helm chart lint, and the full test suite with envtest assets.

If you changed CRD types under `api/`, regenerate the deepcopy functions and embedded CRD YAMLs:

```bash
make generate
```

`make generate-check` (part of `make ci-lint`) fails the build if `internal/crd/assets/` or `zz_generated.deepcopy.go` files are stale.

CI also enforces:

- **Conventional-commit PR titles** – the subject must start with `fix`, `feat`, `docs`, `ci`, or `chore` followed by a colon and an uppercase letter.
- **Pre-commit hooks** – install with `pre-commit install`; they run on every commit.
- **Helm chart lint** – `make helm-lint` renders the chart with all option combinations and checks RBAC; `make tag-guard` requires exactly one `tag:` line in `values.yaml` so CI can rewrite it.

For individual steps: `make fmt`, `make vet`, `make lint`, `make test`, `make helm-lint`.

</llm>


## 📄 License

This Project is licensed under the GNU General Public License v3.0

- see the [LICENSE](LICENSE) file for details.


## :coffee: Contributing

To become a contributor, please check out the [CONTRIBUTING](CONTRIBUTING.md) file.


## :email: Contact

For any inquiries or support requests, please open an issue in this
repository or contact us at [contact@hauke.cloud](mailto:contact@hauke.cloud).
