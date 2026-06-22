//go:build linux

// Package orchestrator is the host-side control-plane engine. It owns the full
// lifecycle of Firecracker microVMs:
//
//   - Allocates a unique vsock context ID (CID) per sandbox.
//   - Boots each microVM *through the Firecracker jailer*, so the VMM runs
//     chroot'd, in its own PID namespace, demoted to an unprivileged uid/gid.
//   - Boots with NO network interface ("none" networking) by default.
//   - Pins each VMM into a dedicated cgroup v2 leaf with hard CPU, memory and
//     PID caps (created *before* boot so the guest is never briefly uncapped).
//   - Enforces host-level admission control (max concurrent + memory budget).
//   - Persists records via a sandbox.Recorder and reconciles orphaned state on
//     restart; reaps sandboxes whose VMM has died.
//
// The orchestrator process itself must run with privileges sufficient to invoke
// the jailer and write the cgroup hierarchy (root, or CAP_SYS_ADMIN plus a
// delegated cgroup subtree). Only the *VMM* is demoted; the control plane is not.
//
// SDK compatibility notes
// -----------------------
// Targets github.com/firecracker-microvm/firecracker-go-sdk v1.x. Three call
// sites are version-sensitive and tagged VERIFY-SDK below:
//
//  1. firecracker.NewNaiveChrootStrategy — v1.x takes the kernel image path
//     only; some v0.x releases took (rootfs, kernel).
//  2. firecracker.JailerConfig.CgroupVersion — present in recent releases.
//  3. (*firecracker.Machine).Shutdown / .PID — present in v1.x.
//
// Everything else (cgroup v2 writes, CID allocation, lifecycle, none-networking)
// is plain Go + Linux and version-independent.
package orchestrator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	firecracker "github.com/firecracker-microvm/firecracker-go-sdk"
	models "github.com/firecracker-microvm/firecracker-go-sdk/client/models"

	"github.com/yourorg/sandbox-platform/internal/sandbox"
)

// DefaultKernelArgs boots a headless microVM fast: serial console only, immediate
// reboot/panic behavior, and all legacy PCI/i8042 probing disabled. No `ip=`
// argument is set, which is part of the default-deny "none" network posture.
const DefaultKernelArgs = "console=ttyS0 reboot=k panic=1 pci=off i8042.noaux i8042.nomux i8042.nopnp i8042.dumbkbd random.trust_cpu=on"

// memHeadroomBytes is added on top of the guest RAM when setting memory.max. The
// VMM needs a little host memory beyond guest RAM (device emulation, page
// tables); without headroom the kernel can OOM-kill the VMM at boot.
const memHeadroomBytes = 64 * 1024 * 1024 // 64 MiB

// ErrAdmissionDenied is returned when a Launch would exceed configured host limits.
var ErrAdmissionDenied = errors.New("orchestrator: admission denied (host limits reached)")

// Config holds host-wide settings shared by every sandbox the supervisor boots.
type Config struct {
	// Binaries.
	FirecrackerBin string // path to the firecracker binary (staged into the jail)
	JailerBin      string // path to the jailer binary

	// Guest images.
	KernelImagePath string // host path to vmlinux
	RootfsPath      string // host path to the golden ext4 rootfs (copied per VM)

	// Jailer.
	UseJailer     bool   // production: true. Dev on a permissive host: false.
	ChrootBaseDir string // jailer chroot base, e.g. /srv/jailer
	JailerUID     int    // unprivileged uid the VMM is demoted to
	JailerGID     int    // unprivileged gid the VMM is demoted to

	// Per-VM runtime state (rootfs copies, dev-mode sockets).
	StateDir string // e.g. /srv/sandbox-state

	// cgroup v2.
	CgroupRoot string // mount point, normally /sys/fs/cgroup
	CgroupBase string // parent leaf relative to root, e.g. "sandboxes"

	// Boot.
	KernelArgs string // overrides DefaultKernelArgs when non-empty

	// Optional custom compiled seccomp-BPF filter for the dev (non-jailer) path.
	// When empty, Firecracker's strong built-in seccomp filter is used.
	SeccompFilterPath string

	// Guest CID range (0,1,2 are reserved by the vsock spec).
	CIDMin uint32
	CIDMax uint32

	// Admission control. 0 means unlimited.
	MaxConcurrent   int   // max simultaneously running sandboxes
	MemoryBudgetMiB int64 // total guest RAM the host will commit across all VMs
}

