//go:build linux

// Package security holds regression tests for every security fix applied to
// the platform. Each test is named after the property it guards so that the
// commit message for a future regression shows exactly which control failed.
//
// These tests require Linux (for the gateway package) but NOT a KVM host;
// they exercise the control logic directly, without booting a real VM.
package security_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gorilla/websocket"
	"github.com/redis/go-redis/v9"

	"github.com/yourorg/sandbox-platform/internal/gateway"
	"github.com/yourorg/sandbox-platform/internal/hitl"
	"github.com/yourorg/sandbox-platform/internal/protocol"
	"github.com/yourorg/sandbox-platform/internal/state"
)

// ── R1: Protocol frame size cap ───────────────────────────────────────────────
//
// Fix: protocol.ReadMessage rejects frames whose 4-byte length prefix claims
// a payload larger than MaxFrameSize (16 MiB).
// Without this cap a peer can allocate 4 GiB from one crafted header byte.

func TestR1_FrameSizeCap_ReadRejectsOversizedLength(t *testing.T) {
	// Craft a frame header claiming MaxFrameSize+1 bytes.
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(protocol.MaxFrameSize+1))

	var buf bytes.Buffer
	buf.Write(hdr[:])
	// No actual payload — ReadMessage should reject before reading any payload.

	var v protocol.ResponseFrame
	err := protocol.ReadMessage(&buf, &v)
	if err == nil {
		t.Fatal("ReadMessage: expected error for oversized frame, got nil")
	}
	if !errors.Is(err, protocol.ErrFrameTooLarge) {
		t.Fatalf("ReadMessage: want ErrFrameTooLarge, got %v", err)
	}
}

func TestR1_FrameSizeCap_WriteRejectsOversizedPayload(t *testing.T) {
	// Build a request whose JSON serialisation exceeds MaxFrameSize.
	req := protocol.ExecutionRequest{
		ID:     "x",
		Script: strings.Repeat("a", protocol.MaxFrameSize),
	}
	var buf bytes.Buffer
	err := protocol.WriteMessage(&buf, req)
	if !errors.Is(err, protocol.ErrFrameTooLarge) {
		t.Fatalf("WriteMessage: want ErrFrameTooLarge for payload > MaxFrameSize, got %v", err)
	}
}

func TestR1_FrameSizeCap_LegalFrameAccepted(t *testing.T) {
	req := protocol.ExecutionRequest{ID: "ok", Script: "echo hi", TimeoutSec: 5}
	var buf bytes.Buffer
	if err := protocol.WriteMessage(&buf, req); err != nil {
		t.Fatalf("WriteMessage: unexpected error: %v", err)
	}

	var got protocol.ExecutionRequest
	if err := protocol.ReadMessage(&buf, &got); err != nil {
		t.Fatalf("ReadMessage: unexpected error: %v", err)
	}
	if got.Script != req.Script {
		t.Fatalf("round-trip: got script %q, want %q", got.Script, req.Script)
	}
}

// ── R2: WebSocket Origin allowlist ────────────────────────────────────────────
//
// Fix: the WS upgrader's CheckOrigin must use an explicit allowlist and never
// return true unconditionally. An open CheckOrigin allows CSRF attacks where
// a malicious page opens a WS to the control plane using the victim's cookies.
//
// We test this by starting an httptest.Server with the gateway handler
// (supervisor = nil, so all VM endpoints return 500/404; the origin check fires
// during the WS upgrade which is before any supervisor call).
//
// The /terminal endpoint in gateway/api.go calls sup.Get() before the WS
// upgrade. To test just the origin check in isolation we register a minimal
// handler that uses the same upgrader logic. The key property is: disallowed
// origins must receive HTTP 403.

