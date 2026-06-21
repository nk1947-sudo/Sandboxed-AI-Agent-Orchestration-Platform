#!/usr/bin/env bash
#
# check-kernel-config.sh — assert that the options Firecracker guests require are
# compiled built-in (=y), not as modules (=m) and not disabled. Used by
# build-kernel.sh and suitable as a CI lint (risk R12).
#
# Usage: check-kernel-config.sh [path-to-.config]   (default: .config)
set -euo pipefail

CFG="${1:-.config}"
[ -f "$CFG" ] || { printf '[!] config not found: %s\n' "$CFG" >&2; exit 1; }

# Must be built-in for the guest to mount its root device and reach the host
# over vsock without an initramfs or loadable modules.
required=(
	CONFIG_VSOCKETS
	CONFIG_VIRTIO_VSOCKETS
	CONFIG_VIRTIO
	CONFIG_VIRTIO_MMIO
	CONFIG_VIRTIO_BLK
	CONFIG_EXT4_FS
	CONFIG_SERIAL_8250
	CONFIG_SERIAL_8250_CONSOLE
)

fail=0
for opt in "${required[@]}"; do
	if grep -q "^${opt}=y" "$CFG"; then
		printf '[ok] %s=y\n' "$opt"
	else
		printf '[!] %s is NOT built-in (=y) in %s\n' "$opt" "$CFG" >&2
		fail=1
	fi
done

if [ "$fail" -ne 0 ]; then
	printf '[FAIL] required kernel options are missing or modular\n' >&2
	exit 1
fi
printf '[ok] all required kernel options are built-in\n'