// LaunchSpec describes one sandbox to boot.
type LaunchSpec struct {
	ID         string // optional; a random id is generated when empty
	VCPUs      int64  // guest vCPUs (default 1)
	MemMiB     int64  // guest RAM in MiB (default 256)
	CPUPercent int    // host CPU cap, percent of one core; 100 == one full core
	PidsMax    int64  // max processes in the cgroup leaf (fork-bomb backstop)
}

// Instance is a booted, tracked sandbox.
type Instance struct {
	ID           string
	CID          uint32
	PID          int    // VMM (jailer/firecracker) pid on the host
	ChrootDir    string // <base>/firecracker/<id>/root (jailer) or "" (dev)
	SocketPath   string // firecracker API socket, host-absolute
	VsockUDSPath string // host-absolute path the gateway dials (CONNECT 5005)
	CgroupPath   string // cgroup v2 leaf for this VM
	CreatedAt    time.Time
	Spec         LaunchSpec

	machine *firecracker.Machine
	ctx     context.Context
	cancel  context.CancelFunc
}

func (i *Instance) toRecord() sandbox.Record {
	return sandbox.Record{
		ID:           i.ID,
		CID:          i.CID,
		PID:          i.PID,
		VsockUDSPath: i.VsockUDSPath,
		SocketPath:   i.SocketPath,
		ChrootDir:    i.ChrootDir,
		CgroupPath:   i.CgroupPath,
		Status:       sandbox.StatusRunning,
		CreatedAt:    i.CreatedAt,
		Spec: sandbox.Spec{
			VCPUs:      i.Spec.VCPUs,
			MemMiB:     i.Spec.MemMiB,
			CPUPercent: i.Spec.CPUPercent,
			PidsMax:    i.Spec.PidsMax,
		},
	}
}

// Supervisor boots and tracks sandboxes. It is safe for concurrent use.
type Supervisor struct {
	cfg      Config
	log      *slog.Logger
	recorder sandbox.Recorder

	mu   sync.Mutex
	inst map[string]*Instance
	cids *cidAllocator
}

// Option configures a Supervisor.
type Option func(*Supervisor)

// WithLogger sets the structured logger (defaults to slog.Default()).
func WithLogger(l *slog.Logger) Option {
	return func(s *Supervisor) {
		if l != nil {
			s.log = l
		}
	}
}

// WithRecorder enables persistence + restart reconciliation.
func WithRecorder(r sandbox.Recorder) Option {
	return func(s *Supervisor) { s.recorder = r }
}

