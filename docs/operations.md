# Operating routers

## Looking

```sh
kubectl get routerdeployments,routersets,routers          # short names: rd, rs, rt
kubectl get vyosconfigs,hetznermachines                   # vc, hm
kubectl get router-api                                    # everything, by category
```

`Router` is the object that answers "does it work". Its conditions:

| Condition | True means | Comes from |
| --- | --- | --- |
| `Ready` | in service: `MachineReady`, configured at least once, `Healthy` | core |
| `MachineReady` | the server exists and is running | infrastructure provider |
| `BootstrapReady` | the data the server boots with has been published | config provider |
| `ConfigApplied` | it runs the configuration its config object currently describes | config provider |
| `Healthy` | the API answers and no VRRP group is in fault | config provider |
| `Active` | the router of its group that carries the traffic (the VRRP master) | config provider |
| `Drained` | asked to hand over, and no longer VRRP master | config provider |

The `VyOSConfig` of the same name has the detail: `APIReachable`, `VRRPMaster` (its reason is
the VRRP state), the VyOS version, and when the configuration was last changed.

What is open for Gateways, and why a Gateway or a ListenerSet is not among them:

```sh
kubectl get routerexposures                               # rex
kubectl get rex edge -o jsonpath='{range .status.gateways[*]}{.namespace}/{.name} exposed={.exposed} {.message}{"\n"}{end}'
kubectl get rex edge -o jsonpath='{range .status.listenerSets[*]}{.namespace}/{.name} of {.gateway} exposed={.exposed} {.message}{"\n"}{end}'
```

## Changing

| To | Do | Effect |
| --- | --- | --- |
| change the configuration | edit `commands` or `values` of the `VyOSConfigTemplate` | applied in place, one router at a time |
| upgrade VyOS | change `image` | routers are replaced, one at a time |
| resize or move | edit the `HetznerMachineTemplate` | routers are replaced, one at a time |
| open or close a port | add or remove a listener of an annotated Gateway, or of a ListenerSet it accepted | the firewall follows at once, the routers in place, one at a time |
| add or remove routers | `kubectl scale routerdeployment edge --replicas=3` | |
| hold everything | `spec.paused: true` on the deployment | no rollouts; existing routers stay managed |
| hands off one object | annotation `router.hauke.cloud/paused` | no controller acts on it |

How routers are replaced is the deployment's `strategy.type`:

| | `Surge` (default) | `Slots` |
| --- | --- | --- |
| A replacement | is built and has to work before the old router goes | is built after the old router is gone |
| During a rollout | never fewer routers than asked for, one more at times | one router fewer, never more |
| Routers | interchangeable | each in a slot that keeps its address and its values |
| Use it when | everything that identifies the group can move between routers | each router needs something that stays, such as an address the other side dials |

Either way a working router is asked to hand over before it is removed, so
replacing the active one is a failover it announces, not one the others have to
detect. And the standby is taken first, so that a rollout costs one failover
and not two. With slots, `kubectl get routers -L router.hauke.cloud/slot` shows who
is where.

Watch a rollout with `kubectl get rd,rs,rt -w`. A deployment's `RollingOut` condition is true
until every router is of the current revision, available, and runs the current configuration.

A router that is being deleted no longer counts against `maxSurge`, as with Pods: its server
can exist for a moment next to its replacement's.

## When something is stuck

**A rollout does not advance.** One router at a time, and only while the others are settled.
Look for a `Router` that is not `Ready`, or whose `ConfigApplied` is not true; its message says
why. A configuration one router refused is deliberately not handed to the next.

**`ConfigApplied=False`.** See the table in [configuration.md](configuration.md). Fix the
template; the retry is immediate when the rendered configuration changes.

**`APIReachable=False`.** The operator cannot reach the router's management port. Check, in
this order: the `HetznerRouterNetwork`'s `ManagementSourcesResolved` condition and
`status.managementCIDRs` (does it list the address the operator comes from right now?), the
server in the Hetzner console, and VyOS itself on the server:

```sh
ssh root@<server>                       # with a key from HetznerRouterNetwork.spec.sshKeys
systemctl status vyos
podman exec -it vyos su - vyos          # the VyOS CLI
journalctl -u cloud-final               # the first boot
```

**A router was replaced and you want to know why.** `kubectl describe routerhealthcheck`. The
sequence is: unready for longer than the timeout, a reboot (visible as the
`router.hauke.cloud/remediation` annotation on the `RouterMachine`), and if it is still unready
after `rebootTimeout`, deletion, upon which the `RouterSet` builds a new one.

**Nothing is being remediated although routers are down.** `RemediationAllowed=False` on the
health check: more than `maxUnhealthy` are unhealthy at once. That is the guard against an
operator that has lost its own connectivity rebooting a healthy fleet. Routers keep failing
over among themselves regardless.

**The Floating IP did not follow the VRRP master.** Moving it is the job of
`hcloud-vrrp-failover`, which VyOS runs on the router that becomes master; the operator is not
involved and does not see it fail. On the master:

```sh
podman exec vyos journalctl -b | grep -E 'hcloud-vrrp|keepalived-fifo'
```

The two causes met so far: no name server configured on the router (the helper has to resolve
`api.hetzner.cloud`), and `/config/hetzner/failover.json` or the token file missing.

**A server disappeared.** The `HetznerMachine` reports `ServerNotFound` and does not create
another: a new server from the old user data would come up as a router the operator believes it
has already configured. The health check replaces the router.

## Deleting

Delete the `RouterDeployment`. Each router's server is deleted before the credentials that
belong to it. The `HetznerRouterNetwork` can be deleted once no machine uses it; its firewall
and placement group go with it. The Hetzner network and the Floating IPs were never the
operator's and are left alone.

A `HetznerMachine` is not let go of while its server cannot be deleted, for instance because the
token Secret is gone. Restore the Secret rather than removing the finalizer, or the server keeps
running, and being billed, with nothing in the cluster that knows about it.
