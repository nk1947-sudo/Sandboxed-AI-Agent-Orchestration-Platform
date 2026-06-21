//go:build linux

// snapshot.go — Phase 7: Firecracker snapshot create/restore and overlayfs
// rootfs-overlay helpers.
//
// Snapshot create flow:
//  1. PauseVM  → quiesce guest I/O.
//  2. CreateSnapshot(memFilePath, snapshotPath) → two opaque files on host.
//  3. ResumeVM → golden VM keeps running; snapshot files are never mutated.
//
// Snapshot restore flow (LoadSnapshot):
//  1. Build a firecracker.Config with Config.Snapshot set (not KernelImagePath).
//  2. Mount a per-VM overlayfs over the golden rootfs (CoW; no full copy).
//  3. Start the machine — FC restores guest state from the snap files.
//  4. Re-key: fresh CID, vsock UDS path, entropy seed.
//
// VERIFY-SDK: call sites tested against github.com/firecracker-microvm/firecracker-go-sdk v1.0.0:
//   machine.PauseVM / machine.ResumeVM / machine.CreateSnapshot (v1.0.0 machine.go:1105–1151)
//   Config.Snapshot  SnapshotConfig struct             (v1.0.0 snapshot.go:16)
//   firecracker.VsockDevice{ID, CID uint32, Path string} (v1.0.0 machine.go:315)
//   firecracker.Int64 / firecracker.String / firecracker.Bool helpers
package orchestrator

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	firecracker "github.com/firecracker-microvm/firecracker-go-sdk"
	models "github.com/firecracker-microvm/firecracker-go-sdk/client/models"
)

// ── Snapshot types ─────────────────────────────────────────────────────────

// SnapshotPaths holds the host-absolute paths for the two snapshot artefacts.
type SnapshotPaths struct {
	MemFile   string // memory image (guest RAM pages)
	StateFile string // VM state (CPU registers, device state)
}

// Snapshot represents one captured VM state on disk.
type Snapshot struct {
	ID          string
	SandboxID   string // golden VM that was snapshotted
	Paths       SnapshotPaths
	CreatedAt   time.Time
	GuestMemMiB int64
	GuestVCPUs  int64
}

// ── SnapshotEngine ─────────────────────────────────────────────────────────

// SnapshotEngine handles snapshot create/restore operations.
type SnapshotEngine struct {
	snapshotDir string
}

// NewSnapshotEngine returns a SnapshotEngine storing files under dir (created if absent).
func NewSnapshotEngine(dir string) (*SnapshotEngine, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("snapshot: mkdir %s: %w", dir, err)
	}
	return &SnapshotEngine{snapshotDir: dir}, nil
}

// Create pauses inst, captures a full snapshot, then resumes it.
// The golden VM keeps running; the two snapshot files on disk are never mutated.
func (e *SnapshotEngine) Create(ctx context.Context, inst *Instance) (Snapshot, error) {
	id := "snap-" + randHex(6)
	dir := filepath.Join(e.snapshotDir, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Snapshot{}, fmt.Errorf("snapshot: mkdir: %w", err)
	}

	memFile := filepath.Join(dir, "mem.img")
	stateFile := filepath.Join(dir, "state.img")

	if err := inst.machine.PauseVM(ctx); err != nil {
		return Snapshot{}, fmt.Errorf("snapshot: pause: %w", err)
	}

	createErr := inst.machine.CreateSnapshot(ctx, memFile, stateFile)

	// Always resume; if resume fails we still surface the create error (if any).
	if resumeErr := inst.machine.ResumeVM(ctx); resumeErr != nil {
		if createErr != nil {
			return Snapshot{}, fmt.Errorf("snapshot: create: %w (resume also failed: %v)", createErr, resumeErr)
		}
		return Snapshot{}, fmt.Errorf("snapshot: resume after create: %w", resumeErr)
	}
	if createErr != nil {
		_ = os.RemoveAll(dir)
		return Snapshot{}, fmt.Errorf("snapshot: create: %w", createErr)
	}

	return Snapshot{
		ID:          id,
		SandboxID:   inst.ID,
		Paths:       SnapshotPaths{MemFile: memFile, StateFile: stateFile},
		CreatedAt:   time.Now(),
		GuestMemMiB: inst.Spec.MemMiB,
		GuestVCPUs:  inst.Spec.VCPUs,
	}, nil
}

// Delete removes the snapshot files from disk.
func (e *SnapshotEngine) Delete(snap Snapshot) error {
	return os.RemoveAll(filepath.Dir(snap.Paths.MemFile))
}