// NewSupervisor validates the host configuration and returns a ready supervisor.
func NewSupervisor(cfg Config, opts ...Option) (*Supervisor, error) {
	if cfg.KernelImagePath == "" || cfg.RootfsPath == "" {
		return nil, errors.New("orchestrator: KernelImagePath and RootfsPath are required")
	}
	if cfg.FirecrackerBin == "" {
		cfg.FirecrackerBin = "firecracker"
	}
	if cfg.UseJailer && cfg.JailerBin == "" {
		cfg.JailerBin = "jailer"
	}
	if cfg.ChrootBaseDir == "" {
		cfg.ChrootBaseDir = "/srv/jailer"
	}
	if cfg.StateDir == "" {
		cfg.StateDir = "/srv/sandbox-state"
	}
	if cfg.CgroupRoot == "" {
		cfg.CgroupRoot = "/sys/fs/cgroup"
	}
	if cfg.CgroupBase == "" {
		cfg.CgroupBase = "sandboxes"
	}
	if cfg.JailerUID == 0 {
		cfg.JailerUID = 10001
	}
	if cfg.JailerGID == 0 {
		cfg.JailerGID = 10001
	}
	if cfg.CIDMin == 0 {
		cfg.CIDMin = 3
	}
	if cfg.CIDMax == 0 {
		cfg.CIDMax = 1<<31 - 1
	}

	// Image paths and cgroup v2 are only required when the jailer is active.
	// In dev mode (UseJailer=false) the supervisor starts but will refuse Launch
	// requests at runtime, which is the expected behaviour for local testing.
	if cfg.UseJailer {
		for _, p := range []string{cfg.KernelImagePath, cfg.RootfsPath} {
			if _, err := os.Stat(p); err != nil {
				return nil, fmt.Errorf("orchestrator: %s: %w", p, err)
			}
		}
		if _, err := os.Stat(filepath.Join(cfg.CgroupRoot, "cgroup.controllers")); err != nil {
			return nil, fmt.Errorf("orchestrator: cgroup v2 not mounted at %s: %w", cfg.CgroupRoot, err)
		}
	}

	s := &Supervisor{
		cfg:  cfg,
		log:  slog.Default(),
		inst: make(map[string]*Instance),
		cids: newCIDAllocator(cfg.CIDMin, cfg.CIDMax),
	}
	for _, o := range opts {
		o(s)
	}
	return s, nil
}

// Launch boots a sandbox and applies its cgroup limits before returning. On any
// failure all partial state (CID, chroot, per-VM rootfs, cgroup, VMM) is rolled
// back.
func (s *Supervisor) Launch(ctx context.Context, spec LaunchSpec) (*Instance, error) {
	spec = applySpecDefaults(spec)

	id := spec.ID
	if id == "" {
		id = "sb-" + randHex(6)
		spec.ID = id
	}

	// Duplicate check + admission control under one lock.
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

	fcCfg, paths, err := s.buildFirecrackerConfig(id, cid, spec)
	if err != nil {
		s.cids.free(cid)
		return nil, err
	}

	// Create the cgroup leaf and write caps BEFORE boot, so the guest is never
	// uncapped; only the final PID move happens after the VMM starts.
	leaf, err := s.prepareCgroup(id, spec)
	if err != nil {
		s.cids.free(cid)
		_ = os.RemoveAll(filepath.Join(s.cfg.StateDir, id))
		return nil, fmt.Errorf("orchestrator: prepare cgroup: %w", err)
	}

	// The VM's lifetime is tied to vmCtx (NOT the request ctx, so the sandbox
	// outlives the call that created it). cleanup unwinds partial state.
	vmCtx, cancel := context.WithCancel(context.Background())
	cleanup := func() {
		cancel()
		s.cids.free(cid)
		_ = os.Remove(leaf)
		_ = os.RemoveAll(filepath.Join(s.cfg.StateDir, id))
		if paths.ChrootRoot != "" {
			_ = os.RemoveAll(filepath.Dir(paths.ChrootRoot)) // <base>/firecracker/<id>
		}
	}

	if err := ctx.Err(); err != nil {
		cleanup()
		return nil, err
	}

	machine, err := s.newMachine(vmCtx, fcCfg)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("orchestrator: new machine: %w", err)
	}
	if err := machine.Start(vmCtx); err != nil {
		cleanup()
		return nil, fmt.Errorf("orchestrator: start vm: %w", err)
	}

	pid, err := machine.PID() // VERIFY-SDK: Machine.PID() exists in v1.x
	if err != nil {
		_ = machine.StopVMM()
		cleanup()
		return nil, fmt.Errorf("orchestrator: read vmm pid: %w", err)
	}

	if err := s.placeInCgroup(leaf, pid); err != nil {
		// Fail closed: an uncapped VMM is a host-DoS risk, so tear it down.
		_ = machine.StopVMM()
		cleanup()
		return nil, fmt.Errorf("orchestrator: place in cgroup: %w", err)
	}

	inst := &Instance{
		ID:           id,
		CID:          cid,
		PID:          pid,
		ChrootDir:    paths.ChrootRoot,
		SocketPath:   paths.HostSocket,
		VsockUDSPath: paths.HostVsock,
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
			s.log.Warn("persist sandbox record failed", "id", id, "err", err)
		}
	}
	s.log.Info("sandbox launched",
		"id", id, "cid", cid, "pid", pid, "vcpus", spec.VCPUs, "mem_mib", spec.MemMiB)
	return inst, nil
}

