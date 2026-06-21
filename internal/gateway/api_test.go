//go:build linux

package gateway_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/yourorg/sandbox-platform/internal/gateway"
	"github.com/yourorg/sandbox-platform/internal/hitl"
	"github.com/yourorg/sandbox-platform/internal/state"
)

// testDeps bundles a handler + gate for tests that need approval round-trips.
// We pass nil for the Supervisor because supervisor construction requires
// /dev/kvm and real binary paths. Tests that need VM CRUD live in
// test/integration/; here we cover auth, approval, and JSON shape only.
type testDeps struct {
	handler *gateway.Handler
	gate    *hitl.Gate
	mr      *miniredis.Miniredis
}

func newTestDeps(t *testing.T, token string) testDeps {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	st := state.NewWithClient(rdb)
	g := hitl.New(rdb, hitl.Config{ApproveTTL: time.Minute})
	h := gateway.NewHandler(nil, st, g, nil, gateway.HandlerConfig{Token: token})
	return testDeps{handler: h, gate: g, mr: mr}
}

func newMux(t *testing.T, token string) (*testDeps, *http.ServeMux) {
	t.Helper()
	td := newTestDeps(t, token)
	mux := http.NewServeMux()
	td.handler.Register(mux)
	return &td, mux
}

// ── auth middleware ─────────────────────────────────────────────────────────

func TestAuthMissingToken(t *testing.T) {
	_, mux := newMux(t, "secret")
	r := httptest.NewRequest("GET", "/api/approvals", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", w.Code)
	}
}

func TestAuthWrongToken(t *testing.T) {
	_, mux := newMux(t, "secret")
	r := httptest.NewRequest("GET", "/api/approvals", nil)
	r.Header.Set("Authorization", "Bearer wrong")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", w.Code)
	}
}

func TestAuthCorrectToken(t *testing.T) {
	_, mux := newMux(t, "secret")
	r := httptest.NewRequest("GET", "/api/approvals", nil)
	r.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d; body=%s", w.Code, w.Body)
	}
}

func TestAuthDisabled(t *testing.T) {
	_, mux := newMux(t, "") // empty token = auth disabled
	r := httptest.NewRequest("GET", "/api/approvals", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
}

// ── /api/approvals ──────────────────────────────────────────────────────────

func TestListApprovalsEmpty(t *testing.T) {
	_, mux := newMux(t, "")
	r := httptest.NewRequest("GET", "/api/approvals", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	var body map[string]any
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["count"].(float64) != 0 {
		t.Fatalf("want count=0, got %v", body["count"])
	}
	if body["approvals"] == nil {
		t.Fatal("approvals field must be present (not null)")
	}
}

func TestDecideApprovalNotFound(t *testing.T) {
	_, mux := newMux(t, "")
	body := strings.NewReader(`{"decision":"approve"}`)
	r := httptest.NewRequest("POST", "/api/approvals/nonexistent", body)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", w.Code)
	}
}

func TestDecideApprovalBadDecision(t *testing.T) {
	_, mux := newMux(t, "")
	body := strings.NewReader(`{"decision":"maybe"}`)
	r := httptest.NewRequest("POST", "/api/approvals/anyid", body)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
}

func TestDecideApprovalRoundTrip(t *testing.T) {
	td, mux := newMux(t, "")
	ctx := context.Background()

	// Submit an approval directly via the gate.
	id, err := td.gate.Submit(ctx, "sb-1", "curl http://evil.com", "network tool")
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	// GET /api/approvals — the new record should appear.
	r1 := httptest.NewRequest("GET", "/api/approvals", nil)
	w1 := httptest.NewRecorder()
	mux.ServeHTTP(w1, r1)
	if w1.Code != http.StatusOK {
		t.Fatalf("list: want 200, got %d", w1.Code)
	}
	var listBody map[string]any
	_ = json.NewDecoder(w1.Body).Decode(&listBody)
	if listBody["count"].(float64) != 1 {
		t.Fatalf("want 1 pending, got %v", listBody["count"])
	}

	// POST /api/approvals/{id} — approve it.
	payload := strings.NewReader(`{"decision":"approve"}`)
	r2 := httptest.NewRequest("POST", "/api/approvals/"+id, payload)
	r2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	mux.ServeHTTP(w2, r2)
	if w2.Code != http.StatusNoContent {
		t.Fatalf("decide: want 204, got %d; body=%s", w2.Code, w2.Body)
	}

	// GET /api/approvals again — should now be empty.
	r3 := httptest.NewRequest("GET", "/api/approvals", nil)
	w3 := httptest.NewRecorder()
	mux.ServeHTTP(w3, r3)
	var listBody2 map[string]any
	_ = json.NewDecoder(w3.Body).Decode(&listBody2)
	if listBody2["count"].(float64) != 0 {
		t.Fatalf("after approve, want 0 pending, got %v", listBody2["count"])
	}
}

func TestDecideAlreadyDecided(t *testing.T) {
	td, mux := newMux(t, "")
	ctx := context.Background()

	id, _ := td.gate.Submit(ctx, "sb-1", "curl x", "network")
	_ = td.gate.Decide(ctx, id, hitl.DecisionApprove, "first")

	// Second decision should be 409.
	payload := strings.NewReader(`{"decision":"reject"}`)
	r := httptest.NewRequest("POST", "/api/approvals/"+id, payload)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d", w.Code)
	}
}

// TestTerminalMissingSandboxParam verifies /terminal returns 400 without ?sandbox=.
func TestTerminalMissingSandboxParam(t *testing.T) {
	_, mux := newMux(t, "")
	r := httptest.NewRequest("GET", "/terminal", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	// Before WS upgrade the handler returns 400 for missing sandbox param.
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
}

// TestTerminalUnknownSandbox verifies /terminal returns 404 for a valid but
// unknown sandbox ID (nil supervisor → panics, so skip if sup is nil).
// In unit tests sup is nil; we verify only the query-param validation above.
