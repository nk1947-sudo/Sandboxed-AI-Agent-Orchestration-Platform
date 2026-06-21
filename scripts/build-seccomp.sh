#!/usr/bin/env bash
# build-seccomp.sh — Phase 6: compile vmm-filter.json → compiled BPF file.
#
# Requires: seccompiler-bin (ships with Firecracker releases or build from source).
# Usage:
#   scripts/build-seccomp.sh [--out /path/to/output.bpf]
#
# The compiled file is staged into the jailer chroot at boot time by the
# orchestrator (SeccompFilterPath config field). Firecracker receives it via
# --seccomp-filter=/jail/.../vmm-filter.bpf at start-up.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(dirname "$SCRIPT_DIR")"
FILTER_JSON="$REPO_ROOT/deploy/seccomp/vmm-filter.json"
OUT="${1:-$REPO_ROOT/deploy/seccomp/vmm-filter.bpf}"

# ── locate seccompiler-bin ────────────────────────────────────────────────────

SECCOMPILER=${SECCOMPILER_BIN:-}
if [[ -z "$SECCOMPILER" ]]; then
  for candidate in \
    "/usr/local/bin/seccompiler-bin" \
    "/usr/bin/seccompiler-bin" \
    "$REPO_ROOT/bin/seccompiler-bin"; do
    if [[ -x "$candidate" ]]; then
      SECCOMPILER="$candidate"
      break
    fi
  done
fi

if [[ -z "$SECCOMPILER" ]]; then
  echo "ERROR: seccompiler-bin not found."
  echo ""
  echo "Install options:"
  echo "  1. Download from the Firecracker release page and place in /usr/local/bin/"
  echo "  2. Build from source: cargo install seccompiler --path tools/seccompiler"
  echo "  3. Set SECCOMPILER_BIN=/path/to/seccompiler-bin"
  exit 1
fi

# ── compile ───────────────────────────────────────────────────────────────────

echo "[build-seccomp] Input : $FILTER_JSON"
echo "[build-seccomp] Output: $OUT"
echo "[build-seccomp] Using : $SECCOMPILER"

"$SECCOMPILER" \
  --input-file "$FILTER_JSON" \
  --output-file "$OUT" \
  --target-arch x86_64

echo "[build-seccomp] Compiled OK: $(du -h "$OUT" | cut -f1) BPF binary"
echo ""
echo "Set SeccompFilterPath in Config or SeccompFilterPath env to:"
echo "  $OUT"
echo ""
echo "The orchestrator stages this file into the jailer chroot at boot."
