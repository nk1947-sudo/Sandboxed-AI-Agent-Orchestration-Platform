package pg

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when a looked-up row does not exist.
var ErrNotFound = errors.New("pg: not found")

// Config holds the PostgreSQL connection parameters (12-factor, from PG_* env).
type Config struct {
	Host     string
	Port     string
	User     string
	Password string
	DB       string
	SSLMode  string // disable | require | verify-full ...
}

// DSN renders a libpq connection URL, URL-escaping the credentials.
func (c Config) DSN() string {
	if c.Port == "" {
		c.Port = "5432"
	}
	if c.SSLMode == "" {
		c.SSLMode = "disable"
	}
	return fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=%s",
		url.QueryEscape(c.User), url.QueryEscape(c.Password),
		c.Host, c.Port, url.PathEscape(c.DB), c.SSLMode)
}

// Store is the Postgres-backed durable store. It exposes one repository per
// aggregate; all share a single pgx connection pool and are safe for concurrent
// use.
type Store struct {
	pool *pgxpool.Pool

	Users       *UserRepo
	Sessions    *SessionRepo
	Sandboxes   *SandboxRepo
	Snapshots   *SnapshotRepo
	Transcripts *TranscriptRepo
	Audit       *AuditRepo
}

// New opens a pooled connection, verifies it with a ping, and wires the repos.
func New(ctx context.Context, cfg Config) (*Store, error) {
	pool, err := pgxpool.New(ctx, cfg.DSN())
	if err != nil {
		return nil, fmt.Errorf("pg: connect: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pg: ping %s:%s: %w", cfg.Host, cfg.Port, err)
	}
	return newWithPool(pool), nil
}

// NewWithPool wraps an existing pool (used by tests).
func NewWithPool(pool *pgxpool.Pool) *Store { return newWithPool(pool) }

func newWithPool(pool *pgxpool.Pool) *Store {
	s := &Store{pool: pool}
	s.Users = &UserRepo{pool: pool}
	s.Sessions = &SessionRepo{pool: pool}
	s.Sandboxes = &SandboxRepo{pool: pool}
	s.Snapshots = &SnapshotRepo{pool: pool}
	s.Transcripts = &TranscriptRepo{pool: pool}
	s.Audit = &AuditRepo{pool: pool}
	return s
}

// Pool exposes the underlying pool (for advanced callers / tests).
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Close releases all pooled connections.
func (s *Store) Close() { s.pool.Close() }
