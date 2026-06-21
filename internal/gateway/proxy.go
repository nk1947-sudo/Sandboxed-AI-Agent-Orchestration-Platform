//go:build linux

package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"sync"

	"github.com/gorilla/websocket"

	"github.com/yourorg/sandbox-platform/internal/hitl"
	"github.com/yourorg/sandbox-platform/internal/protocol"
)

// wsRequest is the JSON message a browser sends over the terminal WebSocket.
type wsRequest struct {
	Script     string            `json:"script"`
	TimeoutSec int               `json:"timeout_sec,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
}

// wsEvent is the JSON frame the server streams back over the terminal WebSocket.
type wsEvent struct {
	Kind       string `json:"kind"`
	Data       string `json:"data,omitempty"`
	ExitCode   int    `json:"exit_code,omitempty"`
	ApprovalID string `json:"approval_id,omitempty"`
	Reason     string `json:"reason,omitempty"`
	Message    string `json:"message,omitempty"`
}

const (
	evtOutput   = "output"
	evtExit     = "exit"
	evtError    = "error"
	evtPending  = "pending"
	evtApproved = "approved"
	evtRejected = "rejected"
)

// terminalProxy is the WebSocket↔vsock bridge for the /terminal endpoint.
// It is embedded inside Handler (api.go) and shares its fields.
type terminalProxy struct {
	gate     *hitl.Gate
	vsockCfg Config
	origins  []string
	log      *slog.Logger
	upgrader *websocket.Upgrader
}

func newTerminalProxy(gate *hitl.Gate, vsockCfg Config, origins []string, log *slog.Logger) *terminalProxy {
	p := &terminalProxy{
		gate:     gate,
		vsockCfg: vsockCfg,
		origins:  origins,
		log:      log,
	}
	p.upgrader = &websocket.Upgrader{
		ReadBufferSize:  4096,
		WriteBufferSize: 32768,
		CheckOrigin:     p.checkOrigin,
	}
	return p
}

// checkOrigin validates the WebSocket Origin header against h.origins.
// An empty Origin header (curl, direct API calls) is always allowed.
// If no allowlist is configured, only same-host origins are permitted.
func (p *terminalProxy) checkOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true // same-origin or non-browser client
	}
	for _, allowed := range p.origins {
		if origin == allowed {
			return true
		}
	}
	if len(p.origins) > 0 {
		return false // explicit allowlist but no match
	}
	// No allowlist: permit same-host only.
	ou, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return ou.Host == r.Host
}

// serve handles one WebSocket terminal session.
// vsockPath is the Firecracker UDS path for the target sandbox.
// sandboxID is used for rate-limit keying and logging.
func (p *terminalProxy) serve(
	w http.ResponseWriter,
	r *http.Request,
	vsockPath string,
	sandboxID string,
	rateAllow func(ctx context.Context, name string) (bool, error),
) {
	conn, err := p.upgrader.Upgrade(w, r, nil)
	if err != nil {
		p.log.Error("ws upgrade failed", "sandbox", sandboxID, "err", err)
		return
	}
	defer conn.Close()

	p.log.Info("terminal session started", "sandbox", sandboxID)
	defer p.log.Info("terminal session ended", "sandbox", sandboxID)

	client := New(vsockPath, p.vsockCfg)

	// serializedWrite serializes concurrent writes to the WS connection.
	var writeMu sync.Mutex
	send := func(evt wsEvent) {
		b, _ := json.Marshal(evt)
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = conn.WriteMessage(websocket.TextMessage, b)
	}

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err,
				websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				p.log.Warn("terminal ws unexpected close", "sandbox", sandboxID, "err", err)
			}
			return
		}

		var req wsRequest
		if err := json.Unmarshal(msg, &req); err != nil {
			send(wsEvent{Kind: evtError, Message: "invalid request JSON"})
			continue
		}
		if req.Script == "" {
			send(wsEvent{Kind: evtError, Message: "script is required"})
			continue
		}

		// Rate limit: up to 60 commands/min per sandbox.
		if rateAllow != nil {
			ok, err := rateAllow(r.Context(), "cmd:"+sandboxID)
			if err != nil {
				p.log.Warn("rate limit check failed", "err", err)
			} else if !ok {
				send(wsEvent{Kind: evtError, Message: "rate limit exceeded (60 commands/min)"})
				continue
			}
		}

		// Classify the script.
		class, reason := p.gate.Classify(req.Script)
		if class == hitl.ClassSensitive {
			approvalID, err := p.gate.Submit(r.Context(), sandboxID, req.Script, reason)
			if err != nil {
				p.log.Error("hitl submit failed", "sandbox", sandboxID, "err", err)
				send(wsEvent{Kind: evtError, Message: "failed to submit for approval"})
				continue
			}
			send(wsEvent{Kind: evtPending, ApprovalID: approvalID, Reason: reason})

			decision, err := p.gate.WaitForDecision(r.Context(), approvalID)
			if err != nil {
				send(wsEvent{Kind: evtError, Message: "approval wait interrupted"})
				continue
			}
			if decision == hitl.StateRejected {
				send(wsEvent{Kind: evtRejected})
				continue
			}
			send(wsEvent{Kind: evtApproved})
		}

		// Execute — benign or newly approved.
		execReq := protocol.ExecutionRequest{
			ID:         newExecID(),
			Kind:       protocol.KindExec,
			Script:     req.Script,
			TimeoutSec: req.TimeoutSec,
			Env:        req.Env,
		}

		execErr := client.Exec(r.Context(), execReq, func(f protocol.ResponseFrame) {
			switch f.Kind {
			case protocol.FrameOutput:
				send(wsEvent{Kind: evtOutput, Data: f.Data})
			case protocol.FrameExit:
				send(wsEvent{Kind: evtExit, ExitCode: f.ExitCode})
			}
		})
		if execErr != nil {
			p.log.Warn("vsock exec error", "sandbox", sandboxID, "err", execErr)
			send(wsEvent{Kind: evtError, Message: execErr.Error()})
		}
	}
}

func newExecID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "exec"
	}
	return "ex-" + hex.EncodeToString(b)
}
