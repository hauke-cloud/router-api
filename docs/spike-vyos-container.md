# Spike: VyOS in a container, driven through its REST API

What was actually observed, on 2026-10-08, with VyOS `2026.10.07-0712-rolling`
converted by `vyos-build/scripts/iso-to-oci` (commit `f5a1e83`) and run under
rootless podman 6.1 on a workstation. Everything the operator design relies on
is listed here with its evidence; the last section is what is still unproven.

## Image

| Finding | Consequence |
| --- | --- |
| Unpacking the squashfs as a regular user makes every file belong to that user. VyOS then boots but cannot commit: `sudo: /etc/sudoers is owned by uid 1000`. | The rootfs is built as root or under `fakeroot` (`scripts/build-rootfs.sh` enforces it). |
| `podman import` of the 350 MB `.tar.xz` did not finish in ten minutes (single-byte reads). | The image is built from a `FROM scratch` Dockerfile with `ADD`. |
| podman drops `HEALTHCHECK` from OCI-format images. | Built with `--format docker` under podman. |
| The image has no `/etc/ssl/certs/ssl-cert-snakeoil.pem`. With `service https` and no certificate, nginx fails to start and the API never answers. | The seed configuration always carries a PKI certificate and `service https certificates certificate`. |
| The default `vyos` user has the well-known default password. | The seed configuration replaces the login. |
| Boot to `vyos-router` active: about 15 s. | |

## REST API

All calls made with the certificate pinned (`curl --cacert`), not `-k`.

| Call | Result |
| --- | --- |
| `GET /info` | 200 without a key: banner, hostname, version. Usable as an unauthenticated liveness probe. |
| any `POST` with a wrong key | 401 `Valid API key is required` |
| `/show` `["configuration","commands"]` | The running config as `set` lines, one per line, values single-quoted. **Includes private keys and password hashes** — never log it. |
| `/retrieve` `exists`, `returnValues`, `showConfig` | As documented; `showConfig` returns JSON. |
| `/configure` with a list of ops | One commit. An invalid value fails the whole request with 400 before anything is applied. |
| `/configure`, ops spanning components, one component failing | 400, but the components that succeeded **stay committed**. A failed apply is not a no-op. |
| `/configure` with `confirm_time`, one component failing | 400, the revert timer is **not armed** (`commit-confirm.timer` inactive, `confirm` answers `No confirm pending`) and the components that succeeded stay committed. Commit-confirm does not clean up a failed commit; the caller has to put the previous configuration back. |
| `/configure` delete of a missing path | 200 (strict mode is off by default). |
| `/configure` JSON body `{"key","confirm_time":N,"commands":[...]}` | Applies and arms a revert timer. `N` is in **minutes**, minimum 1. |
| `/config-file` `{"op":"confirm"}` | `Reboot timer stopped` / `Reload timer stopped`. With nothing pending: 200 `No confirm pending`. |
| not confirming | After the timeout the change is gone. It reverts to the commit before it, not to the saved config: an earlier commit that was confirmed but never saved survived. |
| `/config-file` `merge` with `string` and `confirm_time` | Works the same way. |
| `/config-file` `save` | Writes `/config/config.boot`. Nothing else persists a change. |

`commit-confirm action reload` needs `system config-management
commit-revisions` to be set: without it VyOS rejects the commit with
`commit-confirm reload requires non-zero commit-revisions`. It is part of the
default configuration, so a configuration that is made to match a list
exactly has to list it.

The revert action defaults to **reboot**. In a container that stops the
container. With `set system config-management commit-confirm action reload`
the revert happens in place; the seed configuration sets it.

Every command output is prefixed with `sudo: unable to resolve host <name>`
after a hostname change. It is noise in the `data` field and has to be ignored
when parsing.

## Bootstrap without touching the router

A script at `/config/scripts/vyos-postconfig-bootup.script` is run by VyOS
after it has loaded its configuration, on every boot. Placed in the config
volume before the container first starts, it seeds the router with no access
from outside:

