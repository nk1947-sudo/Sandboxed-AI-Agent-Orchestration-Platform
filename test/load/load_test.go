//go:build integration

// Package load contains Phase-8 load tests for concurrent VM launches and
// restore operations. These tests require a KVM host (skipped otherwise) and
// are intended to be run on the CI KVM runner or a dedicated load machine.
//
// Required environment:
//
//	LOAD_KERNEL  — path to vmlinux
//	LOAD_ROOTFS  — path to rootfs.ext4
//
// Optional:
//
//	LOAD_FC_BIN  — firecracker binary (default: "firecracker")
//	LOAD_N       — number of VMs to launch concurrently (default: 5)
package load

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/yourorg/sandbox-platform/internal/orchestrator"
)

// ── harness ───────────────────────────────────────────────────────────────────

func requireLoadEnv(t testing.TB) (kernel, rootfs string) {
	t.Helper()
	kernel = os.Getenv("LOAD_KERNEL")
	rootfs = os.Getenv("LOAD_ROOTFS")
	if kernel == "" || rootfs == "" {
		t.Skip("LOAD_KERNEL and LOAD_ROOTFS must be set to run load tests")
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("/dev/kvm not available")
	}
	return
}

func newLoadSupervisor(t testing.TB, maxConcurrent int) *orchestrator.Supervisor {
	t.Helper()
	kernel, rootfs := requireLoadEnv(t)
	tmp := t.TempDir()

	sup, err := orchestrator.NewSupervisor(orchestrator.Config{
		FirecrackerBin:  envOrLoad("LOAD_FC_BIN", "firecracker"),
		KernelImagePath: kernel,
		RootfsPath:      rootfs,
		UseJailer:       false,
		StateDir:        tmp,
		CgroupRoot:      "/sys/fs/cgroup",
		CgroupBase:      "sandboxes-load",
		MaxConcurrent:   maxConcurrent,
	})
	if err != nil {
		t.Skipf("supervisor init failed: %v", err)
	}
	t.Cleanup(func() {
		for _, inst := range sup.List() {
			_ = sup.Terminate(inst.ID)
		}
	})
	return sup
}

func envOrLoad(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func loadN(t testing.TB) int {
	if s := os.Getenv("LOAD_N"); s != "" {
		n, err := strconv.Atoi(s)
		if err == nil && n > 0 {
			return n
		}
	}
	return 5
}

// ── TestConcurrentBoot ────────────────────────────────────────────────────────

// TestConcurrentBoot launches N VMs concurrently and records per-VM boot
// latency. It verifies that all VMs come up healthy and reports p50/p95/p99.
func TestConcurrentBoot(t *testing.T) {
	n := loadN(t)
	sup := newLoadSupervisor(t, n+1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(n)*60*time.Second)
	defer cancel()

	type result struct {
		id      string
		elapsed time.Duration
		err     error
	}
	results := make([]result, n)
	var wg sync.WaitGroup

	t.Logf("launching %d VMs concurrently…", n)
	overallStart := time.Now()

	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			inst, err := sup.Launch(ctx, orchestrator.LaunchSpec{VCPUs: 1, MemMiB: 256})
			results[i] = result{
				id:      func() string {
					if inst != nil {
						return inst.ID
					}
					return ""
				}(),
				elapsed: time.Since(start),
				err:     err,
			}
		}()
	}
	wg.Wait()
	totalElapsed := time.Since(overallStart)

	// Collect latencies and check for errors.
	var latencies []time.Duration
	errCount := 0
	for i, r := range results {
		if r.err != nil {
			t.Errorf("VM %d launch error: %v", i, r.err)
			errCount++
			continue
		}
		latencies = append(latencies, r.elapsed)
		t.Logf("VM %d (%s): boot=%v", i, r.id, r.elapsed)
	}

	if errCount > 0 {
		t.Errorf("%d/%d VMs failed to launch", errCount, n)
	}

	if len(latencies) == 0 {
		t.Fatal("no successful launches to report")
	}

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	p := func(pct float64) time.Duration {
		idx := int(float64(len(latencies)-1) * pct)
		return latencies[idx]
	}

	t.Logf("boot latency (n=%d): p50=%v p95=%v p99=%v max=%v wall=%v",
		len(latencies), p(0.50), p(0.95), p(0.99), latencies[len(latencies)-1], totalElapsed)

	// Soft assertion: p99 should be under 30 s on a decent host.
	const p99Target = 30 * time.Second
	if p(0.99) > p99Target {
		t.Logf("WARNING: p99 boot latency %v exceeds target %v", p(0.99), p99Target)
	}
}

