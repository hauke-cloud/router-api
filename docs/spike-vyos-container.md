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

The revert action defaults to **reboot**. In a container that stops the
container. With `set system config-management commit-confirm action reload`
the revert happens in place; the seed configuration sets it.

Every command output is prefixed with `sudo: unable to resolve host <name>`
after a hostname change. It is noise in the `data` field and has to be ignored
when parsing.

## Features exercised

- BGP: `protocols bgp` commits, `show bgp summary` reports the neighbour.
- Metrics: `service monitoring prometheus node-exporter` serves on 9100.
- WireGuard key generation through `/generate`.

## Limits of this spike

Rootless podman cannot write non-namespaced sysctls or load kernel modules, so
on the workstation:

- any `firewall` commit fails (`sysctl -f` is denied),
- `interfaces wireguard` fails (`Loading Kernel module wireguard failed`; the
  module is not loaded on the workstation either),
- `system option` fails at boot (`/proc/sys/kernel/panic`), which leaves
  `/tmp/vyos-config-status` at 1 and systemd `degraded`,
- the only NIC is pasta's copy of the host interface (`enp4s0`), which VyOS
  does not treat as an `ethN` interface.

None of these say anything about a rootful, host-network container on a real
server. Still to be shown there, before the bootstrap half of the config
provider is written against it:

1. VyOS taking over the NICs of an Ubuntu 24.04 Hetzner server: who configures
   `eth0` when netplan and VyOS both want to, how the public and the private NIC
   end up named, and whether the operator can still reach the API afterwards.
2. `firewall`, `nat` and `interfaces wireguard` committing, with the kernel
   modules coming from the host through `/lib/modules`.
3. Unicast VRRP between two servers over the private network, and the
   transition script moving a Floating IP.
4. The size of the bootstrap user data against Hetzner's limit.
