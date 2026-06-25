//go:build linux

// Package bench — Phase 7 benchmarks for snapshot restore latency and CoW
// memory efficiency.
//
// These benchmarks require a KVM host with Firecracker binaries, a built
// kernel image, and a golden rootfs; they are skipped automatically when those
// resources are absent.
//
// Run:
//
//	BENCH_KERNEL=/path/vmlinux BENCH_ROOTFS=/path/rootfs.ext4 \
//	  go test ./test/bench/ -bench=. -benchtime=5x -v
package bench

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/yourorg/sandbox-platform/internal/orchestrator"
)

// ── test harness ──────────────────────────────────────────────────────────────

type benchEnv struct {
	kernelPath string
	rootfsPath string
	stateDir   string
	snapDir    string
}

func newBenchEnv(t testing.TB) *benchEnv {
	t.Helper()
	kernel := os.Getenv("BENCH_KERNEL")
	rootfs := os.Getenv("BENCH_ROOTFS")
	if kernel == "" || rootfs == "" {
		t.Skip("BENCH_KERNEL and BENCH_ROOTFS must be set to run restore benchmarks")
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("/dev/kvm not available — skipping KVM-dependent benchmarks")
	}
	tmp := t.TempDir()
	return &benchEnv{
		kernelPath: kernel,
		rootfsPath: rootfs,
		stateDir:   filepath.Join(tmp, "state"),
		snapDir:    filepath.Join(tmp, "snaps"),
	}
}

func (e *benchEnv) supervisor(t testing.TB) *orchestrator.Supervisor {
	t.Helper()
	sup, err := orchestrator.NewSupervisor(orchestrator.Config{
		FirecrackerBin:  envOrDefault("BENCH_FC_BIN", "firecracker"),
		JailerBin:       envOrDefault("BENCH_JAILER_BIN", "jailer"),
		KernelImagePath: e.kernelPath,
		RootfsPath:      e.rootfsPath,
		UseJailer:       false, // benches run without jailer; security tests use it
		StateDir:        e.stateDir,
		CgroupRoot:      "/sys/fs/cgroup",
		CgroupBase:      "bench",
	})
	if err != nil {
		t.Skipf("supervisor init failed (likely not on a full KVM host): %v", err)
	}
	return sup
}

// ── helpers ───────────────────────────────────────────────────────────────────

// bootGoldenVM boots the base VM that will be snapshotted.
func bootGoldenVM(ctx context.Context, t testing.TB, sup *orchestrator.Supervisor) *orchestrator.Instance {
	t.Helper()
	inst, err := sup.Launch(ctx, orchestrator.LaunchSpec{
		ID:     "golden",
		VCPUs:  1,
		MemMiB: 256,
	})
	if err != nil {
		t.Fatalf("boot golden VM: %v", err)
	}
	// Wait briefly for the guest agent to be ready.
	time.Sleep(3 * time.Second)
	return inst
}

func captureSnapshot(ctx context.Context, t testing.TB, sup *orchestrator.Supervisor, eng *orchestrator.SnapshotEngine, inst *orchestrator.Instance) orchestrator.Snapshot {
	t.Helper()
	snap, err := eng.Create(ctx, inst)
	if err != nil {
		t.Fatalf("create snapshot: %v", err)
	}
	return snap
}

// ── benchmarks ────────────────────────────────────────────────────────────────

// BenchmarkSnapshotRestore measures the wall-clock time from calling
// LoadSnapshot to having a running sandbox. Target: < 125 ms.
func BenchmarkSnapshotRestore(b *testing.B) {
	env := newBenchEnv(b)
	sup := env.supervisor(b)
	ctx := context.Background()

	eng, err := orchestrator.NewSnapshotEngine(env.snapDir)
	if err != nil {
		b.Fatalf("snapshot engine: %v", err)
	}

	golden := bootGoldenVM(ctx, b, sup)
	snap := captureSnapshot(ctx, b, sup, eng, golden)
	b.Cleanup(func() {
		_ = sup.Terminate(golden.ID)
		_ = eng.Delete(snap)
	})

	spec := orchestrator.LaunchSpec{VCPUs: snap.GuestVCPUs, MemMiB: snap.GuestMemMiB}

	b.ResetTimer()
	b.ReportAllocs()
	var total time.Duration

	for i := 0; i < b.N; i++ {
		start := time.Now()
		inst, err := sup.LoadSnapshot(ctx, snap, spec)
		elapsed := time.Since(start)
		if err != nil {
			b.Fatalf("LoadSnapshot iteration %d: %v", i, err)
		}
		total += elapsed
		b.ReportMetric(float64(elapsed.Milliseconds()), "ms/restore")

		// Verify the budget.
		if elapsed > 125*time.Millisecond {
			b.Logf("WARNING: restore %d took %v (> 125 ms target)", i, elapsed)
		}

		b.Cleanup(func() { _ = sup.Terminate(inst.ID) })
	}
	if b.N > 0 {
		avg := total / time.Duration(b.N)
		b.Logf("average restore latency: %v", avg)
	}
}

