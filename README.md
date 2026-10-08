# router-api

Routers as Kubernetes objects, in the way [Cluster API](https://cluster-api.sigs.k8s.io) does
it for cluster nodes: you describe a group of routers and how they are configured, and
controllers create the servers, configure them, watch them, replace them when they break and
roll changes through them without the group going down.

The first and so far only implementation runs [VyOS](https://vyos.io) on
[Hetzner Cloud](https://www.hetzner.com/cloud). Both halves are providers behind a contract,
so another cloud or another router OS is a new provider, not a change to the core.

> **Status: early.** The controllers are tested against a real Kubernetes API server, the VyOS
> side against a real VyOS, and routers have been bootstrapped, configured and failed over on
> Hetzner Cloud. No lab has been connected through one yet.
> [docs/spike-vyos-container.md](docs/spike-vyos-container.md) lists exactly what is
> established, how, and what is not.

## The objects

```
RouterDeployment ──► RouterSet ──► Router ──┬─► RouterMachine ──► HetznerMachine    ◄── HetznerMachineTemplate
                                            └─► VyOSConfig                           ◄── VyOSConfigTemplate
RouterHealthCheck ── selects Routers        HetznerRouterNetwork: token, network, firewall, placement group
```

| Kind | Group | What it is |
| --- | --- | --- |
| `RouterDeployment` | `router.hauke.cloud` | A group of routers that back each other up, and the way to change them. |
| `RouterSet` | | A number of identical routers. Created by a deployment, one per revision. |
| `Router` | | One router: a machine and its configuration. **The object to look at.** |
| `RouterMachine` | | The server a router runs on, whatever the provider. |
| `RouterHealthCheck` | | Reboots, then replaces, routers that stop working. |
| `HetznerMachine`, `HetznerMachineTemplate` | `infrastructure.router.hauke.cloud` | A Hetzner Cloud server. |
| `HetznerRouterNetwork` | | What the routers of a group share at Hetzner. |
| `VyOSConfig`, `VyOSConfigTemplate` | `config.router.hauke.cloud` | What a VyOS router boots and runs. |

You write a `HetznerRouterNetwork`, the two templates, a `RouterDeployment` and usually a
`RouterHealthCheck`. Everything else is created for you.
[examples/home-lab](examples/home-lab) is a complete set: a pair of routers that puts a home lab
with a changing address behind stable public IPs.

## How a router comes to be

1. The **config provider** generates the router's credentials (an API key and a self-signed
   certificate) and its bootstrap data: cloud-init user data that installs podman on a stock
   Ubuntu, starts the [VyOS container](https://github.com/hauke-cloud/vyos) and drops a
   first-boot script into VyOS's config volume.
2. The **infrastructure provider** creates the server with that user data, behind a firewall
   that lets the management port through from the operator only.
3. VyOS boots and runs the script, which enables its REST API with the generated key and
   certificate and locks the default account. Nothing reaches into the router from outside to
   do this, and the key and certificate are this router's alone.
4. The config provider connects, trusting exactly that certificate, and applies the
   configuration: your `set` commands, rendered as a template with the router's addresses,
   its peers and values from Secrets.

## Changing routers

**Configuration** (`commands`, `values`) is applied to the running routers, one at a time and
only while the others are fine. Each change is committed with a revert timer and confirmed over
the same connection; a change that cuts the operator off is therefore never confirmed and undoes
itself. A change the router refuses is rolled back and goes no further than the first router.

**Everything else** (the image, files, the server type) replaces the routers: a new one is
built and has to prove itself before an old one is asked to hand over its addresses and is
deleted. With the default `maxSurge: 1` and `maxUnavailable: 0` there are never fewer working
routers than you asked for.

A router is `Ready` while it is in service. Whether it already runs the latest configuration is
a separate condition, `ConfigApplied`: a router that is applying a change, or refused one, still
forwards traffic on the configuration it had.

## When a router breaks

Failover between the routers is theirs alone: VRRP, and a helper in the VyOS image that moves
the Hetzner Floating IPs to the new master. It does not depend on the operator, the cluster it
runs in, or the connection between them.

The operator repairs: a `RouterHealthCheck` reboots a router that stays unready, and replaces
it if that does not help. If more than `maxUnhealthy` routers look broken at once it does
nothing, because seen from one vantage point that is far more likely to be the vantage point.

## Installing

```sh
helm install router-api oci://ghcr.io/hauke-cloud/charts/router-api \
  --namespace router-api --create-namespace
```

Three Deployments, one per manager (`core`, `infrastructure-hetzner`, `config-vyos`), each with
its own ServiceAccount. Each installs the CustomResourceDefinitions of its API group when it
starts. The `config-vyos` manager is the one that connects to the routers.

## Documentation

- [docs/configuration.md](docs/configuration.md): writing a router's configuration
- [docs/operations.md](docs/operations.md): watching, changing, debugging
- [docs/provider-contract.md](docs/provider-contract.md): what a provider has to implement
- [docs/spike-vyos-container.md](docs/spike-vyos-container.md): what was verified about VyOS, and how

## Development

```sh
make check        # format, lint, generated code, chart, all tests
make test         # unit, integration (envtest) and the system test
make test-vyos    # the config provider against a real VyOS container (needs podman and the image)
make e2e          # everything against Hetzner Cloud (needs a token; creates servers)
make help
```

`make e2e` runs the managers against Hetzner Cloud itself and creates billed servers;
[test/e2e](test/e2e/e2e_test.go) says what it needs.

`make test` runs every controller against a real kube-apiserver. `test/system` runs all three
managers together through the life of a router group, with a fake Hetzner whose servers are fake
routers booted from the generated user data.

## License

GNU General Public License v3.0, see [LICENSE](LICENSE).
