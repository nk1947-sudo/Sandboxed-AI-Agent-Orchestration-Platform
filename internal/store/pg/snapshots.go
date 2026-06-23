package pg

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SnapshotRepo persists snapshot metadata (the files themselves live on disk
// under STATE_DIR/snapshots; these rows track their paths for resume + GC).
type SnapshotRepo struct{ pool *pgxpool.Pool }

const snapshotCols = `id, sandbox_id, mem_file_path, state_file_path, disk_file_path, size_bytes, guest_vcpus, guest_mem_mib, label, created_at`

func scanSnapshot(row pgx.Row) (Snapshot, error) {
	var s Snapshot
	err := row.Scan(&s.ID, &s.SandboxID, &s.MemFilePath, &s.StateFilePath,
		&s.DiskFilePath, &s.SizeBytes, &s.GuestVCPUs, &s.GuestMemMiB, &s.Label, &s.CreatedAt)
	return s, err
}

// Create inserts a snapshot row and returns it with its generated id.
func (r *SnapshotRepo) Create(ctx context.Context, s Snapshot) (Snapshot, error) {
	out, err := scanSnapshot(r.pool.QueryRow(ctx,
		`INSERT INTO snapshots(sandbox_id, mem_file_path, state_file_path, disk_file_path, size_bytes, guest_vcpus, guest_mem_mib, label)
		 VALUES($1,$2,$3,$4,$5,$6,$7,$8) RETURNING `+snapshotCols,
		s.SandboxID, s.MemFilePath, s.StateFilePath, s.DiskFilePath,
		s.SizeBytes, s.GuestVCPUs, s.GuestMemMiB, s.Label))
	if err != nil {
		return Snapshot{}, fmt.Errorf("pg: create snapshot: %w", err)
	}
	return out, nil
}

// Latest returns the most recent snapshot for a sandbox, or ErrNotFound.
func (r *SnapshotRepo) Latest(ctx context.Context, sandboxID string) (Snapshot, error) {
	s, err := scanSnapshot(r.pool.QueryRow(ctx,
		`SELECT `+snapshotCols+` FROM snapshots WHERE sandbox_id=$1 ORDER BY created_at DESC LIMIT 1`,
		sandboxID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Snapshot{}, ErrNotFound
	}
	if err != nil {
		return Snapshot{}, fmt.Errorf("pg: latest snapshot: %w", err)
	}
	return s, nil
}

// ListBySandbox returns all snapshots for a sandbox, newest first.
func (r *SnapshotRepo) ListBySandbox(ctx context.Context, sandboxID string) ([]Snapshot, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT `+snapshotCols+` FROM snapshots WHERE sandbox_id=$1 ORDER BY created_at DESC`,
		sandboxID)
	if err != nil {
		return nil, fmt.Errorf("pg: list snapshots: %w", err)
	}
	defer rows.Close()
	var out []Snapshot
	for rows.Next() {
		s, err := scanSnapshot(rows)
		if err != nil {
			return nil, fmt.Errorf("pg: scan snapshot: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Delete removes a snapshot row by id (the caller deletes the files on disk).
func (r *SnapshotRepo) Delete(ctx context.Context, id string) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM snapshots WHERE id=$1`, id)
	if err != nil {
		return fmt.Errorf("pg: delete snapshot: %w", err)
	}
	return nil
}
