#!/usr/bin/env bash
# common.sh — shared helpers for Phase-8 adversarial test scripts.
# Source this file; do not execute directly.
#
# Usage in a scenario script:
#   source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

set -euo pipefail

# ── environment ───────────────────────────────────────────────────────────────

# Binaries and images — override via env.
FC_BIN=${FC_BIN:-firecracker}
FC_KERNEL=${FC_KERNEL:-}    # host path to vmlinux; required for Go tests
FC_ROOTFS=${FC_ROOTFS:-}    # host path to rootfs.ext4; required for Go tests

SANDBOX_STATE_DIR=${SANDBOX_STATE_DIR:-/tmp/sandbox-adversarial-state}
CGROUP_ROOT=${CGROUP_ROOT:-/sys/fs/cgroup}
CGROUP_BASE=${CGROUP_BASE:-sandboxes-test}

# Running total of assertion failures; checked by scenario_result.
FAILURES=0

# ── output helpers ────────────────────────────────────────────────────────────

section()  { echo; echo "── $* ──"; }
info()     { echo "  [INFO]  $*"; }
warn()     { echo "  [WARN]  $*" >&2; }

# ── prerequisite checks ───────────────────────────────────────────────────────

require_env() {
  local var="$1"
  if [[ -z "${!var:-}" ]]; then
    echo "ERROR: environment variable $var must be set" >&2
    exit 1
  fi
}

require_root() {
  if [[ $EUID -ne 0 ]]; then
    warn "not running as root — cgroup writes and jailer may fail"
  fi
}

require_kvm() {
  if [[ ! -e /dev/kvm ]]; then
    echo "ERROR: /dev/kvm not available — adversarial tests require a KVM host" >&2
    exit 1
  fi
}

require_cmd() {
  for cmd in "$@"; do
    if ! command -v "$cmd" &>/dev/null; then
      echo "ERROR: required command not found: $cmd" >&2
      exit 1
    fi
  done
}

# ── cgroup helpers ────────────────────────────────────────────────────────────

# Read one cgroup v2 controller value for a named sandbox cgroup leaf.
# Usage: cgroup_get <leaf-name> <controller>
# Example: cgroup_get sb-abc123 pids.max
cgroup_get() {
  local leaf="$1" ctrl="$2"
  local path="${CGROUP_ROOT}/${CGROUP_BASE}/${leaf}/${ctrl}"
  if [[ -r "$path" ]]; then
    cat "$path"
  else
    echo "MISSING"
  fi
}

# ── host metrics ──────────────────────────────────────────────────────────────

# Returns the 1-minute load average from /proc/loadavg as an integer (truncated).
host_load_int() {
  awk '{printf "%d", $1}' /proc/loadavg
}

host_load_raw() {
  awk '{print $1}' /proc/loadavg
}

# ── assertion helpers ─────────────────────────────────────────────────────────

assert_eq() {
  local desc="$1" got="$2" want="$3"
  if [[ "$got" == "$want" ]]; then
    echo "  PASS: $desc"
  else
    echo "  FAIL: $desc — want=${want} got=${got}" >&2
    FAILURES=$(( FAILURES + 1 ))
  fi
}

# Assert integer $got < integer $max.
assert_lt() {
  local desc="$1" got="$2" max="$3"
  if (( got < max )); then
    echo "  PASS: $desc (${got} < ${max})"
  else
    echo "  FAIL: $desc — ${got} is not < ${max}" >&2
    FAILURES=$(( FAILURES + 1 ))
  fi
}

# Assert two strings are NOT equal.
assert_ne() {
  local desc="$1" a="$2" b="$3"
  if [[ "$a" != "$b" ]]; then
    echo "  PASS: $desc (values differ as expected)"
  else
    echo "  FAIL: $desc — both values are: ${a}" >&2
    FAILURES=$(( FAILURES + 1 ))
  fi
}

assert_not_contains() {
  local desc="$1" haystack="$2" needle="$3"
  if [[ "$haystack" != *"$needle"* ]]; then
    echo "  PASS: $desc"
  else
    echo "  FAIL: $desc — output unexpectedly contains: ${needle}" >&2
    FAILURES=$(( FAILURES + 1 ))
  fi
}

assert_contains() {
  local desc="$1" haystack="$2" needle="$3"
  if [[ "$haystack" == *"$needle"* ]]; then
    echo "  PASS: $desc"
  else
    echo "  FAIL: $desc — expected to find '${needle}' in output" >&2
    FAILURES=$(( FAILURES + 1 ))
  fi
}

# Print a final PASS/FAIL summary for the scenario. Returns 1 on failure.
scenario_result() {
  local name="$1"
  echo
  if (( FAILURES == 0 )); then
    echo "  ✓ ${name}: PASSED"
    return 0
  else
    echo "  ✗ ${name}: FAILED (${FAILURES} assertion(s) failed)" >&2
    return 1
  fi
}