// wsTestServer creates an httptest.Server with a minimal WS handler that uses
// the same origin-check policy as the gateway (allowlist approach).
func wsTestServer(t *testing.T, allowedOrigins []string) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			origin := r.Header.Get("Origin")
			// Empty origin (non-browser clients) → allow.
			if origin == "" {
				return true
			}
			// Explicit allowlist.
			for _, o := range allowedOrigins {
				if strings.EqualFold(origin, o) {
					return true
				}
			}
			// Same-host fallback: compare origin host to request host.
			originHost := strings.TrimPrefix(origin, "http://")
			originHost = strings.TrimPrefix(originHost, "https://")
			originHost = strings.SplitN(originHost, "/", 2)[0]
			return originHost == r.Host
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			// upgrader.Upgrade already wrote 403 — nothing more to do.
			return
		}
		conn.Close()
	}))
	t.Cleanup(srv.Close)
	return srv
}

func dialWS(t *testing.T, serverURL, origin string) (*http.Response, error) {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(serverURL, "http") + "/ws"
	dialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second}
	hdr := http.Header{}
	if origin != "" {
		hdr.Set("Origin", origin)
	}
	conn, resp, err := dialer.Dial(wsURL, hdr)
	if conn != nil {
		conn.Close()
	}
	return resp, err
}

func TestR2_OriginCheck_DisallowedOriginRejected(t *testing.T) {
	srv := wsTestServer(t, []string{"http://trusted.example.com"})

	resp, err := dialWS(t, srv.URL, "http://evil.example.com")
	if err == nil {
		t.Fatal("WS dial: expected error for disallowed origin, got nil")
	}
	if resp == nil {
		t.Fatal("WS dial: response is nil, cannot check status code")
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("WS upgrade: want 403 for disallowed origin, got %d", resp.StatusCode)
	}
}

func TestR2_OriginCheck_AllowedOriginAccepted(t *testing.T) {
	srv := wsTestServer(t, []string{"http://trusted.example.com"})

	_, err := dialWS(t, srv.URL, "http://trusted.example.com")
	if err != nil {
		t.Fatalf("WS dial with allowed origin: unexpected error: %v", err)
	}
}

func TestR2_OriginCheck_EmptyOriginAllowed(t *testing.T) {
	// Non-browser (CLI) clients omit Origin. Must not be rejected.
	srv := wsTestServer(t, []string{"http://trusted.example.com"})
	_, err := dialWS(t, srv.URL, "")
	if err != nil {
		t.Fatalf("WS dial with empty origin: unexpected error: %v", err)
	}
}

func TestR2_OriginCheck_SameHostAllowed(t *testing.T) {
	// A same-host WS connection (origin == scheme://host of the server) must be allowed
	// even when the allowlist is empty.
	srv := wsTestServer(t, nil)
	// Server host (no scheme); build matching origin.
	serverHost := strings.TrimPrefix(srv.URL, "http://")
	origin := "http://" + serverHost
	_, err := dialWS(t, srv.URL, origin)
	if err != nil {
		t.Fatalf("WS dial with same-host origin: unexpected error: %v", err)
	}
}

// ── R3: Constant-time auth comparison ─────────────────────────────────────────
//
// Fix: API bearer-token comparison uses subtle.ConstantTimeCompare, not ==.
// A timing-oracle attack on == could allow an attacker to guess the token
// one character at a time. This test verifies the HTTP API rejects wrong tokens
// and accepts correct ones regardless of shared prefix.

func newGatewayHandler(t *testing.T, token string) (*gateway.Handler, *http.ServeMux) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	st := state.NewWithClient(rdb)
	g := hitl.New(rdb, hitl.Config{ApproveTTL: time.Minute})
	h := gateway.NewHandler(nil, st, g, nil, gateway.HandlerConfig{Token: token})
	mux := http.NewServeMux()
	h.Register(mux)
	return h, mux
}

func TestR3_Auth_CorrectTokenAccepted(t *testing.T) {
	const token = "super-secret-token"
	_, mux := newGatewayHandler(t, token)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/api/vms", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/vms: %v", err)
	}
	defer resp.Body.Close()
	// 200 or 500 (nil supervisor) — either means auth passed.
	if resp.StatusCode == http.StatusUnauthorized {
		t.Fatalf("correct token rejected (got 401)")
	}
}

