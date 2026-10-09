# Example: a home lab behind a pair of Hetzner routers

The lab has no stable public address. A pair of VyOS routers at Hetzner Cloud is
the stable side: they hold the public addresses, the lab's router dials out to
both of them, and everything addressed to the lab's public IPs is routed (not
NATed) through those tunnels.

```
                                     ┌─► router in slot 0 ══ WireGuard ══╗
internet ──────► Floating IPs "svc" ─┤   (its own address)               ╠══► lab router ──► services in the lab
                 (on the VRRP master)└─► router in slot 1 ══ WireGuard ══╝    (dials both)   (announced by BGP)
                                         (its own address)
```

- **Two routers, each in a slot.** A slot is what stays when a router is
  replaced: a public address (a Hetzner Primary IP), a WireGuard key, and the
  addresses inside its tunnel. The successor of the router in slot 1 is again
  the router in slot 1, at the same address, with the same key.
- **The lab keeps a tunnel to both routers, all the time.** It dials each one at
  the address of its slot and sets `persistent-keepalive`; the Hetzner side has
  no endpoint for the lab at all, which is what makes a rotating lab address a
  non-issue. Because the standby already has its tunnel, a failover does not
  have to wait for one to be set up. With a single shared tunnel that wait is
  WireGuard's own: the lab only starts a new handshake after about 15 seconds
  without an answer.
- **One router is active.** The two elect a master with unicast VRRP over the
  Hetzner private network. Hetzner does not move addresses on a gratuitous ARP,
  so on becoming master a router runs `hcloud-vrrp-failover`, which assigns the
  lab's public addresses (Floating IPs) to itself through the API.
- **Those Floating IPs are the lab's public IPs.** The routers do not hold
  them. The master forwards them into its tunnel, any protocol, any port. Which
  addresses those are, every router learns by **eBGP** from the lab router over
  its own tunnel.
- **The operator runs in the lab** and reaches the routers on their public
  addresses. The Hetzner firewall lets the management port through from the
  lab's dynamic DNS name, which the Hetzner provider resolves every minute.
- **Routers are replaced in place.** A slot holds one router, so a router hands
  over and is removed before its successor is built. During a replacement, and
  only then, the group runs on one router.

## What you have to bring

| | |
| --- | --- |
| A Hetzner Cloud Network `lab` with a subnet | the routers attach to it; VRRP runs over it |
| Two Primary IPs, `edge-0` and `edge-1` | one per slot, in the servers' location, **with auto-delete off**; they are what the lab dials and have to outlive every router |
| Floating IPs | one or more for the lab's services; created by you, for the same reason |
| A Hetzner API token, read and write | in `secret.example.yaml`; used by the operator and, on the routers, by the failover helper |
| A WireGuard key pair per slot, and the lab router's public key | `secret.example.yaml`, `config.yaml` |
| A DNS name that follows the lab's public address | `network.yaml` |

On the lab router: one WireGuard peer per slot, each pointing at that slot's
Primary IP with a keepalive and with the tunnel addresses of that slot; an eBGP
session to each router announcing the service addresses; and replies sent back
through the tunnel their request arrived on (most firewalls call this reply-to),
so that traffic *from* the service addresses does not leave through the lab's
own uplink or through the other router. If the lab router itself learns those
addresses over iBGP, for instance from a Kubernetes cluster, note that routes
learned over iBGP are not passed on to another iBGP peer, which is why the
sessions to Hetzner are eBGP with AS numbers of their own.

Every address, name and AS number in these files is a made-up placeholder,
from the documentation, private-address and private-AS ranges. Replace all of
them.

## Status of this example

- The manifests are submitted to a real API server and rendered by
  `internal/examples/examples_test.go`, and the VyOS configuration of slot 0 is
  validated on a real VyOS (`make test-vyos`).
- Slots are exercised by `test/system` against a fake Hetzner, and by `make e2e`
  on Hetzner itself: a router in each slot at the slot's Primary IP, a
  configuration change in place, and both routers replaced in their slots with
  the Floating IP reachable throughout
  ([docs/spike-vyos-container.md](../../docs/spike-vyos-container.md)).
- **This example's own configuration has not run on Hetzner in its current
  form.** An earlier version, with one tunnel shared by both routers, was
  committed in full on Hetzner servers. The per-slot version is validated on a
  real VyOS, not committed on a server.
- **Not tested at all: the tunnels and BGP with a peer on the other end.** No
  lab router was connected in any test.

## Apply

```sh
kubectl create namespace routers
kubectl -n routers apply -f secret.example.yaml   # after filling it in
kubectl -n routers apply -f network.yaml -f machine.yaml -f config.yaml -f routers.yaml
kubectl -n routers get routerdeployments,routers,vyosconfigs
```

Changing `spec.template.spec.commands` in `config.yaml` reconfigures the
running routers, one after the other. Changing the image, a file or anything in
`machine.yaml` replaces them in place, one after the other.
