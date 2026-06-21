//go:build linux

// Package gateway provides the host-side client for the Firecracker vsock
// multiplexing handshake and the length-prefixed JSON protocol defined in
// internal/protocol.
//
// Firecracker exposes one Unix domain socket per microVM. Multiplexing to a
// specific guest port is done in-band:
//
//	host → FC: "CONNECT <port>\n"
//	FC → host: "OK <host-port>\n"   ← success; bytes now relay to guest:<port>
//	host → guest: <length-prefixed ExecutionRequest>
//	guest → host: <length-prefixed ResponseFrame>…  (terminal frame closes it)
//
// One request per connection; concurrent callers each open their own connection.
package gateway

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/yourorg/sandbox-platform/internal/protocol"
)

const (
	defaultDialTimeout  = 5 * time.Second
	defaultRetryBase    = 100 * time.Millisecond
	defaultRetryCap     = 2 * time.Second
	defaultPingDeadline = 5 * time.Second
)

// ErrHandshakeFailed is returned when Firecracker replies to CONNECT with a
// non-OK line (wrong port, VM not started, bridge misconfigured).
var ErrHandshakeFailed = errors.New("gateway: vsock handshake failed")

// Config holds tunable parameters for a Client. Zero values are replaced with
// sensible defaults by New — except RetryMax, where 0 means "try once".
type Config struct {
	// Port is the guest vsock port the agent listens on.
	// Zero is treated as protocol.DefaultPort (5005).
	Port uint32

	// DialTimeout bounds each individual dial-and-handshake attempt.
	// Zero is treated as 5 s.
	DialTimeout time.Duration

	// RetryMax is the number of retries after the first attempt fails.
	// 0 = try exactly once (no retries).
	// Use a positive value (e.g. 10) to tolerate a still-booting guest agent.
	RetryMax int

	// RetryBase is the initial backoff interval; doubles each attempt up to RetryCap.
	// Zero is treated as 100 ms.
	RetryBase time.Duration

	// RetryCap is the maximum per-attempt backoff interval.
	// Zero is treated as 2 s.
	RetryCap time.Duration

	// PingDeadline is the total deadline for a Ping call.
	// Zero is treated as 5 s.
	PingDeadline time.Duration

	// Logger receives debug-level retry events. Nil uses slog.Default().
	Logger *slog.Logger
}

func (c *Config) applyDefaults() {
	if c.Port == 0 {
		c.Port = protocol.DefaultPort
	}
	if c.DialTimeout == 0 {
		c.DialTimeout = defaultDialTimeout
	}
	if c.RetryBase == 0 {
		c.RetryBase = defaultRetryBase
	}
	if c.RetryCap == 0 {
		c.RetryCap = defaultRetryCap
	}
	if c.PingDeadline == 0 {
		c.PingDeadline = defaultPingDeadline
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

// Client speaks the Firecracker vsock handshake and the protocol defined in
// internal/protocol for one microVM identified by its vsock UDS path. All
// exported methods are safe for concurrent use; each call opens its own conn.
type Client struct {
	udsPath string
	cfg     Config
}

// New returns a Client that dials vsockUDSPath for every RPC.
func New(vsockUDSPath string, cfg Config) *Client {
	cfg.applyDefaults()
	return &Client{udsPath: vsockUDSPath, cfg: cfg}
}

// Exec sends req to the guest and calls onFrame for every ResponseFrame received
// in order. It returns after the terminal frame (FrameExit / FrameError /
// FramePong) or after ctx is cancelled. A FrameError from the guest is returned
// as a non-nil error; onFrame is still called for that terminal frame first.
func (c *Client) Exec(ctx context.Context, req protocol.ExecutionRequest, onFrame func(protocol.ResponseFrame)) error {
	conn, err := c.dialWithRetry(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	// When ctx is cancelled, close conn so the blocked ReadMessage returns.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	if err := protocol.WriteMessage(conn, req); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("gateway: send request: %w", err)
	}

	for {
		var frame protocol.ResponseFrame
		if err := protocol.ReadMessage(conn, &frame); err != nil {
			// Prefer reporting the context error when it caused the conn close.
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err == io.EOF {
				return fmt.Errorf("gateway: connection closed before terminal frame")
			}
			return fmt.Errorf("gateway: read frame: %w", err)
		}
		onFrame(frame)
		if frame.Terminal() {
			if frame.Kind == protocol.FrameError {
				return fmt.Errorf("gateway: guest error: %s", frame.Error)
			}
			return nil
		}
	}
}

// Ping sends a liveness probe and expects a pong within PingDeadline. It is
// safe to call from the orchestrator health loop on every active sandbox.
func (c *Client) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.PingDeadline)
	defer cancel()
	return c.Exec(ctx, protocol.ExecutionRequest{
		ID:   "hb",
		Kind: protocol.KindPing,
	}, func(protocol.ResponseFrame) {})
}

