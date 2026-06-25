package pg

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

// testStore connects to a Postgres test database described by PG_TEST_* env vars
// and runs migrations. It skips (not fails) when PG_TEST_HOST is unset, so the
// default `go test ./...` run stays green without a database. CI provides a
// throwaway Postgres service and sets these vars.
//
//	PG_TEST_HOST (required to run)  PG_TEST_PORT (5432)  PG_TEST_USER (postgres)
//	PG_TEST_PASSWORD               PG_TEST_DB (sandbox)  PG_TEST_SSLMODE (disable)
func testStore(t *testing.T) *Store {
	t.Helper()
	host := os.Getenv("PG_TEST_HOST")
	if host == "" {
		t.Skip("PG_TEST_HOST not set — skipping Postgres integration tests")
	}
	cfg := Config{
		Host:     host,
		Port:     envOr("PG_TEST_PORT", "5432"),
		User:     envOr("PG_TEST_USER", "postgres"),
		Password: os.Getenv("PG_TEST_PASSWORD"),
		DB:       envOr("PG_TEST_DB", "sandbox"),
		SSLMode:  envOr("PG_TEST_SSLMODE", "disable"),
	}
	st, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("pg connect: %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return st
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func uniq(prefix string) string {
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

func TestUserRepo_CRUD(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	username := uniq("u-")

	u, err := st.Users.Create(ctx, username, "hash-xyz", RoleOperator)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _, _ = st.Pool().Exec(ctx, `DELETE FROM users WHERE id=$1`, u.ID) })

	if u.ID == "" || u.Username != username || u.Role != RoleOperator || u.Disabled {
		t.Fatalf("unexpected created user: %+v", u)
	}

	byName, err := st.Users.ByUsername(ctx, username)
	if err != nil || byName.ID != u.ID {
		t.Fatalf("ByUsername: %v (%+v)", err, byName)
	}
	byID, err := st.Users.ByID(ctx, u.ID)
	if err != nil || byID.Username != username {
		t.Fatalf("ByID: %v (%+v)", err, byID)
	}

	if _, err := st.Users.ByUsername(ctx, uniq("nope-")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ByUsername(missing): want ErrNotFound, got %v", err)
	}

	n, err := st.Users.Count(ctx)
	if err != nil || n < 1 {
		t.Fatalf("Count: %v n=%d", err, n)
	}

	if err := st.Users.SetDisabled(ctx, u.ID, true); err != nil {
		t.Fatalf("SetDisabled: %v", err)
	}
	got, _ := st.Users.ByID(ctx, u.ID)
	if !got.Disabled {
		t.Fatal("user should be disabled")
	}
}

func TestSessionRepo_Lifecycle(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	u, err := st.Users.Create(ctx, uniq("s-"), "h", RoleOperator)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = st.Pool().Exec(ctx, `DELETE FROM sessions WHERE user_id=$1`, u.ID)
		_, _ = st.Pool().Exec(ctx, `DELETE FROM users WHERE id=$1`, u.ID)
	})

	// A valid session resolves to its user.
	sid := uuid.NewString()
	if err := st.Sessions.Create(ctx, sid, u.ID, time.Now().Add(time.Hour), "go-test", "127.0.0.1"); err != nil {
		t.Fatalf("create session: %v", err)
	}
	resolved, err := st.Sessions.Resolve(ctx, sid)
	if err != nil || resolved.ID != u.ID {
		t.Fatalf("resolve: %v (%+v)", err, resolved)
	}

	// Revoked sessions no longer resolve.
	if err := st.Sessions.Revoke(ctx, sid); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := st.Sessions.Resolve(ctx, sid); !errors.Is(err, ErrNotFound) {
		t.Fatalf("resolve after revoke: want ErrNotFound, got %v", err)
	}

	// An expired session does not resolve and is reaped by DeleteExpired.
	expSid := uuid.NewString()
	if err := st.Sessions.Create(ctx, expSid, u.ID, time.Now().Add(-time.Minute), "go-test", ""); err != nil {
		t.Fatalf("create expired session: %v", err)
	}
	if _, err := st.Sessions.Resolve(ctx, expSid); !errors.Is(err, ErrNotFound) {
		t.Fatalf("resolve expired: want ErrNotFound, got %v", err)
	}
	if n, err := st.Sessions.DeleteExpired(ctx); err != nil || n < 1 {
		t.Fatalf("DeleteExpired: n=%d err=%v", n, err)
	}
}

func TestSandboxRepo_HistoryLifecycle(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	owner, err := st.Users.Create(ctx, uniq("o-"), "h", RoleOperator)
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	sbID := uniq("sb-")
	t.Cleanup(func() {
		_, _ = st.Pool().Exec(ctx, `DELETE FROM sandboxes WHERE id=$1`, sbID)
		_, _ = st.Pool().Exec(ctx, `DELETE FROM users WHERE id=$1`, owner.ID)
	})

	if err := st.Sandboxes.Upsert(ctx, Sandbox{
		ID: sbID, OwnerID: owner.ID, Name: "first", Status: SandboxRunning,
		VCPUs: 1, MemMiB: 256, CPUPercent: 100, PidsMax: 128, CID: 7,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, err := st.Sandboxes.ByID(ctx, sbID)
	if err != nil || got.OwnerID != owner.ID || got.Status != SandboxRunning {
		t.Fatalf("ByID: %v (%+v)", err, got)
	}

	list, err := st.Sandboxes.ListByOwner(ctx, owner.ID)
	if err != nil || len(list) != 1 || list[0].ID != sbID {
		t.Fatalf("ListByOwner: %v (%d rows)", err, len(list))
	}

	if err := st.Sandboxes.Rename(ctx, sbID, "renamed"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := st.Sandboxes.MarkStopped(ctx, sbID); err != nil {
		t.Fatalf("mark stopped: %v", err)
	}
	got, _ = st.Sandboxes.ByID(ctx, sbID)
	if got.Name != "renamed" || got.Status != SandboxStopped {
		t.Fatalf("after rename+stop: %+v", got)
	}

	if err := st.Sandboxes.Archive(ctx, sbID); err != nil {
		t.Fatalf("archive: %v", err)
	}
	got, _ = st.Sandboxes.ByID(ctx, sbID)
	if got.Status != SandboxArchived {
		t.Fatalf("after archive: status=%s", got.Status)
	}

	// An archived sandbox no longer appears in the owner's active history.
	list, _ = st.Sandboxes.ListByOwner(ctx, owner.ID)
	for _, s := range list {
		if s.ID == sbID {
			t.Fatal("archived sandbox should not appear in ListByOwner")
		}
	}
}
