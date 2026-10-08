#!/usr/bin/env bash
#
# Hands the host's network interfaces to VyOS and starts it.
#
# VyOS only manages interfaces named ethN and expects to be the only thing
# configuring them. Ubuntu names the private interface after its PCI slot and
# has netplan configure both. So, once everything that needs the network has
# been fetched: name the interfaces, take them away from netplan, start VyOS.
set -euo pipefail

IMAGE="$1"
PUBLIC_NAME="$2"
PRIVATE_NAME="$3"

log() { echo "router-api-host-setup: $*"; }

# VyOS loads these on demand, which works from inside a privileged container
# as long as /lib/modules is the host's. Loading them here as well makes a
# missing module fail now and visibly rather than at the first commit.
modprobe -a wireguard nf_conntrack nf_tables 2>/dev/null || log "could not preload kernel modules"

# Needs the network, so it has to happen before the hand-over. An image that
# is already there, as in a pre-baked snapshot, is not fetched again.
if podman image exists "${IMAGE}"; then
  log "${IMAGE} is already present"
else
  log "pulling ${IMAGE}"
  for attempt in 1 2 3 4 5; do
    podman pull "${IMAGE}" && break
    [ "${attempt}" = 5 ] && { log "could not pull ${IMAGE}"; exit 1; }
    sleep 10
  done
fi

public="$(ip -o -4 route show default | awk '{print $5; exit}')"
[ -n "${public}" ] || { log "no default route, cannot tell which interface is public"; exit 1; }

mapfile -t others < <(ip -o link show | awk -F': ' '{print $2}' | cut -d@ -f1 \
  | grep -E '^(en|eth)' | grep -vx "${public}" | sort)

# Persist the names by MAC address, for every boot after this one.
link() {
  local current="$1" name="$2" mac
  mac="$(cat "/sys/class/net/${current}/address")"
  printf '[Match]\nMACAddress=%s\n\n[Link]\nName=%s\n' "${mac}" "${name}" \
    > "/etc/systemd/network/10-router-api-${name}.link"
}
link "${public}" "${PUBLIC_NAME}"
[ "${#others[@]}" -gt 0 ] && link "${others[0]}" "${PRIVATE_NAME}"

# The host stops configuring the network, now and after a reboot.
mkdir -p /etc/cloud/cloud.cfg.d
echo 'network: {config: disabled}' > /etc/cloud/cloud.cfg.d/99-router-api-network.cfg
rm -f /etc/netplan/*.yaml
systemctl disable --now systemd-networkd.socket systemd-networkd.service 2>/dev/null || true

rename() {
  local current="$1" name="$2"
  [ "${current}" = "${name}" ] && return 0
  ip link set dev "${current}" down
  ip link set dev "${current}" name "${name}"
}
log "handing ${public} to VyOS as ${PUBLIC_NAME}"
ip addr flush dev "${public}" || true
rename "${public}" "${PUBLIC_NAME}"
if [ "${#others[@]}" -gt 0 ]; then
  log "handing ${others[0]} to VyOS as ${PRIVATE_NAME}"
  ip addr flush dev "${others[0]}" || true
  rename "${others[0]}" "${PRIVATE_NAME}"
fi

# From here on the host has no network of its own. If VyOS does not start,
# give it one back: a server that can be logged in to can be debugged, one
# that cannot has to be rescued.
recover() {
  log "starting VyOS failed, restoring the host's own network"
  rm -f /etc/cloud/cloud.cfg.d/99-router-api-network.cfg
  {
    echo "network:"
    echo "  version: 2"
    echo "  ethernets:"
    for name in "${PUBLIC_NAME}" "${PRIVATE_NAME}"; do
      [ -e "/sys/class/net/${name}" ] && printf '    %s:\n      dhcp4: true\n' "${name}"
    done
  } > /etc/netplan/99-router-api-recovery.yaml
  chmod 600 /etc/netplan/99-router-api-recovery.yaml
  systemctl enable --now systemd-networkd.service || true
  netplan apply || true
}
trap recover ERR

# The unit is generated from the quadlet file at daemon-reload and takes its
# place in the boot from the file's [Install] section. It cannot be enabled,
# only started.
systemctl daemon-reload
systemctl start vyos.service
log "VyOS started"
