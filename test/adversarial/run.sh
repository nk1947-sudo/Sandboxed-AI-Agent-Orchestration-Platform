#!/usr/bin/env bash
# run.sh — Phase 8 adversarial security test runner.
#
# Orchestrates the three isolation scenarios and prints a summary report.
# Each scenario delegates the actual VM interaction to Go integration tests;
# the shell scripts add host-side metric collection and final assertions.
#
# Usage:
#   sudo FC_KERNEL=/path/to/vmlinux FC_ROOTFS=/path/to/rootfs.ext4 \
#       test/adversarial/run.sh
#
# Environment:
#   FC_KERNEL    — host path to vmlinux (required)
#   FC_ROOTFS    — host path to rootfs.ext4 (required)
#   FC_BIN       — firecracker binary path (default: firecracker)
#   RUN_ONLY     — space-separated scenario numbers, e.g. "01 03" (default: all)
#
# Exit code:
#   0 — all scenarios passed
#   1 — one or more scenarios failed or prerequisites not met
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

# ── prerequisites ─────────────────────────────────────────────────────────────

check_prereqs() {
  local ok=1

  for cmd in go jq awk sha256sum; do
    if ! command -v "$cmd" &>/dev/null; then
      echo "ERROR: required command not found: $cmd" >&2
      ok=0
    fi
  done

  if [[ ! -e /dev/kvm ]]; then
    echo "ERROR: /dev/kvm not available — adversarial tests require a KVM host" >&2
    ok=0
  fi

  if [[ -z "${FC_KERNEL:-}" ]]; then
    echo "ERROR: FC_KERNEL must be set to the vmlinux path" >&2
    ok=0
  elif [[ ! -f "$FC_KERNEL" ]]; then
    echo "ERROR: FC_KERNEL=${FC_KERNEL} does not exist" >&2
    ok=0
  fi

  if [[ -z "${FC_ROOTFS:-}" ]]; then
    echo "ERROR: FC_ROOTFS must be set to the rootfs.ext4 path" >&2
    ok=0
  elif [[ ! -f "$FC_ROOTFS" ]]; then
    echo "ERROR: FC_ROOTFS=${FC_ROOTFS} does not exist" >&2
    ok=0
  fi

  if [[ $EUID -ne 0 ]]; then
    echo "WARNING: not running as root — cgroup writes may fail" >&2
  fi

  (( ok )) || exit 1
}

# ── scenario runner ───────────────────────────────────────────────────────────

run_scenario() {
  local num="$1"
  local script
  # Find script matching the given prefix (e.g. "01" → "01-forkbomb.sh").
  script=$(ls "${SCRIPT_DIR}/${num}-"*.sh 2>/dev/null | head -1 || true)
  if [[ -z "$script" ]]; then
    echo "  SKIP: scenario $num — script not found"
    return 0
  fi

  local name
  name=$(basename "$script" .sh)
  echo
  echo "▶  Scenario ${num}: ${name}"
  echo "   $(date -u +%H:%M:%SZ)"

  if bash "$script"; then
    return 0
  else
    return 1
  fi
}

# ── main ──────────────────────────────────────────────────────────────────────

main() {
  check_prereqs

  # Compile the integration test binary once up front so each scenario doesn't
  # recompile it. go test -c writes to a temp file we control.
  TESTBIN=$(mktemp /tmp/adversarial-tests-XXXXXX)
  trap 'rm -f "$TESTBIN"' EXIT
  echo "Compiling adversarial test binary..."
  cd "$REPO_ROOT"
  go test -c -tags=integration -o "$TESTBIN" ./test/adversarial/ 2>&1
  export ADVERSARIAL_TESTBIN="$TESTBIN"

  local OVERALL=0
  local SCENARIOS="${RUN_ONLY:-01 02 03}"

  echo
  echo "═══════════════════════════════════════════════════════════"
  echo "  Phase 8 Adversarial Test Suite"
  printf "  Date:   %s\n" "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  printf "  Host:   %s (%s)\n" "$(uname -n)" "$(uname -r)"
  printf "  Kernel: %s\n" "$FC_KERNEL"
  printf "  Rootfs: %s\n" "$FC_ROOTFS"
  echo "═══════════════════════════════════════════════════════════"

  for s in $SCENARIOS; do
    if ! run_scenario "$s"; then
      OVERALL=1
    fi
  done

  echo
  echo "═══════════════════════════════════════════════════════════"
  if (( OVERALL == 0 )); then
    echo "  RESULT: ALL SCENARIOS PASSED"
  else
    echo "  RESULT: ONE OR MORE SCENARIOS FAILED" >&2
  fi
  echo "═══════════════════════════════════════════════════════════"
  exit $OVERALL
}

main "$@"
