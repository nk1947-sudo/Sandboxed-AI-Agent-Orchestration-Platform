//go:build linux

package orchestrator

import "testing"

func TestCIDAllocatorUniqueExhaustFree(t *testing.T) {
	a := newCIDAllocator(3, 5) // 3 CIDs available

	seen := map[uint32]bool{}
	for i := 0; i < 3; i++ {
		c, err := a.alloc()
		if err != nil {
			t.Fatalf("alloc %d: %v", i, err)
		}
		if c < 3 || c > 5 {
			t.Fatalf("CID %d out of range [3,5]", c)
		}
		if seen[c] {
			t.Fatalf("CID %d allocated twice", c)
		}
		seen[c] = true
	}

	if _, err := a.alloc(); err == nil {
		t.Fatal("expected pool-exhausted error on 4th alloc")
	}

	// Free one and confirm it can be re-allocated.
	a.free(4)
	c, err := a.alloc()
	if err != nil {
		t.Fatalf("alloc after free: %v", err)
	}
	if c != 4 {
		t.Fatalf("expected freed CID 4 to be reused, got %d", c)
	}
}

func TestApplySpecDefaults(t *testing.T) {
	got := applySpecDefaults(LaunchSpec{})
	want := LaunchSpec{VCPUs: 1, MemMiB: 256, CPUPercent: 100, PidsMax: 128}
	if got != want {
		t.Fatalf("defaults = %+v, want %+v", got, want)
	}

	// Explicit values are preserved.
	in := LaunchSpec{ID: "x", VCPUs: 4, MemMiB: 1024, CPUPercent: 200, PidsMax: 512}
	if out := applySpecDefaults(in); out != in {
		t.Fatalf("explicit spec mutated: %+v", out)
	}
}

func TestCPUMaxString(t *testing.T) {
	cases := map[int]string{
		100: "100000 100000", // one full core
		50:  "50000 100000",  // half a core
		150: "150000 100000", // 1.5 cores
		25:  "25000 100000",
	}
	for pct, want := range cases {
		if got := cpuMaxString(pct); got != want {
			t.Fatalf("cpuMaxString(%d) = %q, want %q", pct, got, want)
		}
	}
}

func TestMemMaxBytes(t *testing.T) {
	// 256 MiB guest RAM + 64 MiB headroom.
	want := int64(256*1024*1024) + memHeadroomBytes
	if got := memMaxBytes(256); got != want {
		t.Fatalf("memMaxBytes(256) = %d, want %d", got, want)
	}
}

func TestAdmissionControl(t *testing.T) {
	s := &Supervisor{
		cfg:  Config{MaxConcurrent: 2, MemoryBudgetMiB: 1024},
		inst: map[string]*Instance{},
	}

	// Under both limits: admitted.
	if err := s.admitLocked(LaunchSpec{MemMiB: 256}); err != nil {
		t.Fatalf("first launch should be admitted: %v", err)
	}

	// Fill to the concurrency cap.
	s.inst["a"] = &Instance{Spec: LaunchSpec{MemMiB: 256}}
	s.inst["b"] = &Instance{Spec: LaunchSpec{MemMiB: 256}}
	if err := s.admitLocked(LaunchSpec{MemMiB: 256}); err == nil {
		t.Fatal("expected admission denial at MaxConcurrent")
	}

	// Memory budget exceeded (independent of concurrency).
	s2 := &Supervisor{cfg: Config{MemoryBudgetMiB: 512}, inst: map[string]*Instance{}}
	s2.inst["a"] = &Instance{Spec: LaunchSpec{MemMiB: 384}}
	if err := s2.admitLocked(LaunchSpec{MemMiB: 256}); err == nil {
		t.Fatal("expected admission denial at memory budget")
	}
	if err := s2.admitLocked(LaunchSpec{MemMiB: 128}); err != nil {
		t.Fatalf("128 MiB should fit in remaining budget: %v", err)
	}
}

func TestProcessAlive(t *testing.T) {
	if processAlive(0) || processAlive(-1) {
		t.Fatal("pid <= 0 must not be alive")
	}
	// A wildly high pid is virtually certain not to exist.
	if processAlive(1 << 30) {
		t.Fatal("nonexistent pid reported alive")
	}
}
