package pg

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TranscriptRepo persists per-sandbox terminal transcripts (chat-like history).
type TranscriptRepo struct{ pool *pgxpool.Pool }

// Append inserts one transcript line, assigning the next per-sandbox seq
// atomically. exitCode may be nil for non-exit kinds. Returns the assigned seq.
func (r *TranscriptRepo) Append(ctx context.Context, sandboxID string, kind TranscriptKind, data string, exitCode *int) (int64, error) {
	var seq int64
	err := r.pool.QueryRow(ctx,
		`INSERT INTO transcript_lines(sandbox_id, seq, kind, data, exit_code)
		 VALUES($1,
		        COALESCE((SELECT max(seq) FROM transcript_lines WHERE sandbox_id=$1), 0) + 1,
		        $2, $3, $4)
		 RETURNING seq`,
		sandboxID, string(kind), data, exitCode).Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("pg: append transcript: %w", err)
	}
	return seq, nil
}

// List returns transcript lines for a sandbox after the given seq (0 = all),
// ordered by seq, capped at limit (0 = no cap).
func (r *TranscriptRepo) List(ctx context.Context, sandboxID string, afterSeq int64, limit int) ([]TranscriptLine, error) {
	q := `SELECT seq, ts, kind, data, exit_code FROM transcript_lines
	      WHERE sandbox_id=$1 AND seq > $2 ORDER BY seq`
	args := []any{sandboxID, afterSeq}
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("pg: list transcript: %w", err)
	}
	defer rows.Close()
	var out []TranscriptLine
	for rows.Next() {
		var l TranscriptLine
		l.SandboxID = sandboxID
		if err := rows.Scan(&l.Seq, &l.TS, &l.Kind, &l.Data, &l.ExitCode); err != nil {
			return nil, fmt.Errorf("pg: scan transcript: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// PruneOlderThanDays deletes transcript lines older than the given number of
// days and returns the count removed. Run periodically for retention.
func (r *TranscriptRepo) PruneOlderThanDays(ctx context.Context, days int) (int64, error) {
	tag, err := r.pool.Exec(ctx,
		`DELETE FROM transcript_lines WHERE ts < now() - ($1 * interval '1 day')`, days)
	if err != nil {
		return 0, fmt.Errorf("pg: prune transcript: %w", err)
	}
	return tag.RowsAffected(), nil
}