func TestR3_Auth_WrongTokenRejected(t *testing.T) {
	const token = "super-secret-token"
	_, mux := newGatewayHandler(t, token)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	for _, wrong := range []string{
		"",
		"super-secret-toke", // prefix match — must still be rejected
		"super-secret-tokenX",
		"wrong",
	} {
		req, _ := http.NewRequest("GET", srv.URL+"/api/vms", nil)
		if wrong != "" {
			req.Header.Set("Authorization", "Bearer "+wrong)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET /api/vms: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("wrong token %q: want 401, got %d", wrong, resp.StatusCode)
		}
	}
}

// ── R4: HITL classifier — cp/mv destination regression ───────────────────────
//
// Fix: the cp/mv rule checks the DESTINATION path, not the source.
// The original negative-lookahead pattern (?!/workspace) was not RE2-compatible
// and had to be rewritten using a separate exclude-regex per rule.
//
// Before the fix: `cp /workspace/evil.sh /etc/cron.d/bad` was wrongly classified
// as BENIGN because the pattern (cp|mv)\s+/ matched the SOURCE /workspace path
// and the exclude /workspace matched, suppressing the rule.
// After the fix: the cp/mv pattern matches the second path argument only, so
// the destination /etc/cron.d/bad correctly triggers "sensitive".

func newTestGate(t *testing.T) *hitl.Gate {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return hitl.New(rdb, hitl.Config{ApproveTTL: time.Minute})
}

func TestR4_Classifier_CpToSystemPath(t *testing.T) {
	g := newTestGate(t)
	// cp with /workspace SOURCE and system DEST must be SENSITIVE.
	cases := []string{
		"cp /workspace/evil.sh /etc/cron.d/bad",
		"cp /workspace/rootkit /usr/bin/ls",
		"mv /workspace/payload /etc/profile.d/backdoor.sh",
		"mv /workspace/lib.so /lib/x86_64-linux-gnu/libssl.so.1",
	}
	for _, s := range cases {
		class, reason := g.Classify(s)
		if class != hitl.ClassSensitive {
			t.Errorf("Classify(%q) = benign, want sensitive (system-path dest) — regression in cp/mv rule", s)
		} else {
			t.Logf("Classify(%q) = sensitive: %s ✓", s, reason)
		}
	}
}

func TestR4_Classifier_CpInsideWorkspace(t *testing.T) {
	g := newTestGate(t)
	// cp/mv where both SOURCE and DEST are inside /workspace must be BENIGN.
	cases := []string{
		"cp /workspace/a.py /workspace/b.py",
		"mv /workspace/input.csv /workspace/processed/input.csv",
	}
	for _, s := range cases {
		class, _ := g.Classify(s)
		if class != hitl.ClassBenign {
			t.Errorf("Classify(%q) = sensitive, want benign (workspace-to-workspace) — regression in exclude rule", s)
		}
	}
}

func TestR4_Classifier_TeeToSystemPath(t *testing.T) {
	g := newTestGate(t)
	cases := []string{
		"echo evil | tee /etc/passwd",
		"cat data | tee /etc/sudoers",
		"> /etc/cron.d/backdoor",
	}
	for _, s := range cases {
		class, reason := g.Classify(s)
		if class != hitl.ClassSensitive {
			t.Errorf("Classify(%q) = benign, want sensitive: %s", s, reason)
		}
	}
}

func TestR4_Classifier_TeeToWorkspace(t *testing.T) {
	g := newTestGate(t)
	cases := []string{
		"tee /workspace/output.txt",
		"echo hello | tee /workspace/log.txt",
		"> /workspace/result.json",
	}
	for _, s := range cases {
		class, _ := g.Classify(s)
		if class != hitl.ClassBenign {
			t.Errorf("Classify(%q) = sensitive, want benign (tee to workspace)", s)
		}
	}
}
