// Package hitl implements the Human-in-the-Loop approval gate that sits
// between the API gateway and the vsock execution layer.
//
// Benign requests (echo, ls, simple Python) pass through immediately.
// Sensitive requests (package installs, network tools, writes outside
// /workspace, privilege escalation) are held in Redis until an operator
// approves or rejects them.
//
// One audit entry is written per terminal decision and kept for 1 year,
// independent of the approval record TTL.
package hitl

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// Classification is the HITL verdict for a script or structured tool call.
type Classification string

const (
	ClassBenign    Classification = "benign"
	ClassSensitive Classification = "sensitive"
)

// Decision is the operator's verdict on a pending approval.
type Decision string

const (
	DecisionApprove Decision = "approve"
	DecisionReject  Decision = "reject"
)

// State is the lifecycle state of one approval record.
type State string

const (
	StatePending  State = "pending"
	StateApproved State = "approved"
	StateRejected State = "rejected"
)

// Sentinel errors — matchable with errors.Is.
var (
	ErrNotFound      = errors.New("hitl: approval not found")
	ErrAlreadyDecided = errors.New("hitl: approval already decided")
)

// ApprovalRecord is persisted for every sensitive request.
type ApprovalRecord struct {
	ID          string     `json:"id"`
	SandboxID   string     `json:"sandbox_id"`
	Script      string     `json:"script"`
	Reason      string     `json:"reason"`
	State       State      `json:"state"`
	SubmittedAt time.Time  `json:"submitted_at"`
	DecidedBy   string     `json:"decided_by,omitempty"`
	DecidedAt   *time.Time `json:"decided_at,omitempty"`
}

// Config holds Gate tuning parameters.
type Config struct {
	// ApproveTTL is how long an approval record lives before expiring.
	// Zero defaults to 1 hour; expired records are treated as implicit rejects.
	ApproveTTL time.Duration
}

// Gate classifies requests and manages the HITL approval lifecycle in Redis.
// All methods are safe for concurrent use.
type Gate struct {
	rdb   redis.UniversalClient
	ttl   time.Duration
	rules []classifierRule
}

// New returns a Gate backed by rdb with the given config.
func New(rdb redis.UniversalClient, cfg Config) *Gate {
	if cfg.ApproveTTL == 0 {
		cfg.ApproveTTL = time.Hour
	}
	return &Gate{rdb: rdb, ttl: cfg.ApproveTTL, rules: defaultRules()}
}

// Classify returns ClassSensitive and a reason when the script matches a
// sensitive pattern. Returns ClassBenign otherwise. The match is performed
// against a lower-cased copy of script to be case-insensitive.
//
// Rules may carry an optional exclude regex: if the primary pattern matches
// but the exclude also matches, the rule does not trigger. This replaces
// negative-lookahead patterns which are not supported in Go RE2.
func (g *Gate) Classify(script string) (Classification, string) {
	lower := strings.ToLower(script)
	for _, r := range g.rules {
		if !r.re.MatchString(lower) {
			continue
		}
		if r.exclude != nil && r.exclude.MatchString(lower) {
			continue // excluded — e.g. write that targets /workspace
		}
		return ClassSensitive, r.reason
	}
	return ClassBenign, ""
}

// Submit creates a pending approval record in Redis and adds it to the
// pending set. Returns the new record's ID.
func (g *Gate) Submit(ctx context.Context, sandboxID, script, reason string) (string, error) {
	id, err := newID()
	if err != nil {
		return "", fmt.Errorf("hitl: generate id: %w", err)
	}
	rec := ApprovalRecord{
		ID:          id,
		SandboxID:   sandboxID,
		Script:      script,
		Reason:      reason,
		State:       StatePending,
		SubmittedAt: time.Now().UTC(),
	}
	b, _ := json.Marshal(rec)

	pipe := g.rdb.Pipeline()
	pipe.Set(ctx, approvalKey(id), b, g.ttl)
	pipe.SAdd(ctx, pendingSetKey, id)
	pipe.Expire(ctx, pendingSetKey, g.ttl)
	if _, err := pipe.Exec(ctx); err != nil {
		return "", fmt.Errorf("hitl: submit: %w", err)
	}
	return id, nil
}

// Decide atomically transitions a pending approval to approved or rejected.
// First writer wins — concurrent calls return ErrAlreadyDecided. Returns
// ErrNotFound if the record does not exist (or has already expired).
func (g *Gate) Decide(ctx context.Context, id string, decision Decision, decidedBy string) error {
	raw, err := g.rdb.Get(ctx, approvalKey(id)).Bytes()
	if errors.Is(err, redis.Nil) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("hitl: decide get: %w", err)
	}

	var rec ApprovalRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return fmt.Errorf("hitl: unmarshal: %w", err)
	}
	if rec.State != StatePending {
		return ErrAlreadyDecided
	}

	now := time.Now().UTC()
	rec.DecidedBy = decidedBy
	rec.DecidedAt = &now
	switch decision {
	case DecisionApprove:
		rec.State = StateApproved
	case DecisionReject:
		rec.State = StateRejected
	default:
		return fmt.Errorf("hitl: unknown decision %q", decision)
	}

	b, _ := json.Marshal(rec)
	pipe := g.rdb.Pipeline()
	pipe.Set(ctx, approvalKey(id), b, g.ttl)
	pipe.SRem(ctx, pendingSetKey, id)
	// Immutable audit entry kept 1 year regardless of the approval TTL.
	pipe.Set(ctx, auditKey(id), b, 365*24*time.Hour)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("hitl: decide set: %w", err)
	}
	return nil
}

