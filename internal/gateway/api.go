//go:build linux

// Package gateway exposes the Phase-4 HTTP/WebSocket surface:
//
//   - REST endpoints for VM lifecycle and HITL approval management.
//   - A WebSocket terminal that bridges browser input to the vsock exec path.
//   - A bearer-token auth middleware (constant-time compare, configurable via
//     API_TOKEN; empty = auth disabled for local dev).
//
// Register all routes on an existing mux via Handler.Register(mux).
package gateway

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/yourorg/sandbox-platform/internal/hitl"
	"github.com/yourorg/sandbox-platform/internal/orchestrator"
	"github.com/yourorg/sandbox-platform/internal/state"
)

// HandlerConfig holds operator-configurable knobs for the HTTP/WS surface.
type HandlerConfig struct {
	// Token is the API bearer token. Empty disables authentication (dev mode).
	Token string
	// Origins is the WebSocket CORS allowlist. Empty permits same-host only.
	Origins []string
	// VsockConfig is passed to New() when creating per-sandbox vsock clients.
	VsockConfig Config
	// RateCapacity / RateRefill configure the per-sandbox command rate limit.
	// Defaults: 60 capacity, 1.0 token/s.
	RateCapacity float64
	RateRefill   float64
}

func (c *HandlerConfig) applyDefaults() {
	if c.RateCapacity == 0 {
		c.RateCapacity = 60
	}
	if c.RateRefill == 0 {
		c.RateRefill = 1.0
	}
}

// Handler bundles all Phase-4 HTTP handlers. It is constructed once and
// registered on the control-plane mux; it is safe for concurrent use.
type Handler struct {
	sup   *orchestrator.Supervisor
	store *state.Store
	proxy *terminalProxy
	log   *slog.Logger
	cfg   HandlerConfig
}

// NewHandler wires the orchestrator, state store, HITL gate, and config
// into a Handler ready to register on an http.ServeMux.
func NewHandler(
	sup *orchestrator.Supervisor,
	store *state.Store,
	gate *hitl.Gate,
	log *slog.Logger,
	cfg HandlerConfig,
) *Handler {
	cfg.applyDefaults()
	if log == nil {
		log = slog.Default()
	}
	return &Handler{
		sup:   sup,
		store: store,
		proxy: newTerminalProxy(gate, cfg.VsockConfig, cfg.Origins, log),
		log:   log,
		cfg:   cfg,
	}
}

// Register attaches all Phase-4 routes to mux. It is safe to call alongside
// existing health routes already on mux.
func (h *Handler) Register(mux *http.ServeMux) {
	auth := h.requireAuth

	mux.HandleFunc("GET /api/vms", auth(h.listVMs))
	mux.HandleFunc("POST /api/vms", auth(h.launchVM))
	mux.HandleFunc("DELETE /api/vms/{id}", auth(h.terminateVM))

	mux.HandleFunc("GET /api/approvals", auth(h.listApprovals))
	mux.HandleFunc("POST /api/approvals/{id}", auth(h.decideApproval))

	// /terminal is WS-upgraded; auth is done inside via requireAuth wrapper.
	mux.HandleFunc("GET /terminal", auth(h.terminal))
}

// ── auth ───────────────────────────────────────────────────────────────────

func (h *Handler) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.cfg.Token == "" {
			next(w, r)
			return
		}
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(bearer), []byte(h.cfg.Token)) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next(w, r)
	}
}

// ── VM CRUD ────────────────────────────────────────────────────────────────

type vmView struct {
	ID        string    `json:"id"`
	CID       uint32    `json:"cid"`
	PID       int       `json:"pid"`
	VCPUs     int64     `json:"vcpus"`
	MemMiB    int64     `json:"mem_mib"`
	CreatedAt time.Time `json:"created_at"`
}

func instanceToView(inst *orchestrator.Instance) vmView {
	return vmView{
		ID:        inst.ID,
		CID:       inst.CID,
		PID:       inst.PID,
		VCPUs:     inst.Spec.VCPUs,
		MemMiB:    inst.Spec.MemMiB,
		CreatedAt: inst.CreatedAt,
	}
}

func (h *Handler) listVMs(w http.ResponseWriter, r *http.Request) {
	list := h.sup.List()
	views := make([]vmView, 0, len(list))
	for _, inst := range list {
		views = append(views, instanceToView(inst))
	}
	writeJSON(w, http.StatusOK, map[string]any{"vms": views, "count": len(views)})
}

