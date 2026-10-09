# Writing a router's configuration

A `VyOSConfigTemplate` describes the routers of a group. Its `spec.template.spec` has two kinds
of fields, and the difference matters:

| Fields | Reach the router | A change |
| --- | --- | --- |
| `commands`, `values`, `management` | through the REST API | is applied to the running routers, one at a time |
| `image`, `files`, `host` | in the user data the server is created with | replaces the routers, one at a time |

## Commands

`commands` is the router's configuration as VyOS `set` commands, one per line. Empty lines and
lines starting with `#` are ignored. `delete` is not allowed, and not needed:

**The configuration is the whole truth.** The router is made to match it. Whatever is
configured on the router and not listed is removed, including what someone changed by hand. Only
these are exempt:

- what the operator itself needs: its certificate and API key, the HTTPS service, and
  `system config-management`. Commands of yours in those places are ignored.
- `interfaces ethernet <name> hw-id`, which VyOS writes itself.

And three things are filled in *unless you configure them yourself*:

- `address dhcp` on the host's public and private interface (`eth0`, `eth1`), so that a
  configuration that does not mention them does not make the router unreachable;
- the default `vyos` account, with its password locked: VyOS ships it with a well-known one.
  Configure any `system login user` yourself and this is left out.

Values with spaces need quotes, as on the VyOS command line:
`set interfaces ethernet eth0 description 'to the internet'`.

## Templates

`commands` and the content of every file are Go templates.

| | |
| --- | --- |
| `.Values.<name>` | a value from `spec.values` |
| `.Router.Name`, `.Router.Namespace` | the Router object |
| `.Router.Group` | the name of its RouterDeployment |
| `.Router.Slot` | its slot, if the deployment uses the Slots strategy (0 otherwise) |
| `.Machine.ExternalIP`, `.Machine.ExternalIPv6`, `.Machine.InternalIP` | the server's addresses |
| `.Peers` | the other routers of the group, sorted by name: `.Name`, `.Slot`, `.ExternalIP`, `.InternalIP` |
| `.Host.PublicInterface`, `.Host.PrivateInterface` | `eth0` and `eth1` unless `spec.host` says otherwise |

Functions: `quote` (make a value one token), `default "fallback" value`, `required "message"
value`, `join "," list`, `lower`, `upper`.

A value that does not exist is an error, not an empty string. The configuration is then not
applied and the router keeps what it has; `ConfigApplied` says what was wrong.

`.Machine` and `.Peers` are empty when files are rendered: files become part of the user data,
before the server exists.

`.Peers` changes when a router of the group is added or removed. Every remaining router is then
reconfigured in place. This is what keeps unicast VRRP peers current through a rollout.

## Values

```yaml
values:
  - name: zone
    value: Europe/Berlin
  - name: wireguardKey
    secretKeyRef: {name: edge-wireguard, key: privateKey}
  - name: motd
    configMapKeyRef: {name: edge, key: motd}
```

Values from Secrets are removed from anything the operator writes to an object's status, such as
the router's explanation of why it refused a commit.

## Values per slot

In a group whose RouterDeployment uses the Slots strategy every router has a
slot that survives its replacement. What a router has of its own goes under
`slots`, one entry per slot:

```yaml
values:
  - name: labPublicKey
    value: ...
slots:
  - values:                      # slot 0
      - name: tunnelLocal
        value: 10.99.0.1
      - name: wireguardPrivateKey
        secretKeyRef: {name: edge-wireguard, key: privateKey0}
  - values:                      # slot 1
      - name: tunnelLocal
        value: 10.99.0.5
      - name: wireguardPrivateKey
        secretKeyRef: {name: edge-wireguard, key: privateKey1}
```

The templates see them as `.Values.<name>` like any other value; a slot's value
wins over a shared one of the same name. If `slots` is set there has to be an
entry for every slot of the group. A router whose slot has none is not
configured at all, rather than with another router's values.

## Files

```yaml
files:
  - path: /config/hetzner/failover.json
    permissions: "0644"
    value: |
      {"tokenFile": "/config/hetzner/token", "groups": {"wan": {"floatingIPs": ["203.0.113.10"]}}}
```

Paths have to be below `/config`, the one directory VyOS keeps. Files are written when the
server is created and never again; change one and the routers are replaced.

## Management

```yaml
management:
  port: 443                  # the REST API's port
  addressType: ExternalIP    # which of the machine's addresses the operator connects to
  allowedSources: []         # restricts the API on the router itself; leave empty if the
                             # operator's address changes
  confirmTimeoutMinutes: 2   # how long the router waits for a commit to be confirmed
```

`confirmTimeoutMinutes` is how long a change that cut the operator off stays in effect before
the router undoes it.

## What happens when a change is wrong

| | Result |
| --- | --- |
| The template does not render | Nothing is sent. `ConfigApplied=False`, reason `RenderFailed`. |
| VyOS rejects the commit | The operator puts the previous configuration back (VyOS keeps the part of a failed commit that applied). `ConfigApplied=False`, reason `CommitFailed`, with the router's message. The same configuration is not tried again for ten minutes, or until you change it. |
| The commit succeeds and the operator can no longer reach the router | It is never confirmed; the router reverts by itself after `confirmTimeoutMinutes`. `ConfigApplied=False`, reason `Unconfirmed`. |

In each case the router stays `Ready`, the rollout stops at this router, and the other routers
of the group never see the change.
