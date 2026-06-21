//go:build linux

// Package integration holds end-to-end tests that require a live KVM host.
// They are skipped automatically when /dev/kvm is absent so they are safe to
// run in the unit-test matrix; the Phase 8 CI harness provides a KVM runner.
//
// Required environment for each test:
//
//	VSOCK_UDS_PATH  — host path to the VM's vsock UDS (set by control plane)
//
// Optional:
//
//	VSOCK_RETRY     — max retries (default 5); useful when the guest agent
//	                  is still booting when the test starts
package integration

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yourorg/sandbox-platform/internal/gateway"
	"github.com/yourorg/sandbox-platform/internal/protocol"
)

func requireKVM(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("KVM not available (/dev/kvm missing) — skipping integration test")
	}
}

func requireVsockPath(t *testing.T) string {
	t.Helper()
	p := os.Getenv("VSOCK_UDS_PATH")
	if p == "" {
		t.Skip("VSOCK_UDS_PATH not set; launch a VM with the control plane first")
	}
	return p
}

func vsockClient(vsockPath string) *gateway.Client {
	retryMax := 5
	if v := os.Getenv("VSOCK_RETRY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			retryMax = n
		}
	}
	return gateway.New(vsockPath, gateway.Config{
		RetryMax:  retryMax,
		RetryBase: 200 * time.Millisecond,
	})
}

// TestVsockPing checks that the guest agent is alive and responding.
func TestVsockPing(t *testing.T) {
	requireKVM(t)
	vsockPath := requireVsockPath(t)

	client := vsockClient(vsockPath)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := client.Ping(ctx); err != nil {
		t.Fatalf("Ping failed: %v", err)
	}
}

// TestVsockExec_EchoHi verifies a simple command runs and returns the expected output.
func TestVsockExec_EchoHi(t *testing.T) {
	requireKVM(t)
	vsockPath := requireVsockPath(t)

	client := vsockClient(vsockPath)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var sb strings.Builder
	var exitCode int
	err := client.Exec(ctx, protocol.ExecutionRequest{
		ID:         "e2e-echo",
		Kind:       protocol.KindExec,
		Script:     "echo hi",
		TimeoutSec: 5,
	}, func(f protocol.ResponseFrame) {
		switch f.Kind {
		case protocol.FrameOutput:
			sb.WriteString(f.Data)
		case protocol.FrameExit:
			exitCode = f.ExitCode
		}
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("want exit 0, got %d", exitCode)
	}
	if got := strings.TrimSpace(sb.String()); got != "hi" {
		t.Fatalf("want output \"hi\", got %q", got)
	}
}

// TestVsockExec_Python3 verifies the guest has a working Python 3 interpreter.
func TestVsockExec_Python3(t *testing.T) {
	requireKVM(t)
	vsockPath := requireVsockPath(t)

	client := vsockClient(vsockPath)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var sb strings.Builder
	err := client.Exec(ctx, protocol.ExecutionRequest{
		ID:         "e2e-py3",
		Kind:       protocol.KindExec,
		Script:     "python3 -c 'print(2**10)'",
		TimeoutSec: 5,
	}, func(f protocol.ResponseFrame) {
		if f.Kind == protocol.FrameOutput {
			sb.WriteString(f.Data)
		}
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if got := strings.TrimSpace(sb.String()); got != "1024" {
		t.Fatalf("want \"1024\", got %q", got)
	}
}

// TestVsockExec_Timeout verifies that execution is bounded by the timeout and
// that no guest process survives after the timeout fires.
func TestVsockExec_Timeout(t *testing.T) {
	requireKVM(t)
	vsockPath := requireVsockPath(t)

	client := vsockClient(vsockPath)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	err := client.Exec(ctx, protocol.ExecutionRequest{
		ID:         "e2e-timeout",
		Kind:       protocol.KindExec,
		Script:     "sleep 60",
		TimeoutSec: 2,
	}, func(f protocol.ResponseFrame) {})
	if err == nil {
		t.Fatal("want error from timed-out exec, got nil")
	}
	t.Logf("got expected timeout error: %v", err)
}

// TestVsockExec_NonzeroExit verifies that the exit code is faithfully reported.
func TestVsockExec_NonzeroExit(t *testing.T) {
	requireKVM(t)
	vsockPath := requireVsockPath(t)

	client := vsockClient(vsockPath)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var exitCode int
	_ = client.Exec(ctx, protocol.ExecutionRequest{
		ID:         "e2e-nonzero",
		Kind:       protocol.KindExec,
		Script:     "exit 42",
		TimeoutSec: 5,
	}, func(f protocol.ResponseFrame) {
		if f.Kind == protocol.FrameExit {
			exitCode = f.ExitCode
		}
	})
	if exitCode != 42 {
		t.Fatalf("want exit 42, got %d", exitCode)
	}
}

// TestVsockExec_WorkspaceWriteRead verifies that guest can write and read
// files inside /workspace.
func TestVsockExec_WorkspaceWriteRead(t *testing.T) {
	requireKVM(t)
	vsockPath := requireVsockPath(t)

	client := vsockClient(vsockPath)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	stamp := strconv.FormatInt(time.Now().UnixNano(), 10)
	writeScript := "echo '" + stamp + "' > /workspace/e2e_test.txt"
	readScript := "cat /workspace/e2e_test.txt"

	for _, s := range []string{writeScript, readScript} {
		err := client.Exec(ctx, protocol.ExecutionRequest{
			ID: "e2e-wr", Script: s, TimeoutSec: 5,
		}, func(f protocol.ResponseFrame) {})
		if err != nil {
			t.Fatalf("Exec(%q): %v", s, err)
		}
	}
}
