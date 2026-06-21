#!/usr/bin/env bash
# vm-tap.sh — Phase 6: create or destroy a restricted TAP device for one VM.
#
# Each VM that needs egress networking gets its own TAP, assigned a unique IP
# in the sandbox subnet (10.200.0.0/24). The TAP is owned by jaileruser so
# the Firecracker VMM can open it from inside the chroot.
#
# Usage:
#   vm-tap.sh create <vm-id> <tap-index>
#     vm-id       : sandbox ID (e.g. "sb-abc123")
#     tap-index   : integer 1-253 → guest IP 10.200.0.<index>
#
#   vm-tap.sh destroy <vm-id>
#
# Environment:
#   JAILER_USER   (default: jaileruser)
#   BRIDGE_IP     (default: 10.200.0.1 — host side of the bridge)
#   SUBNET        (default: 10.200.0.0/24)
#
# Requires: ip(8) (iproute2), iptables or nft already configured.
set -euo pipefail

JAILER_USER=${JAILER_USER:-jaileruser}
BRIDGE_IP=${BRIDGE_IP:-10.200.0.1}
SUBNET=${SUBNET:-10.200.0.0/24}
BRIDGE=sdbr0

ACTION="${1:-}"
VM_ID="${2:-}"

usage() {
  echo "Usage: $0 create <vm-id> <tap-index>"
  echo "       $0 destroy <vm-id>"
  exit 1
}

[[ -z "$ACTION" || -z "$VM_ID" ]] && usage

TAP_NAME="vmtap-${VM_ID:0:12}"   # TAP names are max 15 chars

create_bridge_if_needed() {
  if ! ip link show "$BRIDGE" &>/dev/null; then
    echo "[vm-tap] Creating bridge $BRIDGE ($BRIDGE_IP)"
    ip link add "$BRIDGE" type bridge
    ip addr add "${BRIDGE_IP}/24" dev "$BRIDGE"
    ip link set "$BRIDGE" up
  fi
}

case "$ACTION" in
  create)
    TAP_INDEX="${3:-}"
    [[ -z "$TAP_INDEX" ]] && usage
    GUEST_IP="10.200.0.${TAP_INDEX}"

    echo "[vm-tap] Creating $TAP_NAME for VM $VM_ID (guest IP $GUEST_IP)"
    create_bridge_if_needed

    # Create TAP owned by jaileruser so the VMM (demoted to jaileruser) can open it.
    ip tuntap add dev "$TAP_NAME" mode tap user "$JAILER_USER"
    ip link set "$TAP_NAME" up
    ip link set "$TAP_NAME" master "$BRIDGE"

    # Record the TAP → VM mapping for cleanup.
    mkdir -p /run/sandbox-taps
    echo "$VM_ID $TAP_NAME $GUEST_IP" > "/run/sandbox-taps/${VM_ID}"

    echo "[vm-tap] Created: $TAP_NAME (bridge: $BRIDGE, guest: $GUEST_IP, host: $BRIDGE_IP)"
    echo "Set these in the Firecracker network_interfaces config:"
    echo "  host_dev_name: $TAP_NAME"
    echo "  guest_mac:     $(printf '52:54:00:%02x:%02x:%02x' 0 0 "$TAP_INDEX")"
    ;;

  destroy)
    RECORD="/run/sandbox-taps/${VM_ID}"
    if [[ -f "$RECORD" ]]; then
      read -r _ TAP_NAME _ < "$RECORD"
    fi

    if [[ -n "${TAP_NAME:-}" ]] && ip link show "$TAP_NAME" &>/dev/null; then
      echo "[vm-tap] Removing $TAP_NAME"
      ip link set "$TAP_NAME" nomaster 2>/dev/null || true
      ip link delete "$TAP_NAME" 2>/dev/null || true
    fi

    rm -f "/run/sandbox-taps/${VM_ID}"
    echo "[vm-tap] Destroyed TAP for $VM_ID"
    ;;

  *)
    usage
    ;;
esac