// ── BenchmarkConcurrentBoot ───────────────────────────────────────────────────

// BenchmarkConcurrentBoot reports the mean boot time for a single VM launched
// serially. Use -benchtime=Nx to boot N VMs. Target: < 3 s.
func BenchmarkConcurrentBoot(b *testing.B) {
	n := loadN(b)
	sup := newLoadSupervisor(b, n+b.N+1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(b.N+1)*30*time.Second)
	defer cancel()

	b.ResetTimer()
	b.ReportAllocs()
	var total time.Duration

	for i := 0; i < b.N; i++ {
		start := time.Now()
		inst, err := sup.Launch(ctx, orchestrator.LaunchSpec{VCPUs: 1, MemMiB: 256})
		elapsed := time.Since(start)
		if err != nil {
			b.Fatalf("Launch iteration %d: %v", i, err)
		}
		total += elapsed
		b.ReportMetric(float64(elapsed.Milliseconds()), "ms/boot")
		b.Cleanup(func() { _ = sup.Terminate(inst.ID) })
	}
	if b.N > 0 {
		b.Logf("mean boot latency: %v", total/time.Duration(b.N))
	}
}

// ── TestCIDPoolExhaustion ─────────────────────────────────────────────────────

// TestCIDPoolExhaustion verifies that launching more VMs than available CIDs
// fails gracefully rather than wrapping around or panicking.
func TestCIDPoolExhaustion(t *testing.T) {
	kernel, rootfs := requireLoadEnv(t)
	tmp := t.TempDir()

	// Use a tiny CID range: 3..5 → 3 possible CIDs.
	sup, err := orchestrator.NewSupervisor(orchestrator.Config{
		FirecrackerBin:  envOrLoad("LOAD_FC_BIN", "firecracker"),
		KernelImagePath: kernel,
		RootfsPath:      rootfs,
		UseJailer:       false,
		StateDir:        tmp,
		CgroupRoot:      "/sys/fs/cgroup",
		CgroupBase:      "sandboxes-load-cid",
		CIDMin:          3,
		CIDMax:          5,
	})
	if err != nil {
		t.Skipf("supervisor init failed: %v", err)
	}
	defer func() {
		for _, i := range sup.List() {
			_ = sup.Terminate(i.ID)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*60*time.Second)
	defer cancel()

	// Boot 3 VMs — should succeed.
	var succeeded []string
	for i := 0; i < 3; i++ {
		inst, err := sup.Launch(ctx, orchestrator.LaunchSpec{VCPUs: 1, MemMiB: 256})
		if err != nil {
			t.Fatalf("Launch %d of 3: %v", i+1, err)
		}
		succeeded = append(succeeded, inst.ID)
		t.Logf("VM %d: id=%s", i+1, inst.ID)
	}
	t.Logf("launched %d VMs successfully", len(succeeded))

	// 4th launch must fail (CID pool exhausted or MaxConcurrent hit).
	_, err = sup.Launch(ctx, orchestrator.LaunchSpec{VCPUs: 1, MemMiB: 256})
	if err == nil {
		t.Fatal("4th launch should have failed (CID pool exhausted), but succeeded")
	}
	t.Logf("4th launch correctly rejected: %v", err)
}

// ── TestHostSaturationPoint ───────────────────────────────────────────────────

// TestHostSaturationPoint ramps concurrent VMs until launch errors exceed 10%,
// recording the saturation point. This is an observational test (no hard pass/
// fail) — the output is a saturation report for capacity planning.
func TestHostSaturationPoint(t *testing.T) {
	const maxRamp = 20
	sup := newLoadSupervisor(t, maxRamp+1)
	ctx, cancel := context.WithTimeout(context.Background(), 15*60*time.Second)
	defer cancel()

	var launched []*orchestrator.Instance
	defer func() {
		for _, inst := range launched {
			_ = sup.Terminate(inst.ID)
		}
	}()

	for n := 1; n <= maxRamp; n++ {
		inst, err := sup.Launch(ctx, orchestrator.LaunchSpec{VCPUs: 1, MemMiB: 256})
		if err != nil {
			t.Logf("host saturation at n=%d VMs: %v", n, err)
			t.Logf("saturation point: %d concurrent VMs", n-1)
			return
		}
		launched = append(launched, inst)
		t.Logf("n=%d: launched %s (total running: %d)", n, inst.ID, len(launched))
	}
	fmt.Printf("SATURATION_NOT_REACHED: %d VMs ran successfully\n", len(launched))
	t.Logf("host did not saturate up to %d VMs — consider raising maxRamp", maxRamp)
}
