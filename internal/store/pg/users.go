package pg

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// UserRepo persists operator accounts.
type UserRepo struct{ pool *pgxpool.Pool }

const userCols = `id, username, password_hash, role, disabled, created_at`

func scanUser(row pgx.Row) (User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.Disabled, &u.CreatedAt)
	return u, err
}

// Create inserts a new user and returns it. The caller passes an already-hashed
// password (see internal/auth.HashPassword).
func (r *UserRepo) Create(ctx context.Context, username, passwordHash string, role Role) (User, error) {
	if role == "" {
		role = RoleOperator
	}
	u, err := scanUser(r.pool.QueryRow(ctx,
		`INSERT INTO users(username, password_hash, role)
		 VALUES($1,$2,$3) RETURNING `+userCols,
		username, passwordHash, string(role)))
	if err != nil {
		return User{}, fmt.Errorf("pg: create user: %w", err)
	}
	return u, nil
}

// ByUsername returns the user with the given username, or ErrNotFound.
func (r *UserRepo) ByUsername(ctx context.Context, username string) (User, error) {
	u, err := scanUser(r.pool.QueryRow(ctx,
		`SELECT `+userCols+` FROM users WHERE username=$1`, username))
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("pg: user by username: %w", err)
	}
	return u, nil
}

// ByID returns the user with the given id, or ErrNotFound.
func (r *UserRepo) ByID(ctx context.Context, id string) (User, error) {
	u, err := scanUser(r.pool.QueryRow(ctx,
		`SELECT `+userCols+` FROM users WHERE id=$1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("pg: user by id: %w", err)
	}
	return u, nil
}

// Count returns the number of user accounts (used to decide admin bootstrap).
func (r *UserRepo) Count(ctx context.Context) (int, error) {
	var n int
	if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n); err != nil {
		return 0, fmt.Errorf("pg: count users: %w", err)
	}
	return n, nil
}

// List returns all users ordered by creation time (admin view).
func (r *UserRepo) List(ctx context.Context) ([]User, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT `+userCols+` FROM users ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("pg: list users: %w", err)
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, fmt.Errorf("pg: scan user: %w", err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// SetDisabled toggles a user's disabled flag.
func (r *UserRepo) SetDisabled(ctx context.Context, id string, disabled bool) error {
	_, err := r.pool.Exec(ctx, `UPDATE users SET disabled=$2 WHERE id=$1`, id, disabled)
	if err != nil {
		return fmt.Errorf("pg: set disabled: %w", err)
	}
	return nil
}