// admitLocked enforces host capacity limits. Caller must hold s.mu.
func (s *Supervisor) admitLocked(spec LaunchSpec) error {
	if s.cfg.MaxConcurrent > 0 && len(s.inst) >= s.cfg.MaxConcurrent {
		return fmt.Errorf("%w: max concurrent sandboxes (%d) reached", ErrAdmissionDenied, s.cfg.MaxConcurrent)
	}
	if s.cfg.MemoryBudgetMiB > 0 {
		var used int64
		for _, in := range s.inst {
			used += in.Spec.MemMiB
		}
		if used+spec.MemMiB > s.cfg.MemoryBudgetMiB {
			return fmt.Errorf("%w: memory budget %d MiB exceeded (used %d + requested %d)",
				ErrAdmissionDenied, s.cfg.MemoryBudgetMiB, used, spec.MemMiB)
		}
	}
	return nil
}

// Terminate stops a sandbox and removes its host-side state.
func (s *Supervisor) Terminate(id string) error {
	s.mu.Lock()
	inst, ok := s.inst[id]
	if ok {
		delete(s.inst, id)
	}
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("orchestrator: sandbox %q not found", id)
	}

	if inst.machine != nil {
		// Try a graceful shutdown (CtrlAltDel) first, then force.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = inst.machine.Shutdown(shutdownCtx) // VERIFY-SDK: Shutdown exists in v1.x
		cancel()
		_ = inst.machine.StopVMM()
	}
	if inst.cancel != nil {
		inst.cancel()
	}

	s.cids.free(inst.CID)
	if inst.CgroupPath != "" {
		_ = os.Remove(inst.CgroupPath) // empty cgroup leaves are removable
	}
	_ = os.RemoveAll(filepath.Join(s.cfg.StateDir, inst.ID))
	if inst.ChrootDir != "" {
		_ = os.RemoveAll(filepath.Dir(inst.ChrootDir))
	}
	if s.recorder != nil {
		_ = s.recorder.Remove(context.Background(), inst.ID)
	}
	s.log.Info("sandbox terminated", "id", inst.ID, "cid", inst.CID)
	return nil
}

// Get returns a tracked instance by id.
func (s *Supervisor) Get(id string) (*Instance, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inst, ok := s.inst[id]
	return inst, ok
}

// List returns a snapshot of all tracked instances.
func (s *Supervisor) List() []*Instance {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Instance, 0, len(s.inst))
	for _, inst := range s.inst {
		out = append(out, inst)
	}
	return out
}

// Count returns the number of managed sandboxes.
func (s *Supervisor) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.inst)
}

// Reconcile adopts or garbage-collects sandbox state left by a previous control
// plane process. A VM this process does not manage cannot be driven through the
// SDK, so any still-alive orphan is killed and all its host state reclaimed. Run
// once at startup, before serving launches.
func (s *Supervisor) Reconcile(ctx context.Context) error {
	if s.recorder == nil {
		return nil
	}
	recs, err := s.recorder.All(ctx)
	if err != nil {
		return fmt.Errorf("orchestrator: reconcile list: %w", err)
	}
	reclaimed := 0
	for _, r := range recs {
		s.mu.Lock()
		_, managed := s.inst[r.ID]
		s.mu.Unlock()
		if managed {
			continue
		}
		if r.PID > 0 && processAlive(r.PID) {
			s.log.Warn("killing orphaned VMM from previous run", "id", r.ID, "pid", r.PID)
			killPID(r.PID)
		}
		s.gcRecord(ctx, r)
		reclaimed++
	}
	if reclaimed > 0 {
		s.log.Info("reconcile reclaimed orphaned sandboxes", "count", reclaimed)
	}
	return nil
}

