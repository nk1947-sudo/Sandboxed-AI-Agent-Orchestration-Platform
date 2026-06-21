//go:build linux

package gateway_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yourorg/sandbox-platform/internal/gateway"
	"github.com/yourorg/sandbox-platform/internal/protocol"
)

// fakeFC simulates Firecracker's vsock Unix socket multiplexer for unit tests.
// It runs a lightweight "CONNECT <port>" / "OK <port>" handshake then passes
// the raw connection to a per-test handler function.
type fakeFC struct {
	ln       net.Listener
	sockPath string
}

func newFakeFC(t *testing.T) *fakeFC {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "vsock.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("newFakeFC: listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	return &fakeFC{ln: ln, sockPath: sock}
}

// serveOne accepts a single connection in a goroutine, performs the handshake,
// then calls fn with the connection positioned past the "OK" line.
func (f *fakeFC) serveOne(t *testing.T, fn func(net.Conn)) {
	t.Helper()
	go func() {
		conn, err := f.ln.Accept()
		if err != nil {
			// Listener closed during cleanup — normal teardown path.
			return
		}
		defer conn.Close()

		br := bufio.NewReader(conn)
		line, err := br.ReadString('\n')
		if err != nil {
			t.Errorf("fakeFC: read CONNECT line: %v", err)
			return
		}
		if !strings.HasPrefix(strings.TrimRight(line, "\r\n"), "CONNECT ") {
			t.Errorf("fakeFC: unexpected handshake line: %q", line)
			return
		}
		fmt.Fprintf(conn, "OK 1234\n")
		// At this point br may have buffered bytes from client read-ahead, but
		// since the client has not yet sent the ExecutionRequest (it was waiting
		// for the OK), br is empty — so passing the raw conn to fn is safe.
		fn(conn)
	}()
}

// noRetry returns a Client that tries exactly once (no retries) with the given
// UDS path. Suitable for all tests except the retry test.
func noRetry(sockPath string) *gateway.Client {
	return gateway.New(sockPath, gateway.Config{
		RetryMax:    0,
		DialTimeout: 2 * time.Second,
	})
}

// ── tests ──────────────────────────────────────────────────────────────────

func TestExecOutputAndExit(t *testing.T) {
	fc := newFakeFC(t)
	fc.serveOne(t, func(conn net.Conn) {
		var req protocol.ExecutionRequest
		if err := protocol.ReadMessage(conn, &req); err != nil {
			t.Errorf("read req: %v", err)
			return
		}
		_ = protocol.WriteMessage(conn, protocol.ResponseFrame{
			ID:   req.ID,
			Kind: protocol.FrameOutput,
			Data: "hello\n",
		})
		_ = protocol.WriteMessage(conn, protocol.ResponseFrame{
			ID:       req.ID,
			Kind:     protocol.FrameExit,
			ExitCode: 0,
		})
	})

	var output []string
	err := noRetry(fc.sockPath).Exec(
		context.Background(),
		protocol.ExecutionRequest{ID: "t1", Kind: protocol.KindExec, Script: "echo hello"},
		func(f protocol.ResponseFrame) {
			if f.Kind == protocol.FrameOutput {
				output = append(output, f.Data)
			}
		},
	)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if len(output) != 1 || output[0] != "hello\n" {
		t.Fatalf("got output %v, want [\"hello\\n\"]", output)
	}
}

func TestMultipleOutputFramesThenNonzeroExit(t *testing.T) {
	fc := newFakeFC(t)
	fc.serveOne(t, func(conn net.Conn) {
		var req protocol.ExecutionRequest
		_ = protocol.ReadMessage(conn, &req)
		for i := 0; i < 5; i++ {
			_ = protocol.WriteMessage(conn, protocol.ResponseFrame{
				ID:   req.ID,
				Kind: protocol.FrameOutput,
				Data: fmt.Sprintf("line%d\n", i),
			})
		}
		_ = protocol.WriteMessage(conn, protocol.ResponseFrame{
			ID:       req.ID,
			Kind:     protocol.FrameExit,
			ExitCode: 42,
		})
	})

	var lines []string
	var exitCode int
	err := noRetry(fc.sockPath).Exec(
		context.Background(),
		protocol.ExecutionRequest{ID: "t2", Kind: protocol.KindExec},
		func(f protocol.ResponseFrame) {
			switch f.Kind {
			case protocol.FrameOutput:
				lines = append(lines, f.Data)
			case protocol.FrameExit:
				exitCode = f.ExitCode
			}
		},
	)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if len(lines) != 5 {
		t.Fatalf("want 5 output frames, got %d", len(lines))
	}
	if exitCode != 42 {
		t.Fatalf("want exit 42, got %d", exitCode)
	}
}

