# Example: a home lab behind a pair of Hetzner routers

The lab has no stable public address. A pair of VyOS routers at Hetzner Cloud is
the stable side: they hold the Floating IPs, the lab's router dials out to them,
and everything addressed to the lab's public IPs is routed (not NATed) through
that tunnel.

```
                 Floating IP "vpn"  ─┐
internet ──────► Floating IPs "svc" ─┼─► active router ══ WireGuard ══► lab router ──► Cilium LoadBalancer IPs
                                     │   (VRRP master)      (dialled by the lab)        (announced by BGP)
                                     └─► standby router
```

- **Two routers**, one active. They elect a master with unicast VRRP over the
  Hetzner private network. Hetzner does not move addresses on a gratuitous ARP,
  so on becoming master a router runs `hcloud-vrrp-failover`, which assigns the
  Floating IPs to itself through the API.
- **One Floating IP is the VPN endpoint.** Both routers carry it on a dummy
  interface and share one WireGuard key, so the lab's router keeps dialling the
  same address with the same peer after a failover. The lab side sets
  `persistent-keepalive`; the Hetzner side has no endpoint for the lab at all,
  which is what makes a rotating lab address a non-issue.
- **The other Floating IPs are the lab's public IPs.** The routers do not hold
  them. They forward them into the tunnel, any protocol, any port. Which
  addresses those are is learned by **eBGP** from the lab router over the
  tunnel.
- **The operator runs in the lab** and reaches the routers on their own public
  addresses. The Hetzner firewall lets the management port through from the
  lab's dynamic DNS name, which the Hetzner provider resolves every minute.

## What you have to bring

| | |
| --- | --- |
| A Hetzner Cloud Network `lab` with a subnet | the routers attach to it; VRRP runs over it |
| Floating IPs | one for the VPN, one or more for services; created by you, because they have to outlive any router |
| A Hetzner API token, read and write | in `secret.example.yaml`; used by the operator and, on the routers, by the failover helper |
| A WireGuard key pair for the Hetzner side, and the lab router's public key | `secret.example.yaml` |
| A DNS name that follows the lab's public address | `network.yaml` |

On the lab router (OPNsense): a WireGuard peer pointing at the VPN Floating IP
with a keepalive, an eBGP session to `10.99.0.1` announcing the service
addresses, and policy routing so that replies *from* those addresses leave
through the tunnel rather than through the lab's own uplink. Cilium's sessions
with the lab router are a separate matter: routes learned over iBGP are not
passed on to another iBGP peer, which is why the session to Hetzner is eBGP
with AS numbers of its own.

## Status of this example

- The manifests are submitted to a real API server and rendered by
  `internal/examples/examples_test.go`.
- The VyOS configuration in `config.yaml` was committed in full, firewall, NAT
  and WireGuard included, on two Hetzner Cloud servers that were booted from the
  operator's user data (`TestHomeLabExampleOnAServer`). The two elected one
  master over the private network, and a hard power-off of the master moved the
  Floating IP and the alias IP to the other within six seconds.
- **Not tested: the tunnel and BGP with a peer on the other end.** No lab
  router was connected. The WireGuard interface and the BGP session are
  configured and come up; that traffic flows through them as described is how
  it is designed, not something that was observed.

[docs/spike-vyos-container.md](../../docs/spike-vyos-container.md) has the
details.

## Apply

```sh
kubectl create namespace routers
kubectl -n routers apply -f secret.example.yaml   # after filling it in
kubectl -n routers apply -f network.yaml -f machine.yaml -f config.yaml -f routers.yaml
kubectl -n routers get routerdeployments,routers,vyosconfigs
```

Changing `spec.template.spec.commands` in `config.yaml` reconfigures the
running routers, the standby first. Changing the image, a file or anything in
`machine.yaml` replaces them one at a time.
