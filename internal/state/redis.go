// Package state is the Redis-backed control-plane store. It provides:
//
//   - Durable sandbox records implementing sandbox.Recorder (for restart
//     reconciliation).
//   - Liveness heartbeats (TTL keys).
//   - Operator/WebSocket session → sandbox mapping.
//   - Per-tenant token-bucket rate limiting (atomic via a Lua script).
//
// The package is OS-agnostic and unit-tested against miniredis, so it builds and
// tests on any platform even though the rest of the control plane is Linux-only.
package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/yourorg/sandbox-platform/internal/sandbox"
)

// Key prefixes / names.
const (
	keyActive    = "sandboxes:active" // set of active sandbox ids
	prefixRecord = "sandbox:"         // sandbox:<id> -> JSON record
	prefixBeat   = "heartbeat:"       // heartbeat:<id> -> ts (TTL)
	prefixSess   = "session:"         // session:<sid> -> sandbox id (TTL)
	prefixBucket = "ratelimit:"       // ratelimit:<name> -> token bucket hash
)

// ErrNotFound is returned when a looked-up key does not exist.
var ErrNotFound = errors.New("state: not found")

// Store is the Redis-backed implementation of sandbox.Recorder plus heartbeat,
// session, and rate-limit helpers. It is safe for concurrent use.
type Store struct {
	rdb *redis.Client
	now func() time.Time // injectable clock for tests
}

// Options configures the Redis connection.
type Options struct {
	Addr     string
	Password string
	DB       int
}

// New connects to Redis and verifies connectivity with a PING.
func New(ctx context.Context, opts Options) (*Store, error) {
	if opts.Addr == "" {
		opts.Addr = "127.0.0.1:6379"
	}
	rdb := redis.NewClient(&redis.Options{Addr: opts.Addr, Password: opts.Password, DB: opts.DB})
	if err := rdb.Ping(ctx).Err(); err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("state: redis ping %s: %w", opts.Addr, err)
	}
	return &Store{rdb: rdb, now: time.Now}, nil
}

// NewWithClient wraps an existing client (used by tests with miniredis).
func NewWithClient(rdb *redis.Client) *Store {
	return &Store{rdb: rdb, now: time.Now}
}

// Close releases the underlying client.
func (s *Store) Close() error { return s.rdb.Close() }

// --- sandbox.Recorder ------------------------------------------------------

// Save upserts a record and adds it to the active set atomically.
func (s *Store) Save(ctx context.Context, r sandbox.Record) error {
	b, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("state: marshal record: %w", err)
	}
	pipe := s.rdb.TxPipeline()
	pipe.Set(ctx, prefixRecord+r.ID, b, 0)
	pipe.SAdd(ctx, keyActive, r.ID)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("state: save record %s: %w", r.ID, err)
	}
	return nil
}

// Remove deletes a record, its active-set membership, and its heartbeat.
func (s *Store) Remove(ctx context.Context, id string) error {
	pipe := s.rdb.TxPipeline()
	pipe.Del(ctx, prefixRecord+id)
	pipe.SRem(ctx, keyActive, id)
	pipe.Del(ctx, prefixBeat+id)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("state: remove record %s: %w", id, err)
	}
	return nil
}

// All returns every persisted record, self-healing any stale active-set entries
// whose record key has expired or been deleted out of band.
func (s *Store) All(ctx context.Context) ([]sandbox.Record, error) {
	ids, err := s.rdb.SMembers(ctx, keyActive).Result()
	if err != nil {
		return nil, fmt.Errorf("state: list active: %w", err)
	}
	out := make([]sandbox.Record, 0, len(ids))
	for _, id := range ids {
		b, err := s.rdb.Get(ctx, prefixRecord+id).Bytes()
		if errors.Is(err, redis.Nil) {
			_ = s.rdb.SRem(ctx, keyActive, id).Err()
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("state: get record %s: %w", id, err)
		}
		var r sandbox.Record
		if err := json.Unmarshal(b, &r); err != nil {
			return nil, fmt.Errorf("state: unmarshal record %s: %w", id, err)
		}
		out = append(out, r)
	}
	return out, nil
}

