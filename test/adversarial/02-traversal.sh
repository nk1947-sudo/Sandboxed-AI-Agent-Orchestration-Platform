#!/usr/bin/env bash
# 02-traversal.sh — Scenario 2: Directory-traversal escape attempt.
#
# Payload:   cat /workspace/../../etc/shadow
# Threat:    An agent tries to read host credentials by walking up the
#            directory tree from the allowed workspace.
# Boundary:  The guest runs inside its own KVM-isolated kernel and ext4
#            filesystem. There is no shared FS layer to traverse through.
# Expect:    The guest either reads its own /etc/shadow (harmless) or gets
#            permission denied. The HOST's /etc/shadow is never returned.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=common.sh
source "${SCRIPT_DIR}/common.sh"

require_env FC_KERNEL
require_env FC_ROOTFS
require_kvm

section "Scenario 02: Directory-traversal escape"

# ── capture host shadow for comparison ───────────────────────────────────────

HOST_SHADOW_HASH=$(sha256sum /etc/shadow 2>/dev/null | awk '{print $1}' || echo "HOST_SHADOW_NOT_READABLE")
info "host /etc/shadow sha256: ${HOST_SHADOW_HASH:0:16}…"

# ── run the Go integration test ───────────────────────────────────────────────

if [[ -n "${ADVERSARIAL_TESTBIN:-}" && -x "${ADVERSARIAL_TESTBIN}" ]]; then
  TEST_OUTPUT=$(FC_KERNEL="${FC_KERNEL}" FC_ROOTFS="${FC_ROOTFS}" \
    "${ADVERSARIAL_TESTBIN}" -test.v -test.run=TestAdversarialDirTraversal \
    -test.timeout=90s 2>&1 || true)
else
  TEST_OUTPUT=$(FC_KERNEL="${FC_KERNEL}" FC_ROOTFS="${FC_ROOTFS}" \
    go test -v -tags=integration -run=TestAdversarialDirTraversal \
    -timeout=90s ./test/adversarial/ 2>&1 || true)
fi

echo "$TEST_OUTPUT"

# ── compare hashes ────────────────────────────────────────────────────────────

GUEST_SHADOW_HASH=$(echo "$TEST_OUTPUT" | grep '^GUEST_SHADOW_HASH=' | tail -1 | cut -d= -f2- || true)

if [[ -n "$GUEST_SHADOW_HASH" ]]; then
  info "guest output hash: ${GUEST_SHADOW_HASH:0:16}…"
  assert_ne \
    "guest traversal output differs from host /etc/shadow (boundary holds)" \
    "$GUEST_SHADOW_HASH" "$HOST_SHADOW_HASH"
else
  info "GUEST_SHADOW_HASH not emitted — guest likely got permission denied (also acceptable)"
fi

# ── Go test pass check ────────────────────────────────────────────────────────

assert_contains "Go test reported PASS" "$TEST_OUTPUT" "PASS"

scenario_result "02-traversal"
