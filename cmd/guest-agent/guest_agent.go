//go:build linux

// Command guest_agent is the in-VM RPC daemon. It is compiled as a static,
// runtime-less binary (CGO_ENABLED=0) and baked into the guest rootfs, where
// OpenRC launches it at boot (see deploy/rootfs). It listens on the guest side
// of the virtio-vsock channel and executes shell scripts dispatched by the host
// control plane, streaming merged stdout/stderr back in real time.
//
// In-guest security posture (defense in depth behind the KVM/jailer/cgroup
// boundaries the host already enforces):
//
//   - Every script runs as an unprivileged user (default uid/gid 1000), never
//     as the guest root that owns the agent itself.
//   - Each script runs in its own process group so a timeout — or normal exit —
//     can SIGKILL the entire tree. This is what actually contains
//     `:(){ :|:& };:` style fork bombs whose children are backgrounded with `&`.
//   - Execution is bounded by a wall-clock timeout and a total-output cap.
//   - The child starts from a minimal, sanitized environment in a locked-down
//     working directory (/workspace).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/mdlayher/vsock"

	"github.com/yourorg/sandbox-platform/internal/protocol"
)

// config is resolved from flags, each defaulting to an environment variable so
// the same binary can be tuned via the rootfs init script without recompiling.
type config struct {
	Port           uint32
	UID            uint32
	GID            uint32
	Shell          string
	Workdir        string
	DefaultTimeout time.Duration
	MaxTimeout     time.Duration
	MaxOutput      int64
	ChunkSize      int
}

func main() {
	cfg := loadConfig()

	log.SetFlags(log.LstdFlags | log.Lmicroseconds | log.LUTC)
	log.SetPrefix("[guest-agent] ")

	if err := prepareWorkspace(cfg); err != nil {
		log.Fatalf("workspace setup failed: %v", err)
	}

	ln, err := vsock.Listen(cfg.Port, nil)
	if err != nil {
		log.Fatalf("vsock listen on port %d: %v", cfg.Port, err)
	}
	defer ln.Close()
	log.Printf("listening on vsock port %d (exec uid=%d gid=%d workspace=%s)",
		cfg.Port, cfg.UID, cfg.GID, cfg.Workdir)

	// Graceful shutdown: closing the listener unblocks Accept; in-flight
	// handlers drain via the WaitGroup.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	go func() {
		<-ctx.Done()
		log.Printf("shutdown signal received; closing listener")
		_ = ln.Close()
	}()

	var wg sync.WaitGroup
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				break // listener closed during shutdown
			}
			log.Printf("accept error: %v", err)
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			handleConn(ctx, cfg, conn)
		}()
	}
	wg.Wait()
	log.Printf("all connections drained; exiting")
}

// handleConn reads one ExecutionRequest and streams the response. A panic in a
// single connection must never take down the agent for every other sandbox
// consumer, so handlers recover.
func handleConn(ctx context.Context, cfg config, conn net.Conn) {
	defer conn.Close()
	defer func() {
		if r := recover(); r != nil {
			log.Printf("recovered panic in connection handler: %v", r)
		}
	}()

	fw := protocol.NewFrameWriter(conn)

	// Bound how long we wait for the request header+body so a connection that
	// never sends anything cannot pin a goroutine forever.
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	var req protocol.ExecutionRequest
	if err := protocol.ReadMessage(conn, &req); err != nil {
		if !errors.Is(err, io.EOF) {
			log.Printf("read request: %v", err)
		}
		return
	}
	// Execution has its own timeout; clear the read deadline.
	_ = conn.SetReadDeadline(time.Time{})

	switch req.Kind {
	case protocol.KindPing:
		_ = fw.Write(protocol.ResponseFrame{ID: req.ID, Kind: protocol.FramePong})
	case protocol.KindExec, "":
		runScript(ctx, cfg, fw, req)
	default:
		_ = fw.Write(protocol.ResponseFrame{
			ID:    req.ID,
			Kind:  protocol.FrameError,
			Error: fmt.Sprintf("unknown request kind: %q", req.Kind),
		})
	}
}

