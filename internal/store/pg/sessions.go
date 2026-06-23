package pg

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SessionRepo persists login sessions. A session ID is the opaque value carried
// in the HttpOnly cookie (never the API token).
type SessionRepo struct{ pool *pgxpool.Pool }

// Create inserts a session row. id must be a freshly generated random UUID.
func (r *SessionRepo) Create(ctx context.Context, id, userID string, expiresAt time.Time, userAgent, ip string) error {
	var ipArg any
	if ip != "" {
		ipArg = ip
	}
	_, err := r.pool.Exec(ctx,
		`INSERT INTO sessions(id, user_id, expires_at, user_agent, ip)
		 VALUES($1,$2,$3,$4,$5)`,
		id, userID, expiresAt, userAgent, ipArg)
	if err != nil {
		return fmt.Errorf("pg: create session: %w", err)
	}
	return nil
}

// Resolve returns the user behind a non-expired session and refreshes
// last_seen_at. Expired or unknown sessions return ErrNotFound. A disabled user
// also yields ErrNotFound so a deactivated account is locked out immediately.
func (r *SessionRepo) Resolve(ctx context.Context, sessionID string) (User, error) {
	row := r.pool.QueryRow(ctx,
		`UPDATE sessions SET last_seen_at = now()
		 WHERE id = $1 AND expires_at > now()
		 RETURNING user_id`, sessionID)
	var userID string
	if err := row.Scan(&userID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return User{}, ErrNotFound
		}
		return User{}, fmt.Errorf("pg: resolve session: %w", err)
	}
	u, err := scanUser(r.pool.QueryRow(ctx,
		`SELECT `+userCols+` FROM users WHERE id=$1 AND disabled=false`, userID))
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("pg: resolve session user: %w", err)
	}
	return u, nil
}

// Revoke deletes a session (logout). Idempotent.
func (r *SessionRepo) Revoke(ctx context.Context, sessionID string) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM sessions WHERE id=$1`, sessionID)
	if err != nil {
		return fmt.Errorf("pg: revoke session: %w", err)
	}
	return nil
}

// DeleteExpired removes expired sessions and returns the count deleted. Run
// periodically from the control-plane health loop.
func (r *SessionRepo) DeleteExpired(ctx context.Context) (int64, error) {
	tag, err := r.pool.Exec(ctx, `DELETE FROM sessions WHERE expires_at <= now()`)
	if err != nil {
		return 0, fmt.Errorf("pg: delete expired sessions: %w", err)
	}
	return tag.RowsAffected(), nil
}
