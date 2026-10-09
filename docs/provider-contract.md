# The provider contract

Core never imports a provider's Go types. It reads and writes provider objects as unstructured
data, through the fields named here, in `internal/contract`. A provider is a set of CRDs and a
controller that honours them. This follows Cluster API's v1beta2 provider contracts where they
apply, and adds what a router needs that a node does not.

All provider objects are namespaced and live next to the core objects that refer to them.
References carry an API group and a kind but no version; core uses the version the API server
prefers.

## Infrastructure provider

Creates the server a router runs on.

**Machine kind** (for example `HetznerMachine`), referred to by `RouterMachine.spec.infrastructureRef`:

| Field | |
| --- | --- |
| `spec.providerID` | set once the server exists |
| `status.initialization.provisioned` | true once the server has been created and runs; never goes back |
| `status.addresses[]` | `type` (`ExternalIP`, `InternalIP`, `Hostname`, ...) and `address` |
| `status.failureDomain` | optional |
| `status.conditions[type=Ready]` | true while the server runs |
| `status.lastRemediation` | see below |

The provider finds its owner `RouterMachine` through the controller owner reference and creates
the server from the Secret named in the owner's `spec.bootstrap.dataSecretName` (key `value`).
It must not create a server before that is set.

It must not recreate a server that has existed and is gone. It reports `Ready=False` and leaves
the decision to the health check.

**Remediation.** Core asks for an instance to be recovered by setting the annotation
`router.hauke.cloud/remediation` on the machine object, to `<Action>/<attempt>/<time>`. The only
action is `Reboot`. The provider acts on a value once and then copies it to
`status.lastRemediation`.

**Template kind**: the machine kind's name plus `Template`, with the machine's spec under
`spec.template.spec` and optional labels and annotations under `spec.template.metadata`. Any
change to it replaces routers.

## Config provider

Boots a router and keeps it configured.

**Config kind** (for example `VyOSConfig`), referred to by `Router.spec.configRef`:

| Field | |
| --- | --- |
| `status.dataSecretName` | the Secret with the server's user data under the key `value` |
| `status.initialization.dataSecretCreated` | true once that Secret exists |
| `status.version` | the software version the router reports |
| `status.conditions[type=ConfigApplied]` | true while the router runs what the object currently describes |
| `status.conditions[type=Healthy]` | true while the router can be reached and is in order |
| `status.conditions[type=Drained]` | see below |

The bootstrap data is written once. It has to keep describing the server that was created from
it.

`ConfigApplied` carries `observedGeneration`. Core disregards a verdict about an older
generation of the object. `Healthy` and `Drained` are about the router itself and are taken as
they are.

The provider finds its owner `Router` through the controller owner reference. The router's
addresses are in `Router.status.addresses`, and the other routers of the group are the Routers
with the same `router.hauke.cloud/deployment-name` label.

**Drain.** Before core deletes a working router it sets the annotation
`router.hauke.cloud/drain` on the config object. The provider makes the router give up whatever
a peer can take over and sets `Drained=True` once it has. Core waits for that, up to three
minutes.

**Template kind**: as for infrastructure, plus one field that decides between replacing routers
and reconfiguring them:

| Field | |
| --- | --- |
| `status.replacementHash` | a hash of the fields a running router cannot change |
| `status.observedGeneration` | the generation that hash was computed from |

Core starts a rollout when `replacementHash` changes. For any other change it copies the
template's spec to the config objects of the running routers, one at a time, waiting for
`ConfigApplied` on each. Core does not create routers from a template whose hash is missing or
out of date.

## Permissions

Core's ClusterRole is aggregated. A provider ships a ClusterRole labelled
`router.hauke.cloud/aggregate-to-core: "true"` that grants access to its API group.

A provider needs to read the core kinds it looks at (`routermachines` or `routers`).

## Common

- The annotation `router.hauke.cloud/paused` on an object, or on its owner, stops a controller
  from acting on it.
- Objects created from a template carry `router.hauke.cloud/cloned-from-name` and
  `router.hauke.cloud/cloned-from-groupkind`.
- Everything that belongs to one router carries `router.hauke.cloud/router-name`; everything of
  one group `router.hauke.cloud/deployment-name`.

## Slots

In a group that uses the Slots strategy, every object of a router also carries
`router.hauke.cloud/slot`: "0", "1", and so on. The slot survives the router. Core guarantees
that no two routers of a group hold the same slot, and that a slot is only given to a new router
once the previous one is gone, including its infrastructure object.

A provider ties to the slot whatever must not change when a router is replaced. The Hetzner
provider gives the server the Primary IP listed for its slot, and waits while that address is
still assigned to the previous server. The VyOS provider renders with the values listed for the
slot. A provider must not create anything for a router whose slot it has nothing for.