// runScript executes one script under the unprivileged user, streaming merged
// stdout/stderr, then sends a terminal Exit or Error frame.
func runScript(parent context.Context, cfg config, fw *protocol.FrameWriter, req protocol.ExecutionRequest) {
	if req.Script == "" {
		_ = fw.Write(protocol.ResponseFrame{ID: req.ID, Kind: protocol.FrameError, Error: "empty script"})
		return
	}

	timeout := cfg.DefaultTimeout
	if req.TimeoutSec > 0 {
		timeout = time.Duration(req.TimeoutSec) * time.Second
	}
	if timeout > cfg.MaxTimeout {
		timeout = cfg.MaxTimeout
	}

	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	workdir := cfg.Workdir
	if req.Workdir != "" {
		workdir = req.Workdir
	}

	cmd := exec.CommandContext(ctx, cfg.Shell, "-c", req.Script)
	cmd.Dir = workdir
	cmd.Env = sanitizedEnv(cfg, req.Env)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		// New process group: lets us signal the whole tree with kill(-pgid).
		Setpgid: true,
		// Drop privileges to the unprivileged guest user for every child.
		Credential: &syscall.Credential{
			Uid:         cfg.UID,
			Gid:         cfg.GID,
			NoSetGroups: true, // skip setgroups(2); the user has no supplementary groups
		},
	}
	// On timeout/cancel, SIGKILL the entire process group rather than just bash.
	// exec.CommandContext would otherwise only kill the immediate child, leaving
	// `&`-backgrounded fork-bomb processes alive. The host cgroup pids.max is the
	// hard backstop; this is the in-guest first line of defense.
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 3 * time.Second

	// Setting Stdout and Stderr to the SAME writer makes os/exec use a single
	// pipe for both, so output is correctly interleaved in real time. (This is
	// what the draft's io.MultiReader could not do — it drained stdout fully
	// before reading any stderr.)
	sw := newStreamWriter(fw, req.ID, cfg.MaxOutput, cfg.ChunkSize)
	cmd.Stdout = sw
	cmd.Stderr = sw

	if err := cmd.Start(); err != nil {
		_ = fw.Write(protocol.ResponseFrame{ID: req.ID, Kind: protocol.FrameError, Error: "start failed: " + err.Error()})
		return
	}
	pgid := cmd.Process.Pid

	// cmd.Wait returns only after the process exits AND the output pipe has been
	// fully copied to sw, so streaming is complete here.
	waitErr := cmd.Wait()

	// Reap anything the script backgrounded into our group (fork bombs, stray
	// daemons). ESRCH (group already empty) is expected and ignored.
	_ = syscall.Kill(-pgid, syscall.SIGKILL)

	if sw.Truncated() {
		_ = fw.Write(protocol.ResponseFrame{
			ID:   req.ID,
			Kind: protocol.FrameOutput,
			Data: fmt.Sprintf("\n[output truncated at %d bytes]\n", cfg.MaxOutput),
		})
	}

	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		_ = fw.Write(protocol.ResponseFrame{
			ID:    req.ID,
			Kind:  protocol.FrameError,
			Error: fmt.Sprintf("execution timed out after %s", timeout),
		})
		return
	}

	exitCode := 0
	if waitErr != nil {
		var ee *exec.ExitError
		if errors.As(waitErr, &ee) {
			exitCode = ee.ExitCode()
		} else {
			_ = fw.Write(protocol.ResponseFrame{ID: req.ID, Kind: protocol.FrameError, Error: waitErr.Error()})
			return
		}
	}
	_ = fw.Write(protocol.ResponseFrame{ID: req.ID, Kind: protocol.FrameExit, ExitCode: exitCode})
}