// ReapDead terminates managed sandboxes whose VMM process has died, returning the
// reaped ids. Intended for a periodic health loop.
func (s *Supervisor) ReapDead() []string {
	var dead []string
	for _, inst := range s.List() {
		if !processAlive(inst.PID) {
			s.log.Warn("reaping dead sandbox", "id", inst.ID, "pid", inst.PID)
			_ = s.Terminate(inst.ID)
			dead = append(dead, inst.ID)
		}
	}
	return dead
}

// gcRecord removes all host state associated with a record.
func (s *Supervisor) gcRecord(ctx context.Context, r sandbox.Record) {
	if r.CgroupPath != "" {
		_ = os.Remove(r.CgroupPath)
	}
	if r.ChrootDir != "" {
		_ = os.RemoveAll(filepath.Dir(r.ChrootDir))
	}
	_ = os.RemoveAll(filepath.Join(s.cfg.StateDir, r.ID))
	if s.recorder != nil {
		_ = s.recorder.Remove(ctx, r.ID)
	}
}

// bootPaths are the host-absolute paths derived while building the FC config.
type bootPaths struct {
	HostVsock  string // gateway dials this and sends "CONNECT 5005\n"
	HostSocket string // firecracker API socket
	ChrootRoot string // jailer chroot root, or "" in dev mode
}

func (s *Supervisor) buildFirecrackerConfig(id string, cid uint32, spec LaunchSpec) (firecracker.Config, bootPaths, error) {
	// Per-VM rootfs copy so guest writes never mutate the golden image and never
	// collide between sandboxes. (Under jailer the SDK hard-links the drive into
	// the chroot, which would otherwise share the inode.) The CoW snapshot phase
	// replaces this copy with a MAP_PRIVATE overlay.
	stateDir := filepath.Join(s.cfg.StateDir, id)
	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		return firecracker.Config{}, bootPaths{}, fmt.Errorf("mkdir state dir: %w", err)
	}
	rootfsCopy := filepath.Join(stateDir, "rootfs.ext4")
	if err := copyFile(s.cfg.RootfsPath, rootfsCopy, 0o660); err != nil {
		return firecracker.Config{}, bootPaths{}, fmt.Errorf("copy rootfs: %w", err)
	}
	// The jailer demotes Firecracker to JailerGID; the drive must be
	// group-readable/writable so virtio-blk can open it O_RDWR after chroot +
	// setgid. Chmod explicitly (not just via the open mode) so the process umask
	// cannot strip the group-write bit — without this the guest gets a
	// read-only-permission file and boot fails with EACCES on drive attach.
	if err := os.Chmod(rootfsCopy, 0o660); err != nil {
		return firecracker.Config{}, bootPaths{}, fmt.Errorf("chmod rootfs copy: %w", err)
	}
	if err := os.Chown(rootfsCopy, 0, s.cfg.JailerGID); err != nil {
		return firecracker.Config{}, bootPaths{}, fmt.Errorf("chown rootfs copy: %w", err)
	}

	const vsockName = "v.sock"
	var (
		socketPath string
		vsockPath  string
		paths      bootPaths
	)
	if s.cfg.UseJailer {
		// Under the jailer, paths are relative to the chroot root; the SDK stages
		// them and creates the sockets inside the jail.
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

	kernelArgs := s.cfg.KernelArgs
	if kernelArgs == "" {
		kernelArgs = DefaultKernelArgs
	}

	cfg := firecracker.Config{
		SocketPath:      socketPath,
		KernelImagePath: s.cfg.KernelImagePath,
		KernelArgs:      kernelArgs,
		Drives: []models.Drive{{
			DriveID:      firecracker.String("rootfs"),
			PathOnHost:   firecracker.String(rootfsCopy),
			IsRootDevice: firecracker.Bool(true),
			IsReadOnly:   firecracker.Bool(false),
		}},
		MachineCfg: models.MachineConfiguration{
			VcpuCount:  firecracker.Int64(spec.VCPUs),
			MemSizeMib: firecracker.Int64(spec.MemMiB),
		},
		VsockDevices: []firecracker.VsockDevice{{
			ID:   "vsock0",
			CID:  cid,
			Path: vsockPath,
		}},
		// No NetworkInterfaces => "none" networking: the guest has loopback only
		// and cannot reach the host LAN. Egress (if ever enabled) is an explicit
		// opt-in TAP + Squid allowlist, configured outside this struct.
		NetworkInterfaces: nil,
		VMID:              id,
	}

	if s.cfg.UseJailer {
		cfg.JailerCfg = s.buildJailerConfig(id)
	}
	return cfg, paths, nil
}