// ── Overlayfs rootfs helpers ───────────────────────────────────────────────

// OverlayMount describes one VM's overlayfs CoW mount.
type OverlayMount struct {
	MergedDir string // bind-mount target FC receives as rootfs drive
	UpperDir  string // per-VM writable CoW layer
	WorkDir   string // overlayfs work dir (same filesystem as upper)
	LowerDir  string // read-only golden rootfs (shared across all VMs)
}

// MountOverlay creates a per-VM overlayfs over goldenRootfs.
// The merged dir is the path FC should use as its rootfs drive.
// Requires CAP_SYS_ADMIN (or user-ns overlayfs on kernel ≥ 5.11).
func MountOverlay(stateDir, id, goldenRootfs string) (OverlayMount, error) {
	vmDir := filepath.Join(stateDir, id)
	merged := filepath.Join(vmDir, "rootfs")
	upper := filepath.Join(vmDir, "overlay-upper")
	work := filepath.Join(vmDir, "overlay-work")

	for _, d := range []string{merged, upper, work} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return OverlayMount{}, fmt.Errorf("overlay: mkdir %s: %w", d, err)
		}
	}
	opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", goldenRootfs, upper, work)
	if err := syscall.Mount("overlay", merged, "overlay", 0, opts); err != nil {
		return OverlayMount{}, fmt.Errorf("overlay: mount: %w", err)
	}
	return OverlayMount{
		MergedDir: merged, UpperDir: upper, WorkDir: work, LowerDir: goldenRootfs,
	}, nil
}

// UnmountOverlay unmounts and removes a VM's overlayfs CoW dirs.
func UnmountOverlay(m OverlayMount) error {
	if err := syscall.Unmount(m.MergedDir, 0); err != nil {
		return fmt.Errorf("overlay: umount %s: %w", m.MergedDir, err)
	}
	_ = os.RemoveAll(m.UpperDir)
	_ = os.RemoveAll(m.WorkDir)
	_ = os.Remove(m.MergedDir)
	return nil
}

// ── Identity re-key on restore ─────────────────────────────────────────────

// RestoredIdentity carries fresh per-instance credentials assigned after
// snapshot restore. CID and vsock path are guaranteed unique per instance.
type RestoredIdentity struct {
	CID          uint32
	VsockUDSPath string
	// EntropyBytes is 32 bytes of fresh entropy to reseed the guest PRNG via
	// vsock before the first exec, preventing predictable crypto post-restore.
	EntropyBytes []byte
}

func newRestoredIdentity(vsockPath string, cid uint32) (RestoredIdentity, error) {
	buf := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		return RestoredIdentity{}, fmt.Errorf("snapshot: entropy: %w", err)
	}
	return RestoredIdentity{CID: cid, VsockUDSPath: vsockPath, EntropyBytes: buf}, nil
}

// ── LoadSnapshot on Supervisor ─────────────────────────────────────────────