// Get returns the ApprovalRecord for id. Returns ErrNotFound if the record
// does not exist or has expired.
func (g *Gate) Get(ctx context.Context, id string) (ApprovalRecord, error) {
	raw, err := g.rdb.Get(ctx, approvalKey(id)).Bytes()
	if errors.Is(err, redis.Nil) {
		return ApprovalRecord{}, ErrNotFound
	}
	if err != nil {
		return ApprovalRecord{}, fmt.Errorf("hitl: get: %w", err)
	}
	var rec ApprovalRecord
	return rec, json.Unmarshal(raw, &rec)
}

// Pending returns all records currently in the pending state. It self-heals
// stale set entries whose backing record key has expired.
func (g *Gate) Pending(ctx context.Context) ([]ApprovalRecord, error) {
	ids, err := g.rdb.SMembers(ctx, pendingSetKey).Result()
	if err != nil {
		return nil, fmt.Errorf("hitl: list pending: %w", err)
	}
	var recs []ApprovalRecord
	for _, id := range ids {
		rec, err := g.Get(ctx, id)
		if errors.Is(err, ErrNotFound) {
			_ = g.rdb.SRem(ctx, pendingSetKey, id)
			continue
		}
		if err != nil {
			continue
		}
		if rec.State == StatePending {
			recs = append(recs, rec)
		}
	}
	return recs, nil
}

// WaitForDecision polls Redis until the approval for id is decided or ctx is
// cancelled. An expired record is treated as an implicit reject.
// The returned State is always StateApproved or StateRejected.
func (g *Gate) WaitForDecision(ctx context.Context, id string) (State, error) {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return StatePending, ctx.Err()
		case <-t.C:
			rec, err := g.Get(ctx, id)
			if errors.Is(err, ErrNotFound) {
				return StateRejected, nil // expired = implicit reject
			}
			if err != nil {
				continue
			}
			if rec.State != StatePending {
				return rec.State, nil
			}
		}
	}
}

// ── key helpers ────────────────────────────────────────────────────────────

const pendingSetKey = "hitl:pending"

func approvalKey(id string) string { return "hitl:approval:" + id }
func auditKey(id string) string    { return "hitl:audit:" + id }

// ── classifier ─────────────────────────────────────────────────────────────

type classifierRule struct {
	re     *regexp.Regexp
	// exclude is optional. If non-nil and also matches, the rule does not fire.
	// This replaces negative-lookahead patterns unsupported by Go RE2.
	exclude *regexp.Regexp
	reason  string
}

// ruleSpec is the raw data used to build classifierRule in defaultRules.
type ruleSpec struct {
	pattern string
	exclude string // empty = no exclusion
	reason  string
}

func defaultRules() []classifierRule {
	// All patterns are applied to the lower-cased script string.
	// Patterns use (primary, exclude, reason). If exclude matches alongside the
	// primary, the rule does not fire — replaces RE2-incompatible lookaheads.
	specs := []ruleSpec{
		// Shell redirections and tee: destination is the token right after >/tee.
		// Excluded when that token starts with /workspace.
		{`(>|tee)\s+/`, `(>|tee)\s+/workspace`, "write to system path"},
		// cp/mv: destination is the SECOND absolute path.
		// `cp <non-space> /` matches when the second arg starts with /.
		// Excluded when the second arg starts with /workspace.
		{`\b(cp|mv)\s+\S+\s+/`, `\b(cp|mv)\s+\S+\s+/workspace`, "write to system path"},
		// Package managers.
		{`\bapt(-get)?\s+(install|remove|purge)\b`, "", "package manager (apt)"},
		{`\bapk\s+(add|del)\b`, "", "package manager (apk)"},
		{`\bpip[23]?\s+install\b`, "", "package manager (pip)"},
		{`\bnpm\s+install\b`, "", "package manager (npm)"},
		{`\byum\s+(install|remove)\b`, "", "package manager (yum)"},
		{`\bdnf\s+(install|remove)\b`, "", "package manager (dnf)"},
		// Network tools.
		{`\b(curl|wget|nc|ncat|netcat|ssh|scp|rsync|ftp|sftp|telnet)\b`, "", "network tool"},
		// Destructive rm — excluded when targeting /workspace.
		{`\brm\s+-[a-z]*[rf][a-z]*\s+/`, `\brm\s+-[a-z]*[rf][a-z]*\s+/workspace`, "destructive rm on system path"},
		// Disk/format operations.
		{`\b(mkfs|fdisk|parted|wipefs)\b`, "", "disk operation"},
		{`\bdd\s+.*\bif=\b`, "", "dd disk operation"},
		// Privilege escalation.
		{`\b(sudo|doas)\b`, "", "privilege escalation"},
		{`\bsu\s+(-\s+)?[a-z]`, "", "switch user"},
		// Dangerous chmod on system paths — excluded when targeting /workspace.
		{`\bchmod\s+[0-9]+\s+/`, `\bchmod\s+[0-9]+\s+/workspace`, "broad chmod on system path"},
		// chown to root.
		{`\bchown\s+(root|0)\b`, "", "chown to root"},
		// Container/VM escape tools.
		{`\b(docker|podman|nerdctl|kubectl|k3s|ctr)\b`, "", "container tool"},
		// Compiling native code (could be used to bypass restrictions).
		{`\b(gcc|g\+\+|cc|clang|make)\s`, "", "native compilation"},
	}

	rules := make([]classifierRule, 0, len(specs))
	for _, s := range specs {
		r := classifierRule{
			re:     regexp.MustCompile(s.pattern),
			reason: s.reason,
		}
		if s.exclude != "" {
			r.exclude = regexp.MustCompile(s.exclude)
		}
		rules = append(rules, r)
	}
	return rules
}

func newID() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