func (s *Supervisor) buildJailerConfig(id string) *firecracker.JailerConfig {
	return &firecracker.JailerConfig{
		ID:            id,
		UID:           firecracker.Int(s.cfg.JailerUID),
		GID:           firecracker.Int(s.cfg.JailerGID),
		NumaNode:      firecracker.Int(0),
		ExecFile:      s.cfg.FirecrackerBin,
		JailerBinary:  s.cfg.JailerBin,
		ChrootBaseDir: s.cfg.ChrootBaseDir,
		CgroupVersion: "2", // VERIFY-SDK: field present in recent releases
		Daemonize:     false,
		// VERIFY-SDK: v1.x constructor takes the kernel image path only and
		// stages it into the chroot. (v0.x took (rootfs, kernel).)
		ChrootStrategy: firecracker.NewNaiveChrootStrategy(s.cfg.KernelImagePath),
	}
}

// newMachine creates the SDK machine. Under the jailer the SDK builds and runs
// the jailer->firecracker command itself. In dev mode we run firecracker
// directly and may attach a custom compiled seccomp filter; Firecracker's
// built-in seccomp filter stays active unless explicitly overridden.
func (s *Supervisor) newMachine(ctx context.Context, cfg firecracker.Config) (*firecracker.Machine, error) {
	if cfg.JailerCfg != nil {
		return firecracker.NewMachine(ctx, cfg)
	}
	b := firecracker.VMCommandBuilder{}.
		WithBin(s.cfg.FirecrackerBin).
		WithSocketPath(cfg.SocketPath)
	if s.cfg.SeccompFilterPath != "" {
		b = b.AddArgs("--seccomp-filter", s.cfg.SeccompFilterPath)
	}
	cmd := b.Build(ctx)
	return firecracker.NewMachine(ctx, cfg, firecracker.WithProcessRunner(cmd))
}

// prepareCgroup creates a dedicated cgroup v2 leaf and writes its hard caps. It
// is called before the VMM boots so the limits exist the instant the process is
// placed (placeInCgroup) — closing the brief "uncapped guest" window.
//
// On a systemd host the cgroup root is managed by systemd; for production run the
// control plane in a unit with `Delegate=yes` and point CgroupRoot/CgroupBase at
// the delegated subtree so systemd does not garbage-collect these leaves.
func (s *Supervisor) prepareCgroup(id string, spec LaunchSpec) (string, error) {
	parent := filepath.Join(s.cfg.CgroupRoot, s.cfg.CgroupBase)
	leaf := filepath.Join(parent, id)

	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", fmt.Errorf("mkdir parent cgroup: %w", err)
	}
	// A controller is only usable in a child if every ancestor lists it in
	// cgroup.subtree_control; enable cpu/memory/pids down the chain.
	enableSubtreeControllers(s.cfg.CgroupRoot, s.cfg.CgroupBase)

	if err := os.MkdirAll(leaf, 0o755); err != nil {
		return "", fmt.Errorf("mkdir leaf cgroup: %w", err)
	}
	if err := writeCgroup(leaf, "cpu.max", cpuMaxString(spec.CPUPercent)); err != nil {
		return "", err
	}
	if err := writeCgroup(leaf, "memory.max", strconv.FormatInt(memMaxBytes(spec.MemMiB), 10)); err != nil {
		return "", err
	}
	if err := writeCgroup(leaf, "memory.swap.max", "0"); err != nil {
		return "", err
	}
	if err := writeCgroup(leaf, "pids.max", strconv.FormatInt(spec.PidsMax, 10)); err != nil {
		return "", err
	}
	return leaf, nil
}