// --- heartbeats ------------------------------------------------------------

// Heartbeat refreshes a liveness key with a TTL. Absence implies the sandbox is
// considered dead.
func (s *Store) Heartbeat(ctx context.Context, id string, ttl time.Duration) error {
	return s.rdb.Set(ctx, prefixBeat+id, s.now().UnixMilli(), ttl).Err()
}

// Alive reports whether a (non-expired) heartbeat exists for id.
func (s *Store) Alive(ctx context.Context, id string) (bool, error) {
	n, err := s.rdb.Exists(ctx, prefixBeat+id).Result()
	if err != nil {
		return false, fmt.Errorf("state: alive %s: %w", id, err)
	}
	return n > 0, nil
}

// --- sessions --------------------------------------------------------------

// BindSession maps an operator/WebSocket session id to a sandbox id with a TTL.
func (s *Store) BindSession(ctx context.Context, sessionID, sandboxID string, ttl time.Duration) error {
	return s.rdb.Set(ctx, prefixSess+sessionID, sandboxID, ttl).Err()
}

// SessionSandbox returns the sandbox bound to a session, or ErrNotFound.
func (s *Store) SessionSandbox(ctx context.Context, sessionID string) (string, error) {
	v, err := s.rdb.Get(ctx, prefixSess+sessionID).Result()
	if errors.Is(err, redis.Nil) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("state: session lookup: %w", err)
	}
	return v, nil
}

// UnbindSession removes a session mapping.
func (s *Store) UnbindSession(ctx context.Context, sessionID string) error {
	return s.rdb.Del(ctx, prefixSess+sessionID).Err()
}

// --- rate limiting ---------------------------------------------------------

// tokenBucketScript implements an atomic token bucket. KEYS[1] is the bucket;
// ARGV = capacity, refillPerSec, nowMillis, needed, ttlSeconds. It returns 1 if
// allowed, 0 if denied. No Lua math library is used (ttl is precomputed in Go)
// so it runs identically on real Redis and miniredis.
var tokenBucketScript = redis.NewScript(`
local capacity = tonumber(ARGV[1])
local refill   = tonumber(ARGV[2])
local now      = tonumber(ARGV[3])
local needed   = tonumber(ARGV[4])
local ttl      = tonumber(ARGV[5])
local vals = redis.call('HMGET', KEYS[1], 'tokens', 'ts')
local tokens = tonumber(vals[1])
local ts = tonumber(vals[2])
if tokens == nil then tokens = capacity; ts = now end
local elapsed = now - ts
if elapsed < 0 then elapsed = 0 end
tokens = tokens + (elapsed / 1000.0) * refill
if tokens > capacity then tokens = capacity end
local allowed = 0
if tokens >= needed then allowed = 1; tokens = tokens - needed end
redis.call('HSET', KEYS[1], 'tokens', tokens, 'ts', now)
redis.call('EXPIRE', KEYS[1], ttl)
return allowed
`)

// Allow applies a token-bucket rate limit keyed by name. capacity is the burst
// size, refillPerSec the steady-state rate (must be > 0), needed the cost of
// this call. It returns whether the call is permitted.
func (s *Store) Allow(ctx context.Context, name string, capacity, refillPerSec, needed float64) (bool, error) {
	if refillPerSec <= 0 {
		return false, errors.New("state: refillPerSec must be > 0")
	}
	now := s.now().UnixMilli()
	ttl := int64(capacity/refillPerSec) + 2 // long enough for a full refill
	res, err := tokenBucketScript.Run(ctx, s.rdb,
		[]string{prefixBucket + name},
		capacity, refillPerSec, now, needed, ttl).Int()
	if err != nil {
		return false, fmt.Errorf("state: rate limit %s: %w", name, err)
	}
	return res == 1, nil
}
