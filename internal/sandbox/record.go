// Package sandbox holds OS-agnostic domain types shared between the Linux-only
// host control plane (internal/orchestrator) and the portable persistence layer
// (internal/state). Keeping the record shape and the persistence contract here
// lets both sides agree without a build-tag leaking across packages and without
// an import cycle (orchestrator → sandbox ← state).
package sandbox

import (
	"context"
	"time"
)

// Status is the lifecycle state of a sandbox.
type Status string

const (
	StatusRunning Status = "running"
	StatusStopped Status = "stopped"
)

// Spec is the resource shape of a sandbox. It mirrors orchestrator.LaunchSpec so
// records survive across control-plane restarts.
type Spec struct {
	VCPUs      int64 `json:"vcpus"`
	MemMiB     int64 `json:"mem_mib"`
	CPUPercent int   `json:"cpu_percent"`
	PidsMax    int64 `json:"pids_max"`
}

// Record is the durable description of a sandbox, persisted so the control plane
// can reconcile host state (cgroups, chroots, CIDs) after a crash or restart.
type Record struct {
	ID           string    `json:"id"`
	CID          uint32    `json:"cid"`
	PID          int       `json:"pid"`
	VsockUDSPath string    `json:"vsock_uds_path"`
	SocketPath   string    `json:"socket_path"`
	ChrootDir    string    `json:"chroot_dir"`
	CgroupPath   string    `json:"cgroup_path"`
	Status       Status    `json:"status"`
	CreatedAt    time.Time `json:"created_at"`
	Spec         Spec      `json:"spec"`
}

// Recorder is the persistence contract the orchestrator depends on. A nil
// Recorder disables persistence (single-process, ephemeral operation) — the
// orchestrator treats every method as best-effort and never blocks a VM
// lifecycle on a persistence failure.
type Recorder interface {
	// Save upserts a record.
	Save(ctx context.Context, r Record) error
	// Remove deletes a record by id (idempotent).
	Remove(ctx context.Context, id string) error
	// All returns every persisted record (used for restart reconciliation).
	All(ctx context.Context) ([]Record, error)
}