func TestPingPong(t *testing.T) {
	fc := newFakeFC(t)
	fc.serveOne(t, func(conn net.Conn) {
		var req protocol.ExecutionRequest
		if err := protocol.ReadMessage(conn, &req); err != nil {
			t.Errorf("read req: %v", err)
			return
		}
		if req.Kind != protocol.KindPing {
			t.Errorf("want KindPing, got %q", req.Kind)
		}
		_ = protocol.WriteMessage(conn, protocol.ResponseFrame{
			ID:   req.ID,
			Kind: protocol.FramePong,
		})
	})

	if err := noRetry(fc.sockPath).Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

func TestHandshakeRejected(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "bad.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 64)
		_, _ = conn.Read(buf) // consume the CONNECT line
		fmt.Fprintf(conn, "ERR port not available\n")
	}()

	err = noRetry(sock).Ping(context.Background())
	if !errors.Is(err, gateway.ErrHandshakeFailed) {
		t.Fatalf("want ErrHandshakeFailed in chain, got: %v", err)
	}
}

func TestGuestError(t *testing.T) {
	fc := newFakeFC(t)
	fc.serveOne(t, func(conn net.Conn) {
		var req protocol.ExecutionRequest
		_ = protocol.ReadMessage(conn, &req)
		_ = protocol.WriteMessage(conn, protocol.ResponseFrame{
			ID:    req.ID,
			Kind:  protocol.FrameError,
			Error: "execution timed out",
		})
	})

	err := noRetry(fc.sockPath).Exec(
		context.Background(),
		protocol.ExecutionRequest{ID: "t3", Kind: protocol.KindExec},
		func(protocol.ResponseFrame) {},
	)
	if err == nil || !strings.Contains(err.Error(), "execution timed out") {
		t.Fatalf("want timed-out error, got %v", err)
	}
}

func TestContextCancellation(t *testing.T) {
	fc := newFakeFC(t)
	fc.serveOne(t, func(conn net.Conn) {
		var req protocol.ExecutionRequest
		_ = protocol.ReadMessage(conn, &req)
		// Deliberately block — never sends frames back.
		time.Sleep(10 * time.Second)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := noRetry(fc.sockPath).Exec(ctx,
		protocol.ExecutionRequest{ID: "t4"},
		func(protocol.ResponseFrame) {},
	)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded, got %v", err)
	}
}

func TestOversizeResponseRejected(t *testing.T) {
	fc := newFakeFC(t)
	fc.serveOne(t, func(conn net.Conn) {
		var req protocol.ExecutionRequest
		_ = protocol.ReadMessage(conn, &req)
		// Send a 4-byte header claiming 4 GiB payload — far over MaxFrameSize.
		_, _ = conn.Write([]byte{0xFF, 0xFF, 0xFF, 0xFF})
	})

	err := noRetry(fc.sockPath).Exec(
		context.Background(),
		protocol.ExecutionRequest{ID: "t5"},
		func(protocol.ResponseFrame) {},
	)
	if !errors.Is(err, protocol.ErrFrameTooLarge) {
		t.Fatalf("want ErrFrameTooLarge, got %v", err)
	}
}

func TestConnectionClosedBeforeTerminal(t *testing.T) {
	fc := newFakeFC(t)
	fc.serveOne(t, func(conn net.Conn) {
		var req protocol.ExecutionRequest
		_ = protocol.ReadMessage(conn, &req)
		// Send one output frame then close without a terminal frame.
		_ = protocol.WriteMessage(conn, protocol.ResponseFrame{
			ID:   req.ID,
			Kind: protocol.FrameOutput,
			Data: "partial\n",
		})
		// conn.Close() called by defer in serveOne.
	})

	err := noRetry(fc.sockPath).Exec(
		context.Background(),
		protocol.ExecutionRequest{ID: "t6"},
		func(protocol.ResponseFrame) {},
	)
	if err == nil || !strings.Contains(err.Error(), "before terminal frame") {
		t.Fatalf("want 'before terminal frame' error, got %v", err)
	}
}

func TestRetryUntilSocketAppears(t *testing.T) {
	tmpDir := t.TempDir()
	sockPath := filepath.Join(tmpDir, "late.sock")

	// Start a server that only creates its socket after a short delay,
	// simulating a guest agent that's still booting.
	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(150 * time.Millisecond)

		ln, err := net.Listen("unix", sockPath)
		if err != nil {
			t.Logf("late listen: %v", err)
			return
		}
		defer ln.Close()

		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		br := bufio.NewReader(conn)
		_, _ = br.ReadString('\n')
		fmt.Fprintf(conn, "OK 1234\n")

		var req protocol.ExecutionRequest
		_ = protocol.ReadMessage(conn, &req)
		_ = protocol.WriteMessage(conn, protocol.ResponseFrame{
			ID:   req.ID,
			Kind: protocol.FrameExit,
		})
	}()

	client := gateway.New(sockPath, gateway.Config{
		RetryMax:    20,
		RetryBase:   20 * time.Millisecond,
		RetryCap:    100 * time.Millisecond,
		DialTimeout: 200 * time.Millisecond,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Exec(ctx, protocol.ExecutionRequest{ID: "retry"}, func(protocol.ResponseFrame) {}); err != nil {
		t.Fatalf("Exec after retry: %v", err)
	}
	<-done
}
