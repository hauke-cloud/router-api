#!/usr/bin/env bash
#
# Creates and removes what `make e2e` needs at Hetzner Cloud besides a token:
# a network, a Primary IP per slot and a Floating IP.
#
#   hack/e2e-hetzner.sh setup <id> [env-file]   create them, write the E2E_* variables
#   hack/e2e-hetzner.sh teardown <id>           delete them, and any server still in the network
#
# Needs HCLOUD_TOKEN, curl and jq. <id> names the run; everything is labelled
# router-api-e2e=<id>, and teardown only touches what carries that label.
#
# Use a Hetzner project of its own for this. The test refuses to start in a
# project that already has servers managed by router-api, and when it is done
# it deletes every server, firewall and placement group in the project that
# router-api created.
set -euo pipefail

: "${HCLOUD_TOKEN:?HCLOUD_TOKEN is not set}"
action="${1:?usage: e2e-hetzner.sh setup|teardown <id> [env-file]}"
id="${2:?usage: e2e-hetzner.sh setup|teardown <id> [env-file]}"

API="https://api.hetzner.cloud/v1"
LOCATION="${E2E_LOCATION:-fsn1}"
NETWORK_ZONE="${E2E_NETWORK_ZONE:-eu-central}"
NETWORK_CIDR="${E2E_NETWORK_CIDR:-10.250.0.0/16}"
SUBNET_CIDR="${E2E_SUBNET_CIDR:-10.250.0.0/24}"
name="router-api-e2e-${id}"
label="router-api-e2e=${id}"

# api <method> <path> [json body]: prints the response, fails on an API error.
api() {
  local method="$1" path="$2" body="${3:-}" out
  out="$(curl --silent --show-error --max-time 60 --request "${method}" "${API}${path}" \
    --header "Authorization: Bearer ${HCLOUD_TOKEN}" --header 'Content-Type: application/json' \
    ${body:+--data "${body}"})"
  if [[ -n "${out}" ]] && jq -e '.error' >/dev/null 2>&1 <<<"${out}"; then
    echo "E: ${method} ${path}: $(jq -r '.error.message' <<<"${out}")" >&2
    return 1
  fi
  echo "${out}"
}

# ids <resource>: the IDs of this run's resources of that kind.
ids() {
  api GET "/$1?label_selector=${label}" | jq -r ".$1[].id"
}

setup() {
  local env_file="${1:-/dev/stdout}" labels
  labels="$(jq -cn --arg id "${id}" '{"router-api-e2e": $id}')"

  api POST /networks "$(jq -cn --arg name "${name}" --arg range "${NETWORK_CIDR}" --arg subnet "${SUBNET_CIDR}" \
    --arg zone "${NETWORK_ZONE}" --argjson labels "${labels}" \
    '{name: $name, ip_range: $range, labels: $labels, subnets: [{type: "cloud", ip_range: $subnet, network_zone: $zone}]}')" >/dev/null

  # auto_delete off: the addresses are what stays when a router is replaced.
  for slot in 0 1; do
    api POST /primary_ips "$(jq -cn --arg name "${name}-${slot}" --arg location "${LOCATION}" --argjson labels "${labels}" \
      '{name: $name, type: "ipv4", location: $location, assignee_type: "server", auto_delete: false, labels: $labels}')" >/dev/null
  done

  local floating
  floating="$(api POST /floating_ips "$(jq -cn --arg name "${name}" --arg location "${LOCATION}" --argjson labels "${labels}" \
    '{name: $name, type: "ipv4", home_location: $location, labels: $labels}')" | jq -r '.floating_ip.ip')"

  # Where the operator's connections to the routers will come from.
  local address
  address="$(curl --silent --show-error --max-time 20 https://api.ipify.org)"
  [[ "${address}" =~ ^[0-9.]+$ ]] || { echo "E: could not determine this machine's public address" >&2; exit 1; }

  {
    echo "E2E_NETWORK=${name}"
    echo "E2E_NETWORK_CIDR=${NETWORK_CIDR}"
    echo "E2E_PRIMARY_IPS=${name}-0,${name}-1"
    echo "E2E_FLOATING_IP=${floating}"
    echo "E2E_MANAGEMENT_CIDR=${address}/32"
  } >> "${env_file}"
  echo "I: created network, Primary IPs and Floating IP ${name}" >&2
}

teardown() {
  local network server item
  # Servers a run left behind, when it was cancelled for instance. They are
  # the ones in this run's network.
  for network in $(ids networks); do
    for server in $(api GET "/networks/${network}" | jq -r '.network.servers[]'); do
      echo "I: deleting left-over server ${server}" >&2
      api DELETE "/servers/${server}" >/dev/null || true
    done
  done

  # A Primary IP and a network are only free once the server is really gone.
  for kind in floating_ips primary_ips networks; do
    for item in $(ids "${kind}"); do
      for attempt in $(seq 1 30); do
        if api DELETE "/${kind}/${item}" >/dev/null 2>&1; then
          break
        fi
        [[ "${attempt}" == 30 ]] && { echo "E: could not delete ${kind}/${item}" >&2; exit 1; }
        sleep 4
      done
    done
  done
  echo "I: removed ${name}" >&2
}

case "${action}" in
  setup) setup "${3:-}" ;;
  teardown) teardown ;;
  *) echo "usage: e2e-hetzner.sh setup|teardown <id> [env-file]" >&2; exit 2 ;;
esac
