// Package pg is the PostgreSQL-backed durable store for the control plane. It
// complements internal/state (Redis), which keeps the ephemeral HITL queue,
// rate-limit buckets, heartbeats, and live reconciliation. Postgres owns the
// relational, long-lived data: user accounts, login sessions, sandbox history,
// snapshot metadata, terminal transcripts, and the audit log.
//
// The package is OS-agnostic (no build tags) so it compiles and unit-tests on
// any platform, like internal/state.
package pg

import "time"

// Role is a coarse authorization level for a user account.
type Role string

const (
	RoleAdmin    Role = "admin"
	RoleOperator Role = "operator"
	RoleViewer   Role = "viewer"
)

// User is an operator account.
type User struct {
	ID           string    `json:"id"`
	Username     string    `json:"username"`
	PasswordHash string    `json:"-"` // never serialized
	Role         Role      `json:"role"`
	Disabled     bool      `json:"disabled"`
	CreatedAt    time.Time `json:"created_at"`
}

// Session is a server-side login session. The ID is the opaque value carried in
// the HttpOnly cookie — it is NOT the API token.
type Session struct {
	ID         string
	UserID     string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	LastSeenAt time.Time
	UserAgent  string
	IP         string
}

// SandboxStatus is the durable lifecycle state of a sandbox in history.
type SandboxStatus string

const (
	SandboxRunning  SandboxStatus = "running"
	SandboxStopped  SandboxStatus = "stopped"
	SandboxArchived SandboxStatus = "archived"
)

// Sandbox is the durable record of a sandbox, surviving control-plane restarts
// and termination so it can appear in history and be resumed.
type Sandbox struct {
	ID         string        `json:"id"`
	OwnerID    string        `json:"owner_id"`
	Name       string        `json:"name"`
	Status     SandboxStatus `json:"status"`
	VCPUs      int           `json:"vcpus"`
	MemMiB     int           `json:"mem_mib"`
	CPUPercent int           `json:"cpu_percent"`
	PidsMax    int           `json:"pids_max"`
	CID        int64         `json:"cid"`
	CreatedAt  time.Time     `json:"created_at"`
	StoppedAt  *time.Time    `json:"stopped_at,omitempty"`
	LastUsedAt time.Time     `json:"last_used_at"`
}

// Snapshot is the metadata for one captured VM state (memory + device state +
// retained disk) used to resume a stopped sandbox to its exact prior state.
type Snapshot struct {
	ID            string    `json:"id"`
	SandboxID     string    `json:"sandbox_id"`
	MemFilePath   string    `json:"mem_file_path"`
	StateFilePath string    `json:"state_file_path"`
	DiskFilePath  string    `json:"disk_file_path"`
	SizeBytes     int64     `json:"size_bytes"`
	GuestVCPUs    int       `json:"guest_vcpus"`
	GuestMemMiB   int       `json:"guest_mem_mib"`
	Label         string    `json:"label"`
	CreatedAt     time.Time `json:"created_at"`
}

// TranscriptKind classifies one line of a sandbox's terminal transcript.
type TranscriptKind string

const (
	KindInput  TranscriptKind = "input"  // a script the operator ran
	KindOutput TranscriptKind = "output" // streamed guest output
	KindExit   TranscriptKind = "exit"   // exit code of a script
	KindHITL   TranscriptKind = "hitl"   // approval pending/approved/rejected
	KindSystem TranscriptKind = "system" // session/system notices
)

// TranscriptLine is one persisted line of a sandbox's terminal session.
type TranscriptLine struct {
	ID        int64          `json:"-"`
	SandboxID string         `json:"-"`
	Seq       int64          `json:"seq"`
	TS        time.Time      `json:"ts"`
	Kind      TranscriptKind `json:"kind"`
	Data      string         `json:"data,omitempty"`
	ExitCode  *int           `json:"exit_code,omitempty"`
}

// AuditEntry is one immutable record of a security-relevant operator action.
type AuditEntry struct {
	ID     int64
	UserID *string
	Action string
	Target string
	Detail map[string]any
	TS     time.Time
}