type launchRequest struct {
	ID         string `json:"id,omitempty"`
	VCPUs      int64  `json:"vcpus,omitempty"`
	MemMiB     int64  `json:"mem_mib,omitempty"`
	CPUPercent int    `json:"cpu_percent,omitempty"`
	PidsMax    int64  `json:"pids_max,omitempty"`
}

func (h *Handler) launchVM(w http.ResponseWriter, r *http.Request) {
	var req launchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("invalid JSON: "+err.Error()))
		return
	}
	spec := orchestrator.LaunchSpec{
		ID:         req.ID,
		VCPUs:      req.VCPUs,
		MemMiB:     req.MemMiB,
		CPUPercent: req.CPUPercent,
		PidsMax:    req.PidsMax,
	}
	inst, err := h.sup.Launch(r.Context(), spec)
	if err != nil {
		if errors.Is(err, orchestrator.ErrAdmissionDenied) {
			writeJSON(w, http.StatusTooManyRequests, errBody(err.Error()))
			return
		}
		h.log.Error("launch vm", "err", err)
		writeJSON(w, http.StatusInternalServerError, errBody("launch failed"))
		return
	}
	writeJSON(w, http.StatusCreated, instanceToView(inst))
}

func (h *Handler) terminateVM(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, errBody("missing id"))
		return
	}
	if err := h.sup.Terminate(id); err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeJSON(w, http.StatusNotFound, errBody("sandbox not found"))
			return
		}
		h.log.Error("terminate vm", "id", id, "err", err)
		writeJSON(w, http.StatusInternalServerError, errBody("terminate failed"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── HITL approvals ─────────────────────────────────────────────────────────

func (h *Handler) listApprovals(w http.ResponseWriter, r *http.Request) {
	recs, err := h.proxy.gate.Pending(r.Context())
	if err != nil {
		h.log.Error("list approvals", "err", err)
		writeJSON(w, http.StatusInternalServerError, errBody("list failed"))
		return
	}
	if recs == nil {
		recs = []hitl.ApprovalRecord{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"approvals": recs, "count": len(recs)})
}

type decideRequest struct {
	Decision string `json:"decision"` // "approve" or "reject"
}

func (h *Handler) decideApproval(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, errBody("missing id"))
		return
	}

	// Identify the operator from the bearer token (or "anonymous" in dev mode).
	decidedBy := "anonymous"
	if h.cfg.Token != "" {
		if tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "); tok != "" {
			n := 8
			if len(tok) < n {
				n = len(tok)
			}
			decidedBy = "token:" + tok[:n] + "…"
		}
	}

	var req decideRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("invalid JSON"))
		return
	}

	decision := hitl.Decision(req.Decision)
	if decision != hitl.DecisionApprove && decision != hitl.DecisionReject {
		writeJSON(w, http.StatusBadRequest, errBody(`decision must be "approve" or "reject"`))
		return
	}

	if err := h.proxy.gate.Decide(r.Context(), id, decision, decidedBy); err != nil {
		if errors.Is(err, hitl.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, errBody("approval not found"))
			return
		}
		if errors.Is(err, hitl.ErrAlreadyDecided) {
			writeJSON(w, http.StatusConflict, errBody("already decided"))
			return
		}
		h.log.Error("decide approval", "id", id, "err", err)
		writeJSON(w, http.StatusInternalServerError, errBody("decision failed"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── WebSocket terminal ──────────────────────────────────────────────────────

func (h *Handler) terminal(w http.ResponseWriter, r *http.Request) {
	sandboxID := r.URL.Query().Get("sandbox")
	if sandboxID == "" {
		writeJSON(w, http.StatusBadRequest, errBody("sandbox query parameter required"))
		return
	}

	inst, ok := h.sup.Get(sandboxID)
	if !ok {
		writeJSON(w, http.StatusNotFound, errBody("sandbox not found"))
		return
	}

	rateAllow := func(ctx context.Context, name string) (bool, error) {
		return h.store.Allow(ctx, name, h.cfg.RateCapacity, h.cfg.RateRefill, 1)
	}

	h.proxy.serve(w, r, inst.VsockUDSPath, sandboxID, rateAllow)
}

// ── helpers ────────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func errBody(msg string) map[string]string { return map[string]string{"error": msg} }
