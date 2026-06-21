//go:build linux

package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/yourorg/sandbox-platform/internal/protocol"
)

// collectOutput runs data through a streamWriter and reassembles the output
// frames the way the host side would.
func collectOutput(t *testing.T, data []byte, max int64, chunk int) (string, bool) {
	t.Helper()
	var buf bytes.Buffer
	fw := protocol.NewFrameWriter(&buf)
	sw := newStreamWriter(fw, "id", max, chunk)

	n, err := sw.Write(data)
	if err != nil {
		t.Fatalf("streamWriter.Write: %v", err)
	}
	if n != len(data) {
		t.Fatalf("Write returned %d, want %d (must report full length to avoid blocking the child)", n, len(data))
	}

	var out []byte
	for {
		var f protocol.ResponseFrame
		if err := protocol.ReadMessage(&buf, &f); err != nil {
			break // EOF: no more frames
		}
		if f.Kind == protocol.FrameOutput {
			out = append(out, f.Data...)
		}
	}
	return string(out), sw.Truncated()
}

func TestStreamWriterChunking(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 100)
	out, truncated := collectOutput(t, data, 0 /* no cap */, 32)
	if truncated {
		t.Fatal("should not be truncated with no cap")
	}
	if out != string(data) {
		t.Fatalf("reassembled output mismatch: got %d bytes, want %d", len(out), len(data))
	}
}

func TestStreamWriterTruncation(t *testing.T) {
	data := bytes.Repeat([]byte("y"), 100)
	out, truncated := collectOutput(t, data, 10 /* cap */, 32)
	if !truncated {
		t.Fatal("should be truncated past the cap")
	}
	if len(out) != 10 {
		t.Fatalf("got %d bytes past cap, want 10", len(out))
	}
}

func TestSanitizedEnv(t *testing.T) {
	cfg := config{Workdir: "/workspace", Shell: "/bin/bash"}
	env := envMap(sanitizedEnv(cfg, map[string]string{"FOO": "bar", "PATH": "/override"}))

	if env["HOME"] != "/workspace" {
		t.Fatalf("HOME = %q, want /workspace", env["HOME"])
	}
	if env["SHELL"] != "/bin/bash" {
		t.Fatalf("SHELL = %q, want /bin/bash", env["SHELL"])
	}
	if env["FOO"] != "bar" {
		t.Fatalf("extra var FOO not applied: %q", env["FOO"])
	}
	if env["PATH"] != "/override" {
		t.Fatalf("caller override of PATH not applied: %q", env["PATH"])
	}
}

func envMap(kv []string) map[string]string {
	m := make(map[string]string, len(kv))
	for _, e := range kv {
		if k, v, ok := strings.Cut(e, "="); ok {
			m[k] = v
		}
	}
	return m
}