// sanitizedEnv builds a minimal base environment and overlays caller-supplied
// variables. We never inherit the agent's own environment, so secrets injected
// into the guest init are not leaked to untrusted scripts.
func sanitizedEnv(cfg config, extra map[string]string) []string {
	base := map[string]string{
		"PATH":  "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME":  cfg.Workdir,
		"PWD":   cfg.Workdir,
		"SHELL": cfg.Shell,
		"TERM":  "xterm-256color",
		"LANG":  "C.UTF-8",
	}
	for k, v := range extra {
		base[k] = v
	}
	out := make([]string, 0, len(base))
	for k, v := range base {
		out = append(out, k+"="+v)
	}
	return out
}

// streamWriter forwards process output as protocol Output frames, chunking large
// writes and enforcing a total-output cap. It implements io.Writer so it can be
// wired directly into exec.Cmd. Past the cap it silently discards bytes but keeps
// reporting them as consumed, so the child never blocks on a full stdout pipe.
type streamWriter struct {
	fw    *protocol.FrameWriter
	id    string
	max   int64
	chunk int

	mu        sync.Mutex
	written   int64
	truncated bool
}

func newStreamWriter(fw *protocol.FrameWriter, id string, max int64, chunk int) *streamWriter {
	if chunk <= 0 {
		chunk = 32 << 10
	}
	return &streamWriter{fw: fw, id: id, max: max, chunk: chunk}
}

func (s *streamWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	n := len(p) // bytes "consumed" from the caller's perspective
	if s.truncated {
		return n, nil
	}
	if s.max > 0 && s.written+int64(len(p)) > s.max {
		if allow := s.max - s.written; allow > 0 {
			p = p[:allow]
		} else {
			p = nil
		}
		s.truncated = true
	}

	for len(p) > 0 {
		c := p
		if len(c) > s.chunk {
			c = c[:s.chunk]
		}
		if err := s.fw.Write(protocol.ResponseFrame{ID: s.id, Kind: protocol.FrameOutput, Data: string(c)}); err != nil {
			return n, err
		}
		s.written += int64(len(c))
		p = p[len(c):]
	}
	return n, nil
}

func (s *streamWriter) Truncated() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.truncated
}

// prepareWorkspace ensures the unprivileged execution directory exists and is
// owned by the exec user (the agent runs as root inside the guest).
func prepareWorkspace(cfg config) error {
	if err := os.MkdirAll(cfg.Workdir, 0o775); err != nil {
		return err
	}
	if err := os.Chown(cfg.Workdir, int(cfg.UID), int(cfg.GID)); err != nil {
		return fmt.Errorf("chown %s: %w", cfg.Workdir, err)
	}
	return nil
}

func loadConfig() config {
	port := flag.Int("port", envInt("AGENT_VSOCK_PORT", int(protocol.DefaultPort)), "guest vsock port to listen on")
	uid := flag.Int("uid", envInt("AGENT_UID", 1000), "uid scripts are executed as")
	gid := flag.Int("gid", envInt("AGENT_GID", 1000), "gid scripts are executed as")
	shell := flag.String("shell", envStr("AGENT_SHELL", "/bin/bash"), "shell used to run scripts")
	workdir := flag.String("workdir", envStr("AGENT_WORKDIR", "/workspace"), "working directory for scripts")
	defTO := flag.Int("default-timeout", envInt("AGENT_DEFAULT_TIMEOUT_SEC", 30), "default execution timeout (seconds)")
	maxTO := flag.Int("max-timeout", envInt("AGENT_MAX_TIMEOUT_SEC", 300), "maximum execution timeout (seconds)")
	maxOut := flag.Int("max-output", envInt("AGENT_MAX_OUTPUT_BYTES", 4<<20), "max bytes of output streamed per request")
	chunk := flag.Int("chunk", envInt("AGENT_CHUNK_BYTES", 32<<10), "output chunk size in bytes")
	flag.Parse()

	return config{
		Port:           uint32(*port),
		UID:            uint32(*uid),
		GID:            uint32(*gid),
		Shell:          *shell,
		Workdir:        *workdir,
		DefaultTimeout: time.Duration(*defTO) * time.Second,
		MaxTimeout:     time.Duration(*maxTO) * time.Second,
		MaxOutput:      int64(*maxOut),
		ChunkSize:      *chunk,
	}
}

func envStr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
