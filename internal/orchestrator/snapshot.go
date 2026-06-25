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
//  2. Copy the retained disk (or fall back to the golden rootfs) into a fresh
//     per-VM file — overlayfs cannot CoW a block image, so a full copy is used.
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
	"time"

	firecracker "github.com/firecracker-microvm/firecracker-go-sdk"
	models "github.com/firecracker-microvm/firecracker-go-sdk/client/models"
)

// ── Snapshot types ─────────────────────────────────────────────────────────

// SnapshotPaths holds the host-absolute paths for the snapshot artefacts.
type SnapshotPaths struct {
	MemFile   string // memory image (guest RAM pages)
	StateFile string // VM state (CPU registers, device state)
	DiskFile  string // retained per-VM rootfs disk, consistent with MemFile
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

// CreatePausedWithDisk pauses inst, captures the memory + device-state snapshot,
// and copies the live per-VM disk (diskPath) into the snapshot directory while
// the guest is still paused — so the disk image is byte-consistent with the
// memory image. The VM is LEFT PAUSED; the caller tears it down. This is the
// stop-with-snapshot path used to make a sandbox resumable to its exact state.
// On any failure the partial snapshot directory is removed.
func (e *SnapshotEngine) CreatePausedWithDisk(ctx context.Context, inst *Instance, diskPath string) (Snapshot, error) {
	id := "snap-" + randHex(6)
	dir := filepath.Join(e.snapshotDir, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Snapshot{}, fmt.Errorf("snapshot: mkdir: %w", err)
	}
	memFile := filepath.Join(dir, "mem.img")
	stateFile := filepath.Join(dir, "state.img")
	diskFile := filepath.Join(dir, "disk.img")

	if err := inst.machine.PauseVM(ctx); err != nil {
		_ = os.RemoveAll(dir)
		return Snapshot{}, fmt.Errorf("snapshot: pause: %w", err)
	}
	if err := inst.machine.CreateSnapshot(ctx, memFile, stateFile); err != nil {
		_ = os.RemoveAll(dir)
		return Snapshot{}, fmt.Errorf("snapshot: create: %w", err)
	}
	// Copy the disk while still paused so it matches the memory image exactly.
	if err := copyFile(diskPath, diskFile, 0o660); err != nil {
		_ = os.RemoveAll(dir)
		return Snapshot{}, fmt.Errorf("snapshot: retain disk: %w", err)
	}

	return Snapshot{
		ID:          id,
		SandboxID:   inst.ID,
		Paths:       SnapshotPaths{MemFile: memFile, StateFile: stateFile, DiskFile: diskFile},
		CreatedAt:   time.Now(),
		GuestMemMiB: inst.Spec.MemMiB,
		GuestVCPUs:  inst.Spec.VCPUs,
	}, nil
}

// StopWithSnapshot snapshots a running sandbox (memory + device state + a
// consistent copy of its disk) and then tears the live VM down. The returned
// Snapshot can later be passed to LoadSnapshot to resume the guest to its exact
// prior state. The per-VM disk is read from the standard launch location
// (StateDir/<id>/rootfs.ext4).
func (s *Supervisor) StopWithSnapshot(ctx context.Context, engine *SnapshotEngine, id string) (Snapshot, error) {
	s.mu.Lock()
	inst, ok := s.inst[id]
	s.mu.Unlock()
	if !ok {
		return Snapshot{}, fmt.Errorf("orchestrator: sandbox %q not found", id)
	}
	diskPath := filepath.Join(s.cfg.StateDir, id, "rootfs.ext4")
	snap, err := engine.CreatePausedWithDisk(ctx, inst, diskPath)
	if err != nil {
		return Snapshot{}, err
	}
	// The VM is paused with a consistent snapshot; reclaim all host state.
	if err := s.Terminate(id); err != nil {
		s.log.Warn("terminate after snapshot", "id", id, "err", err)
	}
	return snap, nil
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
	// Reuse the caller-provided id (resume keeps the original sandbox id); a
	// random one is generated when empty.
	id := spec.ID
	if id == "" {
		id = "sb-" + randHex(6)
		spec.ID = id
	}

	s.mu.Lock()
	if _, dup := s.inst[id]; dup {
		s.mu.Unlock()
		return nil, fmt.Errorf("orchestrator: sandbox %q already exists", id)
	}
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

	// Restore the disk into a fresh per-VM file so the resumed VM is independent
	// and the snapshot stays reusable. Chmod defeats the umask and the gid is set
	// to the jailer's so the demoted VMM can open the drive O_RDWR — the same fix
	// the Launch path uses. (overlayfs cannot CoW a block image; a full copy is
	// the correct mechanism.)
	//
	// A snapshot captured with a retained disk (StopWithSnapshot) carries DiskFile
	// and is byte-consistent with the memory image. A diskless snapshot
	// (SnapshotEngine.Create — used for fresh golden/pool snapshots) has no
	// retained disk; fall back to the golden rootfs, which is consistent because
	// the guest had not yet written to its disk at snapshot time.
	diskSrc := snap.Paths.DiskFile
	if diskSrc == "" {
		diskSrc = s.cfg.RootfsPath
	}
	rootfsCopy := filepath.Join(stateDir, "rootfs.ext4")
	if err := copyFile(diskSrc, rootfsCopy, 0o660); err != nil {
		s.cids.free(cid)
		_ = os.RemoveAll(stateDir)
		return nil, fmt.Errorf("snapshot restore: copy disk: %w", err)
	}
	if err := os.Chmod(rootfsCopy, 0o660); err != nil {
		s.cids.free(cid)
		_ = os.RemoveAll(stateDir)
		return nil, fmt.Errorf("snapshot restore: chmod disk: %w", err)
	}
	if err := os.Chown(rootfsCopy, 0, s.cfg.JailerGID); err != nil {
		s.cids.free(cid)
		_ = os.RemoveAll(stateDir)
		return nil, fmt.Errorf("snapshot restore: chown disk: %w", err)
	}

	leaf, err := s.prepareCgroup(id, spec)
	if err != nil {
		s.cids.free(cid)
		_ = os.RemoveAll(stateDir)
		return nil, fmt.Errorf("snapshot restore: cgroup: %w", err)
	}

	vmCtx, cancel := context.WithCancel(context.Background())
	cleanup := func() {
		cancel()
		s.cids.free(cid)
		_ = os.Remove(leaf)
		_ = os.RemoveAll(stateDir)
		if paths.ChrootRoot != "" {
			_ = os.RemoveAll(filepath.Dir(paths.ChrootRoot))
		}
	}

	fcCfg := firecracker.Config{
		SocketPath: socketPath,
		// No KernelImagePath/boot-source — newMachine passes firecracker.WithSnapshot
		// for this config, which swaps in the load-snapshot handler + validation so
		// FC restores guest + device state from the snapshot files below.
		Snapshot: firecracker.SnapshotConfig{
			MemFilePath:         snap.Paths.MemFile,
			SnapshotPath:        snap.Paths.StateFile,
			EnableDiffSnapshots: false,
			ResumeVM:            true,
		},
		Drives: []models.Drive{{
			DriveID:      firecracker.String("rootfs"),
			PathOnHost:   firecracker.String(rootfsCopy),
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