| Finding | Consequence |
| --- | --- |
| A vbash script there that does `configure`, `set ...`, `commit`, `save` brought the API up with the given key and certificate about 18 s after `podman run`. | The bootstrap data only has to put files into the config volume. Nothing has to reach into the container. |
| Statements after the script's `exit` (which leaves configuration mode) still run. | The script writes a marker file last and does nothing when it finds it, so a later change of the API key is not undone at the next boot. |
| The seeded configuration is in `config.boot` and the API answers again after a container restart. | |
| `set system login user vyos authentication encrypted-password '!'` commits and leaves the account without a usable password. Deleting the last user, and all of `system login`, also commits. | The seed locks the default account; a configuration without any login is possible. |

## VRRP

On a dummy interface, so this says nothing about two routers seeing each other:

- A unicast group comes up and `show vrrp` prints one row per group:
  `Name Interface VRID State Priority Last Transition`, state `MASTER`.
- On becoming master VyOS ran the transition script:
  `keepalived-fifo.py: Running the command: /usr/local/bin/hcloud-vrrp-failover wan`.
- `set high-availability disable` stops keepalived; `show vrrp` then prints
  `VRRP data is not available (...)`. That is what a drain uses: a stopping
  keepalived announces priority 0 and a peer takes over at once.

## Features exercised

- BGP: `protocols bgp` commits, `show bgp summary` reports the neighbour.
- Metrics: `service monitoring prometheus node-exporter` serves on 9100.
- WireGuard key generation through `/generate`.

## On Hetzner Cloud

Run on 2026-10-08 with `cx23` servers in `fsn1`, Ubuntu 24.04.4 (kernel
6.8.0-138), podman 4.9.3, created from a snapshot that had the VyOS image
loaded and booted with the user data `go run ./hack/userdata` prints, which is
what the operator generates.

| Finding | Consequence |
| --- | --- |
| Stock Ubuntu names the public interface `eth0` and the private one `enp7s0`, both configured by netplan through systemd-networkd, with the names pinned by MAC address in the netplan file. | The host set-up script names the interfaces by MAC with `.link` files, removes the netplan configuration, disables cloud-init's network configuration and stops systemd-networkd. |
| With that, VyOS in a privileged host-network container took over both interfaces: `eth0` and `eth1` got their addresses from VyOS's DHCP client. The REST API answered on the public address, with the generated certificate, **38 s after the server was created**; a second server took about 25 s. SSH to the host kept working. | The bootstrap works as designed. |
| A unit generated from a quadlet file cannot be enabled (`Unit ... is transient or generated`), only started; it takes its place in the boot from the file's `[Install]` section. The first attempt failed on this after the interfaces had been handed over, and the server had to be read through the rescue system. | The script starts the unit, and if anything fails after the hand-over it gives the host a DHCP configuration back, so that a failed bootstrap can be logged in to. |
| An unqualified `FROM golang:...` does not resolve under Ubuntu's podman, which has no search registries configured. | Both Dockerfiles name their base images in full. |
| Firewall, NAT, WireGuard, BGP and VRRP of `examples/home-lab` committed, and a second apply found nothing to change. `wg0` came up: the module is loaded from the host's `/lib/modules` by the privileged container. | What a rootless container could only validate is committed for real. |
| VyOS's DHCP client takes the private address, a /32, **but not the classless routes Hetzner sends with it**. Ubuntu had `10.0.0.0/16 via 10.0.0.1` and `10.0.0.1 dev enp7s0`; VyOS had neither, the routers could not reach each other, and both became VRRP master. | The configuration has to carry the two routes. The example does. |
| With the routes, unicast VRRP over the private network works: one master, one backup, advertisements visible on the wire. | |
| On becoming master a router ran `hcloud-vrrp-failover`, and Hetzner showed the Floating IP and the alias IP assigned to it. | The on-box failover works without the operator. |
| **Failover: the master was powered off hard while its Floating IP was pinged five times a second. The address was unreachable for 6.0 s.** The backup became master about 4 s after the power-off was issued, moved both addresses, and answered. | |
| Hetzner's API specification limits user data to 32 KiB; the user data for the example is 8.3 KB. | |

A router's firewall is the host's: VyOS runs in the host's network namespace.
A rule set with a default of drop on input also closes SSH to the host unless
it says otherwise.

## Not established

- Traffic through the WireGuard tunnel, and BGP with a peer: no lab router was
  connected in any test.
- IPv6. The servers had IPv6 addresses; nothing was done with them.
