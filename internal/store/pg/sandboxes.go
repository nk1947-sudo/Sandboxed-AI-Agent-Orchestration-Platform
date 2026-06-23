package pg

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SandboxRepo persists the durable sandbox history.
type SandboxRepo struct{ pool *pgxpool.Pool }

const sandboxCols = `id, owner_id, name, status, vcpus, mem_mib, cpu_percent, pids_max, cid, created_at, stopped_at, last_used_at`

func scanSandbox(row pgx.Row) (Sandbox, error) {
	var s Sandbox
	var ownerID *string
	var cid *int64
	err := row.Scan(&s.ID, &ownerID, &s.Name, &s.Status, &s.VCPUs, &s.MemMiB,
		&s.CPUPercent, &s.PidsMax, &cid, &s.CreatedAt, &s.StoppedAt, &s.LastUsedAt)
	if err != nil {
		return Sandbox{}, err
	}
	if ownerID != nil {
		s.OwnerID = *ownerID
	}
	if cid != nil {
		s.CID = *cid
	}
	return s, nil
}

// Upsert inserts or updates a sandbox row (status running on launch). Existing
// rows keep their created_at; name is preserved unless provided non-empty.
func (r *SandboxRepo) Upsert(ctx context.Context, s Sandbox) error {
	_, err := r.pool.Exec(ctx,
		`INSERT INTO sandboxes(id, owner_id, name, status, vcpus, mem_mib, cpu_percent, pids_max, cid, last_used_at)
		 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9, now())
		 ON CONFLICT (id) DO UPDATE SET
		   status       = EXCLUDED.status,
		   cid          = EXCLUDED.cid,
		   name         = COALESCE(NULLIF(EXCLUDED.name,''), sandboxes.name),
		   last_used_at = now(),
		   stopped_at   = NULL`,
		s.ID, nullStr(s.OwnerID), s.Name, string(s.Status),
		s.VCPUs, s.MemMiB, s.CPUPercent, s.PidsMax, nullInt64(s.CID))
	if err != nil {
		return fmt.Errorf("pg: upsert sandbox: %w", err)
	}
	return nil
}

// MarkStopped flags a sandbox stopped (after a snapshot is captured).
func (r *SandboxRepo) MarkStopped(ctx context.Context, id string) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE sandboxes SET status='stopped', stopped_at=now(), cid=NULL WHERE id=$1`, id)
	if err != nil {
		return fmt.Errorf("pg: mark stopped: %w", err)
	}
	return nil
}

// SetStatus updates only the status (and clears stopped_at when running).
func (r *SandboxRepo) SetStatus(ctx context.Context, id string, status SandboxStatus) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE sandboxes SET status=$2,
		   stopped_at = CASE WHEN $2='running' THEN NULL ELSE stopped_at END,
		   last_used_at = now()
		 WHERE id=$1`, id, string(status))
	if err != nil {
		return fmt.Errorf("pg: set status: %w", err)
	}
	return nil
}

// Rename sets a human-friendly name.
func (r *SandboxRepo) Rename(ctx context.Context, id, name string) error {
	_, err := r.pool.Exec(ctx, `UPDATE sandboxes SET name=$2 WHERE id=$1`, id, name)
	if err != nil {
		return fmt.Errorf("pg: rename sandbox: %w", err)
	}
	return nil
}

// Archive marks a sandbox archived (history hidden, snapshot deletable).
func (r *SandboxRepo) Archive(ctx context.Context, id string) error {
	return r.SetStatus(ctx, id, SandboxArchived)
}

// ByID returns a sandbox or ErrNotFound.
func (r *SandboxRepo) ByID(ctx context.Context, id string) (Sandbox, error) {
	s, err := scanSandbox(r.pool.QueryRow(ctx,
		`SELECT `+sandboxCols+` FROM sandboxes WHERE id=$1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Sandbox{}, ErrNotFound
	}
	if err != nil {
		return Sandbox{}, fmt.Errorf("pg: sandbox by id: %w", err)
	}
	return s, nil
}

// ListByOwner returns a user's non-archived sandbox history, newest first.
// An admin (ownerID == "") sees every sandbox.
func (r *SandboxRepo) ListByOwner(ctx context.Context, ownerID string) ([]Sandbox, error) {
	var rows pgx.Rows
	var err error
	if ownerID == "" {
		rows, err = r.pool.Query(ctx,
			`SELECT `+sandboxCols+` FROM sandboxes WHERE status <> 'archived' ORDER BY last_used_at DESC`)
	} else {
		rows, err = r.pool.Query(ctx,
			`SELECT `+sandboxCols+` FROM sandboxes WHERE owner_id=$1 AND status <> 'archived' ORDER BY last_used_at DESC`,
			ownerID)
	}
	if err != nil {
		return nil, fmt.Errorf("pg: list sandboxes: %w", err)
	}
	defer rows.Close()
	var out []Sandbox
	for rows.Next() {
		s, err := scanSandbox(rows)
		if err != nil {
			return nil, fmt.Errorf("pg: scan sandbox: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Touch refreshes last_used_at (e.g. when a terminal session opens).
func (r *SandboxRepo) Touch(ctx context.Context, id string) error {
	_, err := r.pool.Exec(ctx, `UPDATE sandboxes SET last_used_at=now() WHERE id=$1`, id)
	return err
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullInt64(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}
