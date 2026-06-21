#!/usr/bin/env bash
#
# host-kvm-setup.sh — verify the host can run Firecracker microVMs and grant the
# current user access to /dev/kvm. Idempotent; safe to re-run.
#
# Run on the Linux host (bare-metal, a nested-virt cloud instance, or WSL2 with
# nested virtualization enabled).
set -euo pipefail

say()  { printf '[*] %s\n' "$*"; }
ok()   { printf '[ok] %s\n' "$*"; }
fail() { printf '[!] %s\n' "$*" >&2; exit 1; }

say "Checking CPU virtualization extensions (vmx/svm)..."
if [ "$(grep -Ec '(vmx|svm)' /proc/cpuinfo)" -eq 0 ]; then
	fail "No vmx/svm in /proc/cpuinfo. Enable VT-x/AMD-V in firmware, use a *.metal
     or nested-virt cloud instance, or enable WSL2 nested virtualization."
fi
ok "Hardware virtualization is available."

say "Checking /dev/kvm..."
[ -e /dev/kvm ] || fail "/dev/kvm missing. On WSL2 enable nested virtualization; on cloud use a nested-virt/metal instance."

say "Granting access to /dev/kvm (kvm group)..."
sudo groupadd -f kvm
sudo usermod -aG kvm "$USER" || true
sudo chgrp kvm /dev/kvm || true
sudo chmod g+rw /dev/kvm || true

if [ -w /dev/kvm ]; then
	ok "/dev/kvm is writable."
else
	say "/dev/kvm not writable in this shell yet — run 'newgrp kvm' or re-login."
fi

cat <<'NEXT'

[next] Install the Firecracker + jailer binaries (pin a release):
       https://github.com/firecracker-microvm/firecracker/releases
       Place them on PATH (or set FC_BIN / JAILER_BIN for the control plane).

[next] Build the guest images:
       scripts/build-kernel.sh        # -> vmlinux-6.1
       make guest-agent               # -> bin/guest_agent
       sudo scripts/build-rootfs.sh   # -> rootfs.ext4
NEXT
