#!/usr/bin/env bash
#
# build-kernel.sh — build a minimal Firecracker guest kernel (vmlinux).
#
# Strategy: start from Firecracker's published microVM kernel config, append our
# required-config.fragment, resolve with `make olddefconfig`, assert the must-have
# options are built-in, then build a raw vmlinux (NOT bzImage).
#
# Env overrides: KVER (6.1), WORK (/tmp/linux-build), OUT (./vmlinux-<KVER>).
set -euo pipefail

KVER="${KVER:-6.1}"
ARCH="x86_64"
WORK="${WORK:-/tmp/linux-build}"
OUT="${OUT:-$(pwd)/vmlinux-${KVER}}"

REPO="$(cd "$(dirname "$0")/.." && pwd)"
FRAG="${REPO}/deploy/kernel/required-config.fragment"
SAVED_CONFIG="${REPO}/deploy/kernel/microvm-kernel-${ARCH}-${KVER}.config"
FCCFG_URL="https://raw.githubusercontent.com/firecracker-microvm/firecracker/main/resources/guest_configs/microvm-kernel-${ARCH}-${KVER}.config"

for tool in git make curl; do
	command -v "$tool" >/dev/null || { printf '[!] %s is required\n' "$tool" >&2; exit 1; }
done

mkdir -p "$WORK"
cd "$WORK"
if [ ! -d linux ]; then
	printf '[*] Cloning Linux v%s (shallow)...\n' "$KVER"
	git clone --depth 1 --branch "v${KVER}" \
		https://git.kernel.org/pub/scm/linux/kernel/git/stable/linux.git
fi
cd linux

printf '[*] Fetching Firecracker microVM kernel config...\n'
if ! curl -fsSL "$FCCFG_URL" -o .config; then
	[ -f "$SAVED_CONFIG" ] || { printf '[!] could not fetch config and no saved copy at %s\n' "$SAVED_CONFIG" >&2; exit 1; }
	printf '[*] Falling back to saved config %s\n' "$SAVED_CONFIG"
	cp "$SAVED_CONFIG" .config
fi

printf '[*] Merging required CONFIG fragment...\n'
cat "$FRAG" >> .config
make ARCH="$ARCH" olddefconfig

printf '[*] Verifying required options are built-in...\n'
"${REPO}/scripts/check-kernel-config.sh" .config

printf '[*] Saving resolved config to %s\n' "$SAVED_CONFIG"
cp .config "$SAVED_CONFIG"

printf '[*] Building vmlinux with %s jobs (this takes a while)...\n' "$(nproc)"
make ARCH="$ARCH" -j"$(nproc)" vmlinux

cp vmlinux "$OUT"
printf '[ok] kernel built: %s\n' "$OUT"
