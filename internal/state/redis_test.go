package state

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/yourorg/sandbox-platform/internal/sandbox"
)

func newTestStore(t *testing.T) (*Store, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return NewWithClient(rdb), mr
}

func TestRecordRoundTrip(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	r := sandbox.Record{
		ID: "sb-1", CID: 3, PID: 1234,
		Status:    sandbox.StatusRunning,
		CreatedAt: time.Now().UTC().Truncate(time.Second),
		Spec:      sandbox.Spec{VCPUs: 1, MemMiB: 256, CPUPercent: 100, PidsMax: 128},
	}
	if err := st.Save(ctx, r); err != nil {
		t.Fatalf("save: %v", err)
	}

	all, err := st.All(ctx)
	if err != nil {
		t.Fatalf("all: %v", err)
	}
	if len(all) != 1 || all[0].ID != "sb-1" || all[0].CID != 3 || all[0].Spec.MemMiB != 256 {
		t.Fatalf("unexpected records: %+v", all)
	}

	if err := st.Remove(ctx, "sb-1"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if all, _ := st.All(ctx); len(all) != 0 {
		t.Fatalf("want empty after remove, got %+v", all)
	}
}

func TestAllSelfHealsStaleSetEntry(t *testing.T) {
	st, mr := newTestStore(t)
	ctx := context.Background()

	// Active-set entry with no backing record (e.g. record key expired).
	mr.SAdd(keyActive, "ghost")
	all, err := st.All(ctx)
	if err != nil {
		t.Fatalf("all: %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("want 0 records, got %+v", all)
	}
	if mr.Exists(keyActive) {
		t.Fatalf("stale set entry should have been cleaned up")
	}
}

func TestHeartbeatExpiry(t *testing.T) {
	st, mr := newTestStore(t)
	ctx := context.Background()

	if err := st.Heartbeat(ctx, "sb-1", 10*time.Second); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if ok, _ := st.Alive(ctx, "sb-1"); !ok {
		t.Fatal("want alive immediately after heartbeat")
	}
	mr.FastForward(11 * time.Second)
	if ok, _ := st.Alive(ctx, "sb-1"); ok {
		t.Fatal("want expired after TTL")
	}
}

func TestSessionMapping(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	if err := st.BindSession(ctx, "sess-1", "sb-1", time.Minute); err != nil {
		t.Fatalf("bind: %v", err)
	}
	got, err := st.SessionSandbox(ctx, "sess-1")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got != "sb-1" {
		t.Fatalf("got %q, want sb-1", got)
	}
	if _, err := st.SessionSandbox(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestTokenBucket(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	// Fixed clock so refill is deterministic.
	cur := time.Unix(1000, 0)
	st.now = func() time.Time { return cur }

	// capacity 3, refill 1 token/sec, cost 1 per call.
	for i := 0; i < 3; i++ {
		ok, err := st.Allow(ctx, "tenant-a", 3, 1, 1)
		if err != nil {
			t.Fatalf("allow %d: %v", i, err)
		}
		if !ok {
			t.Fatalf("call %d should be allowed (burst)", i)
		}
	}
	if ok, _ := st.Allow(ctx, "tenant-a", 3, 1, 1); ok {
		t.Fatal("4th call should be denied (bucket empty)")
	}

	// Advance 2s -> 2 tokens refilled.
	cur = cur.Add(2 * time.Second)
	for i := 0; i < 2; i++ {
		if ok, _ := st.Allow(ctx, "tenant-a", 3, 1, 1); !ok {
			t.Fatalf("post-refill call %d should be allowed", i)
		}
	}
	if ok, _ := st.Allow(ctx, "tenant-a", 3, 1, 1); ok {
		t.Fatal("should be denied again after draining refilled tokens")
	}

	// Independent bucket key is unaffected.
	if ok, _ := st.Allow(ctx, "tenant-b", 3, 1, 1); !ok {
		t.Fatal("separate tenant should have a full bucket")
	}
}

func TestAllowRejectsBadRefill(t *testing.T) {
	st, _ := newTestStore(t)
	if _, err := st.Allow(context.Background(), "x", 1, 0, 1); err == nil {
		t.Fatal("want error for refillPerSec <= 0")
	}
}
