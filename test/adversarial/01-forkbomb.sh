#!/usr/bin/env bash
# 01-forkbomb.sh — Scenario 1: Fork-bomb / runaway-loop containment.
#
# Payload:   :(){ :|:& };:
# Threat:    A runaway guest process spawns unlimited children, trying to
#            exhaust host PID table, CPU, and memory.
# Controls:  cgroup v2 pids.max caps guest process count.
#            cgroup v2 cpu.max caps VMM CPU on host.
# Expect:    Guest thrashes internally; host load stays bounded.
#            pids.current never exceeds pids.max inside the cgroup.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=common.sh
source "${SCRIPT_DIR}/common.sh"

require_env FC_KERNEL
require_env FC_ROOTFS
require_kvm

section "Scenario 01: Fork-bomb containment"

# ── host baseline ─────────────────────────────────────────────────────────────

LOAD_BEFORE=$(host_load_raw)
info "host 1-min load before: ${LOAD_BEFORE}"

# ── run the Go integration test ───────────────────────────────────────────────
# The test boots a VM with pids.max=256, sends the fork-bomb with a 5 s timeout,
# and writes CGROUP_PATH=<path> to stdout so we can read cgroup values here.

TEST_OUTPUT=$(FC_KERNEL="${FC_KERNEL}" FC_ROOTFS="${FC_ROOTFS}" \
  "${ADVERSARIAL_TESTBIN:-go test -v -tags=integration -run TestAdversarialForkBomb -timeout=90s ./test/adversarial/}" \
  2>&1 || true)

# Support both pre-compiled binary and direct go-test invocation.
if [[ -n "${ADVERSARIAL_TESTBIN:-}" && -x "${ADVERSARIAL_TESTBIN}" ]]; then
  TEST_OUTPUT=$(FC_KERNEL="${FC_KERNEL}" FC_ROOTFS="${FC_ROOTFS}" \
    "${ADVERSARIAL_TESTBIN}" -test.v -test.run=TestAdversarialForkBomb \
    -test.timeout=90s 2>&1 || true)
else
  TEST_OUTPUT=$(FC_KERNEL="${FC_KERNEL}" FC_ROOTFS="${FC_ROOTFS}" \
    go test -v -tags=integration -run=TestAdversarialForkBomb \
    -timeout=90s ./test/adversarial/ 2>&1 || true)
fi

echo "$TEST_OUTPUT"

# ── cgroup assertions ─────────────────────────────────────────────────────────

CGROUP_PATH=$(echo "$TEST_OUTPUT" | grep '^CGROUP_PATH=' | tail -1 | cut -d= -f2- || true)
if [[ -n "$CGROUP_PATH" && -d "$CGROUP_PATH" ]]; then
  PIDS_MAX=$(cat "${CGROUP_PATH}/pids.max" 2>/dev/null || echo "MISSING")
  PIDS_CURRENT=$(cat "${CGROUP_PATH}/pids.current" 2>/dev/null || echo "0")
  info "pids.max=${PIDS_MAX}  pids.current=${PIDS_CURRENT}"

  assert_ne "pids.max is configured (not unlimited)" "$PIDS_MAX" "max"
  if [[ "$PIDS_MAX" != "max" && "$PIDS_MAX" != "MISSING" ]]; then
    assert_lt "pids.current ≤ pids.max" \
      "$PIDS_CURRENT" "$(( PIDS_MAX + 1 ))"
  fi
else
  info "cgroup leaf already cleaned up (VM terminated) — skipping live cgroup check"
fi

# ── host load assertion ───────────────────────────────────────────────────────

LOAD_AFTER=$(host_load_raw)
info "host 1-min load after:  ${LOAD_AFTER}"

LOAD_BEFORE_INT=$(host_load_int)
LOAD_AFTER_INT=$(echo "$LOAD_AFTER" | awk '{printf "%d", $1}')
LOAD_MAX=$(( LOAD_BEFORE_INT * 4 + 8 ))

assert_lt "host load did not spike beyond ${LOAD_MAX}" \
  "$LOAD_AFTER_INT" "$LOAD_MAX"

# ── Go test pass check ────────────────────────────────────────────────────────

assert_contains "Go test reported PASS" "$TEST_OUTPUT" "PASS"

scenario_result "01-forkbomb"