// dialWithRetry retries the dial+handshake until success, context cancellation,
// or RetryMax retries are exhausted. Exponential backoff (RetryBase → RetryCap)
// lets callers safely invoke this while the guest agent is still booting.
func (c *Client) dialWithRetry(ctx context.Context) (net.Conn, error) {
	backoff := c.cfg.RetryBase
	var lastErr error

	for attempt := 0; attempt <= c.cfg.RetryMax; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("gateway: cancelled after %d attempt(s): %w", attempt, ctx.Err())
			case <-time.After(backoff):
			}
			backoff *= 2
			if backoff > c.cfg.RetryCap {
				backoff = c.cfg.RetryCap
			}
		}

		conn, err := c.connect(ctx)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		c.cfg.Logger.Debug("gateway: connect attempt failed",
			"attempt", attempt+1, "max", c.cfg.RetryMax+1, "err", err)
	}
	return nil, fmt.Errorf("gateway: gave up after %d attempt(s): %w", c.cfg.RetryMax+1, lastErr)
}

// vsockConn overrides the Read path of a net.Conn with a bufio.Reader so that
// bytes buffered during the handshake reply ("OK <port>\n") are not silently
// discarded before the first protocol.ReadMessage call.
type vsockConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *vsockConn) Read(b []byte) (int, error) { return c.r.Read(b) }

// connect performs exactly one dial and the Firecracker vsock handshake.
func (c *Client) connect(ctx context.Context) (net.Conn, error) {
	dialCtx, cancel := context.WithTimeout(ctx, c.cfg.DialTimeout)
	defer cancel()

	var d net.Dialer
	raw, err := d.DialContext(dialCtx, "unix", c.udsPath)
	if err != nil {
		return nil, fmt.Errorf("gateway: dial %s: %w", c.udsPath, err)
	}

	br := bufio.NewReader(raw)
	if err := doHandshake(raw, br, c.cfg.Port, c.cfg.DialTimeout); err != nil {
		_ = raw.Close()
		return nil, err
	}
	return &vsockConn{Conn: raw, r: br}, nil
}

// doHandshake sends "CONNECT <port>\n" and reads the "OK <port>\n" reply.
// Reading through br (not directly from conn) ensures any read-ahead past the
// OK line is not lost when the caller switches to protocol.ReadMessage.
func doHandshake(conn net.Conn, br *bufio.Reader, port uint32, timeout time.Duration) error {
	_ = conn.SetDeadline(time.Now().Add(timeout))

	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", port); err != nil {
		return fmt.Errorf("gateway: write CONNECT: %w", err)
	}

	line, err := br.ReadString('\n')
	if err != nil {
		return fmt.Errorf("gateway: read handshake reply: %w", err)
	}
	line = strings.TrimRight(line, "\r\n")

	if !strings.HasPrefix(line, "OK ") {
		return fmt.Errorf("%w: got %q", ErrHandshakeFailed, line)
	}

	// Clear deadline — execution timeouts are managed by the guest agent.
	_ = conn.SetDeadline(time.Time{})
	return nil
}