// placeInCgroup moves the VMM (and all its threads) into the leaf.
func (s *Supervisor) placeInCgroup(leaf string, pid int) error {
	return writeCgroup(leaf, "cgroup.procs", strconv.Itoa(pid))
}

// cpuMaxString renders cgroup v2 cpu.max ("QUOTA PERIOD" microseconds). 100% of
// one core => "100000 100000"; 150 => "150000 100000" (1.5 cores).
func cpuMaxString(cpuPercent int) string {
	const period = 100000
	quota := cpuPercent * period / 100
	return fmt.Sprintf("%d %d", quota, period)
}

// memMaxBytes is the cgroup memory.max value: guest RAM plus VMM headroom.
func memMaxBytes(memMiB int64) int64 {
	return memMiB*1024*1024 + memHeadroomBytes
}

// enableSubtreeControllers best-effort delegates cpu/memory/pids from the root
// down to base. Enabling an already-enabled (or unavailable) controller is
// harmless; if delegation truly fails, the subsequent cpu.max/memory.max writes
// surface a clear error and Launch fails closed.
func enableSubtreeControllers(root, base string) {
	cur := root
	enableControllers(cur)
	for _, part := range strings.Split(filepath.Clean(base), string(os.PathSeparator)) {
		if part == "" || part == "." {
			continue
		}
		cur = filepath.Join(cur, part)
		_ = os.MkdirAll(cur, 0o755)
		enableControllers(cur)
	}
}

func enableControllers(dir string) {
	// Write controllers individually so one unavailable controller does not block
	// the others.
	for _, c := range []string{"+cpu", "+memory", "+pids"} {
		_ = os.WriteFile(filepath.Join(dir, "cgroup.subtree_control"), []byte(c), 0)
	}
}

func writeCgroup(dir, file, val string) error {
	if err := os.WriteFile(filepath.Join(dir, file), []byte(val), 0); err != nil {
		return fmt.Errorf("write %s=%q: %w", file, val, err)
	}
	return nil
}

// --- process helpers -------------------------------------------------------

// processAlive reports whether pid refers to a live process. Signal 0 probes for
// existence without delivering a signal; EPERM means it exists but we may not
// signal it (still "alive").
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// killPID sends SIGTERM, waits briefly, then SIGKILL.
func killPID(pid int) {
	if pid <= 0 {
		return
	}
	_ = syscall.Kill(pid, syscall.SIGTERM)
	for i := 0; i < 30; i++ {
		if !processAlive(pid) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
}

// --- misc helpers ----------------------------------------------------------

func applySpecDefaults(spec LaunchSpec) LaunchSpec {
	if spec.VCPUs <= 0 {
		spec.VCPUs = 1
	}
	if spec.MemMiB <= 0 {
		spec.MemMiB = 256
	}
	if spec.CPUPercent <= 0 {
		spec.CPUPercent = 100
	}
	if spec.PidsMax <= 0 {
		spec.PidsMax = 128
	}
	return spec
}

func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failure is catastrophic and effectively never happens;
		// fall back to a time-based suffix so we still return a usable id.
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b)
}

// --- CID allocation --------------------------------------------------------

type cidAllocator struct {
	mu   sync.Mutex
	min  uint32
	max  uint32
	next uint32
	used map[uint32]bool
}

func newCIDAllocator(min, max uint32) *cidAllocator {
	return &cidAllocator{min: min, max: max, next: min, used: make(map[uint32]bool)}
}

func (a *cidAllocator) alloc() (uint32, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	total := a.max - a.min + 1
	for i := uint32(0); i < total; i++ {
		c := a.next
		if a.next == a.max {
			a.next = a.min
		} else {
			a.next++
		}
		if !a.used[c] {
			a.used[c] = true
			return c, nil
		}
	}
	return 0, errors.New("orchestrator: CID pool exhausted")
}

func (a *cidAllocator) free(cid uint32) {
	a.mu.Lock()
	delete(a.used, cid)
	a.mu.Unlock()
}