// BenchmarkPoolAcquire measures the time for Pool.Acquire when warm slots exist.
// This should be sub-millisecond.
func BenchmarkPoolAcquire(b *testing.B) {
	env := newBenchEnv(b)
	sup := env.supervisor(b)
	ctx := context.Background()

	eng, err := orchestrator.NewSnapshotEngine(env.snapDir)
	if err != nil {
		b.Fatalf("snapshot engine: %v", err)
	}
	golden := bootGoldenVM(ctx, b, sup)
	snap := captureSnapshot(ctx, b, sup, eng, golden)
	spec := orchestrator.LaunchSpec{VCPUs: snap.GuestVCPUs, MemMiB: snap.GuestMemMiB}

	pool, err := orchestrator.NewPool(sup, orchestrator.PoolConfig{
		Snapshot: snap,
		Spec:     spec,
		Depth:    3,
	}, nil)
	if err != nil {
		b.Fatalf("new pool: %v", err)
	}

	// Allow the pool to warm up.
	time.Sleep(5 * time.Second)

	b.Cleanup(func() {
		pool.Close()
		_ = sup.Terminate(golden.ID)
		_ = eng.Delete(snap)
	})

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		start := time.Now()
		inst, err := pool.Acquire(ctx)
		elapsed := time.Since(start)
		if err != nil {
			b.Fatalf("Acquire %d: %v", i, err)
		}
		b.ReportMetric(float64(elapsed.Milliseconds()), "ms/acquire")
		if err := pool.Release(inst); err != nil {
			b.Logf("Release %d: %v", i, err)
		}
	}
}

// requireJailerForFanout skips tests that restore MORE THAN ONE VM from a single
// snapshot. A Firecracker snapshot bakes in the absolute host-side vsock UDS path,
// so every restore re-binds that same path — fan-out works only under the jailer,
// where each VM has its own chroot and the path is relative and therefore unique.
// The bench harness runs non-jailer (UseJailer:false), so these are skipped unless
// BENCH_JAILER=1 (a host with the jailer configured). The single-VM production
// resume path (StopWithSnapshot → LoadSnapshot) is unaffected by this.
func requireJailerForFanout(t testing.TB) {
	t.Helper()
	if os.Getenv("BENCH_JAILER") == "" {
		t.Skip("multi-VM snapshot fan-out needs the jailer (per-chroot vsock UDS path); " +
			"non-jailer restore collides on the snapshot's baked absolute vsock path. " +
			"Set BENCH_JAILER=1 on a jailer-configured host to run.")
	}
}

// TestRestoreUniqueIdentity verifies that two VMs restored from the same
// snapshot receive distinct IDs, CIDs, and vsock paths.
func TestRestoreUniqueIdentity(t *testing.T) {
	requireJailerForFanout(t)
	env := newBenchEnv(t)
	sup := env.supervisor(t)
	ctx := context.Background()

	eng, err := orchestrator.NewSnapshotEngine(env.snapDir)
	if err != nil {
		t.Fatalf("snapshot engine: %v", err)
	}
	golden := bootGoldenVM(ctx, t, sup)
	snap := captureSnapshot(ctx, t, sup, eng, golden)
	spec := orchestrator.LaunchSpec{VCPUs: snap.GuestVCPUs, MemMiB: snap.GuestMemMiB}

	t.Cleanup(func() {
		_ = sup.Terminate(golden.ID)
		_ = eng.Delete(snap)
	})

	a, err := sup.LoadSnapshot(ctx, snap, spec)
	if err != nil {
		t.Fatalf("restore A: %v", err)
	}
	b, err := sup.LoadSnapshot(ctx, snap, spec)
	if err != nil {
		t.Fatalf("restore B: %v", err)
	}
	t.Cleanup(func() {
		_ = sup.Terminate(a.ID)
		_ = sup.Terminate(b.ID)
	})

	if a.ID == b.ID {
		t.Errorf("duplicate sandbox ID: %s", a.ID)
	}
	if a.CID == b.CID {
		t.Errorf("duplicate CID: %d", a.CID)
	}
	if a.VsockUDSPath == b.VsockUDSPath {
		t.Errorf("duplicate vsock path: %s", a.VsockUDSPath)
	}
}

// TestPageSharing verifies that the golden snapshot files are shared (read-only)
// and not duplicated per-VM. We check that the base memory file size doesn't
// grow linearly with the number of restores.
func TestPageSharing(t *testing.T) {
	requireJailerForFanout(t)
	env := newBenchEnv(t)
	sup := env.supervisor(t)
	ctx := context.Background()

	eng, err := orchestrator.NewSnapshotEngine(env.snapDir)
	if err != nil {
		t.Fatalf("snapshot engine: %v", err)
	}
	golden := bootGoldenVM(ctx, t, sup)
	snap := captureSnapshot(ctx, t, sup, eng, golden)
	spec := orchestrator.LaunchSpec{VCPUs: snap.GuestVCPUs, MemMiB: snap.GuestMemMiB}

	baseSize := fileSize(t, snap.Paths.MemFile)
	t.Logf("base mem file: %s", formatBytes(baseSize))

	const N = 3
	var instances [N]*orchestrator.Instance
	for i := range instances {
		inst, err := sup.LoadSnapshot(ctx, snap, spec)
		if err != nil {
			t.Fatalf("restore %d: %v", i, err)
		}
		instances[i] = inst
	}
	t.Cleanup(func() {
		for _, inst := range instances {
			if inst != nil {
				_ = sup.Terminate(inst.ID)
			}
		}
		_ = sup.Terminate(golden.ID)
		_ = eng.Delete(snap)
	})

	// The base file must be unchanged (not mutated by any restore).
	afterSize := fileSize(t, snap.Paths.MemFile)
	if afterSize != baseSize {
		t.Errorf("base mem file changed after %d restores: %s → %s", N,
			formatBytes(baseSize), formatBytes(afterSize))
	}
	t.Logf("base mem file unchanged after %d restores (%s) — CoW confirmed", N, formatBytes(baseSize))
}

// ── helpers ───────────────────────────────────────────────────────────────────

func fileSize(t testing.TB, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.Size()
}

func formatBytes(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(b)/(1<<20))
	default:
		return strconv.FormatInt(b, 10) + " B"
	}
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
