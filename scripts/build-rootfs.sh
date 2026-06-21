#!/usr/bin/env bash
#
# build-rootfs.sh — build a minimal Alpine ext4 rootfs with the guest agent baked
# in and auto-started by OpenRC on the vsock channel.
#
# Requires root (loop mount + apk --root). Env overrides: ALPINE (3.19),
# SIZE_MB (512), OUT (./rootfs.ext4), AGENT (./bin/guest_agent).
set -euo pipefail

ALPINE="${ALPINE:-3.19}"
SIZE_MB="${SIZE_MB:-512}"
OUT="${OUT:-$(pwd)/rootfs.ext4}"
AGENT="${AGENT:-$(pwd)/bin/guest_agent}"
REPO="$(cd "$(dirname "$0")/.." && pwd)"

[ "$(id -u)" -eq 0 ] || { printf '[!] run as root: sudo %s\n' "$0" >&2; exit 1; }
[ -f "$AGENT" ] || { printf '[!] guest agent not found at %s — run: make guest-agent\n' "$AGENT" >&2; exit 1; }
command -v mkfs.ext4 >/dev/null || { printf '[!] e2fsprogs (mkfs.ext4) required\n' >&2; exit 1; }

MNT="$(mktemp -d)"
cleanup() { umount "$MNT" 2>/dev/null || true; rmdir "$MNT" 2>/dev/null || true; }
trap cleanup EXIT

printf '[*] Creating %sMB ext4 image: %s\n' "$SIZE_MB" "$OUT"
dd if=/dev/zero of="$OUT" bs=1M count="$SIZE_MB" status=none
mkfs.ext4 -q -F "$OUT"
mount -o loop "$OUT" "$MNT"

printf '[*] Bootstrapping Alpine %s userland...\n' "$ALPINE"
if command -v apk >/dev/null; then
	apk add --root "$MNT" --initdb --no-cache \
		--repository "https://dl-cdn.alpinelinux.org/alpine/v${ALPINE}/main" \
		alpine-base bash python3 ca-certificates openrc
elif command -v docker >/dev/null; then
	printf '[*] apk not on host; bootstrapping via docker...\n'
	docker run --rm -v "$MNT:/rootfs" "alpine:${ALPINE}" sh -c \
		"apk add --root /rootfs --initdb --no-cache alpine-base bash python3 ca-certificates openrc"
else
	printf '[!] need either apk or docker on the host to bootstrap Alpine\n' >&2
	exit 1
fi

printf '[*] Creating unprivileged guest user (uid/gid 1000) and /workspace...\n'
chroot "$MNT" /bin/sh -eux <<'CHROOT'
addgroup -g 1000 agentuser 2>/dev/null || true
adduser -D -u 1000 -G agentuser -h /workspace agentuser 2>/dev/null || true
mkdir -p /workspace
chown 1000:1000 /workspace
chmod 0775 /workspace
CHROOT

printf '[*] Installing guest agent + OpenRC service...\n'
install -D -m 0755 "$AGENT" "$MNT/usr/local/bin/guest_agent"
install -D -m 0755 "$REPO/deploy/rootfs/guest-agent.openrc" "$MNT/etc/init.d/guest-agent"

chroot "$MNT" /bin/sh -eux <<'CHROOT'
rc-update add guest-agent default
# Bring up essential boot services; no network services are installed by design.
rc-update add devfs    sysinit 2>/dev/null || true
rc-update add procfs   boot    2>/dev/null || true
rc-update add localmount boot  2>/dev/null || true
CHROOT

sync
printf '[ok] rootfs built: %s\n' "$OUT"
printf '[i] No SSH / DHCP / network daemons installed (none-networking by design).\n'
