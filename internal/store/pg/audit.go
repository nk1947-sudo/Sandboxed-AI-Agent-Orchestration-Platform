package pg

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// AuditRepo persists an immutable trail of security-relevant operator actions.
type AuditRepo struct{ pool *pgxpool.Pool }

// Log records one audit entry. userID may be empty for unauthenticated actions
// (e.g. a failed login). detail is stored as JSONB.
func (r *AuditRepo) Log(ctx context.Context, userID, action, target string, detail map[string]any) error {
	var detailArg any
	if len(detail) > 0 {
		b, err := json.Marshal(detail)
		if err != nil {
			return fmt.Errorf("pg: marshal audit detail: %w", err)
		}
		detailArg = string(b) // cast text->jsonb in SQL to avoid bytea ambiguity
	}
	_, err := r.pool.Exec(ctx,
		`INSERT INTO audit_log(user_id, action, target, detail) VALUES($1,$2,$3,$4::jsonb)`,
		nullStr(userID), action, target, detailArg)
	if err != nil {
		return fmt.Errorf("pg: audit log: %w", err)
	}
	return nil
}

// Recent returns the most recent audit entries (admin view).
func (r *AuditRepo) Recent(ctx context.Context, limit int) ([]AuditEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := r.pool.Query(ctx,
		`SELECT id, user_id, action, target, detail, ts
		 FROM audit_log ORDER BY ts DESC LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("pg: recent audit: %w", err)
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var detail []byte
		if err := rows.Scan(&e.ID, &e.UserID, &e.Action, &e.Target, &detail, &e.TS); err != nil {
			return nil, fmt.Errorf("pg: scan audit: %w", err)
		}
		if len(detail) > 0 {
			_ = json.Unmarshal(detail, &e.Detail)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