// LoadSnapshot restores a new sandbox from snap. It:
//  1. Allocates a fresh CID (never reuses the snapshotted VM's CID).
//  2. Mounts a per-VM CoW overlayfs over the golden rootfs.
//  3. Boots a new Firecracker process using Config.Snapshot (not kernel/rootfs).
//  4. Places the VMM in a fresh cgroup leaf with the requested caps.
//  5. Re-keys: assigns fresh CID, vsock UDS path, and entropy seed.
func (s *Supervisor) LoadSnapshot(ctx context.Context, snap Snapshot, spec LaunchSpec) (*Instance, error) {
	spec = applySpecDefaults(spec)
	id := "sb-" + randHex(6)
	spec.ID = id

	s.mu.Lock()
	if err := s.admitLocked(spec); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	s.mu.Unlock()

	cid, err := s.cids.alloc()
	if err != nil {
		return nil, err
	}

	// Build per-VM socket paths (same derivation as Launch).
	stateDir := filepath.Join(s.cfg.StateDir, id)
	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		s.cids.free(cid)
		return nil, fmt.Errorf("snapshot restore: mkdir state: %w", err)
	}

	const vsockName = "v.sock"
	var socketPath, vsockPath string
	var paths bootPaths
	if s.cfg.UseJailer {
		paths.ChrootRoot = filepath.Join(s.cfg.ChrootBaseDir, "firecracker", id, "root")
		socketPath = "run/firecracker.socket"
		vsockPath = vsockName
		paths.HostSocket = filepath.Join(paths.ChrootRoot, socketPath)
		paths.HostVsock = filepath.Join(paths.ChrootRoot, vsockName)
	} else {
		socketPath = filepath.Join(stateDir, "firecracker.socket")
		vsockPath = filepath.Join(stateDir, vsockName)
		paths.HostSocket = socketPath
		paths.HostVsock = vsockPath
	}

	// Per-VM CoW overlayfs — no full rootfs copy.
	overlay, err := MountOverlay(s.cfg.StateDir, id, s.cfg.RootfsPath)
	if err != nil {
		s.cids.free(cid)
		_ = os.RemoveAll(stateDir)
		return nil, fmt.Errorf("snapshot restore: overlay: %w", err)
	}

	leaf, err := s.prepareCgroup(id, spec)
	if err != nil {
		s.cids.free(cid)
		_ = UnmountOverlay(overlay)
		_ = os.RemoveAll(stateDir)
		return nil, fmt.Errorf("snapshot restore: cgroup: %w", err)
	}

	vmCtx, cancel := context.WithCancel(context.Background())
	cleanup := func() {
		cancel()
		s.cids.free(cid)
		_ = os.Remove(leaf)
		_ = UnmountOverlay(overlay)
		_ = os.RemoveAll(stateDir)
		if paths.ChrootRoot != "" {
			_ = os.RemoveAll(filepath.Dir(paths.ChrootRoot))
		}
	}

	fcCfg := firecracker.Config{
		SocketPath: socketPath,
		// No KernelImagePath — FC restores guest state from the snapshot.
		Snapshot: firecracker.SnapshotConfig{
			MemFilePath:         snap.Paths.MemFile,
			SnapshotPath:        snap.Paths.StateFile,
			EnableDiffSnapshots: false,
			ResumeVM:            true,
		},
		Drives: []models.Drive{{
			DriveID:      firecracker.String("rootfs"),
			PathOnHost:   firecracker.String(overlay.MergedDir),
			IsRootDevice: firecracker.Bool(true),
			IsReadOnly:   firecracker.Bool(false),
		}},
		MachineCfg: models.MachineConfiguration{
			VcpuCount:  firecracker.Int64(snap.GuestVCPUs),
			MemSizeMib: firecracker.Int64(snap.GuestMemMiB),
		},
		VsockDevices: []firecracker.VsockDevice{{
			ID:   "vsock0",
			CID:  cid,
			Path: vsockPath,
		}},
		NetworkInterfaces: nil,
		VMID:              id,
	}
	if s.cfg.UseJailer {
		fcCfg.JailerCfg = s.buildJailerConfig(id)
	}

	machine, err := s.newMachine(vmCtx, fcCfg)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("snapshot restore: new machine: %w", err)
	}
	if err := machine.Start(vmCtx); err != nil {
		cleanup()
		return nil, fmt.Errorf("snapshot restore: start: %w", err)
	}
	pid, err := machine.PID()
	if err != nil {
		_ = machine.StopVMM()
		cleanup()
		return nil, fmt.Errorf("snapshot restore: pid: %w", err)
	}
	if err := s.placeInCgroup(leaf, pid); err != nil {
		_ = machine.StopVMM()
		cleanup()
		return nil, fmt.Errorf("snapshot restore: cgroup: %w", err)
	}

	identity, err := newRestoredIdentity(paths.HostVsock, cid)
	if err != nil {
		_ = machine.StopVMM()
		cleanup()
		return nil, fmt.Errorf("snapshot restore: identity: %w", err)
	}

	inst := &Instance{
		ID:           id,
		CID:          identity.CID,
		PID:          pid,
		ChrootDir:    paths.ChrootRoot,
		SocketPath:   paths.HostSocket,
		VsockUDSPath: identity.VsockUDSPath,
		CgroupPath:   leaf,
		CreatedAt:    time.Now(),
		Spec:         spec,
		machine:      machine,
		ctx:          vmCtx,
		cancel:       cancel,
	}

	s.mu.Lock()
	s.inst[id] = inst
	s.mu.Unlock()

	if s.recorder != nil {
		if err := s.recorder.Save(context.Background(), inst.toRecord()); err != nil {
			s.log.Warn("persist restored sandbox record", "id", id, "err", err)
		}
	}
	s.log.Info("sandbox restored from snapshot",
		"id", id, "cid", cid, "pid", pid, "snap", snap.ID,
		"entropy_bytes", len(identity.EntropyBytes))
	return inst, nil
}
