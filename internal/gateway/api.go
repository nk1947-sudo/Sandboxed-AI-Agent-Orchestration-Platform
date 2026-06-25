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
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/yourorg/sandbox-platform/internal/auth"
	"github.com/yourorg/sandbox-platform/internal/hitl"
	"github.com/yourorg/sandbox-platform/internal/orchestrator"
	"github.com/yourorg/sandbox-platform/internal/state"
	"github.com/yourorg/sandbox-platform/internal/store/pg"
)

// HandlerConfig holds operator-configurable knobs for the HTTP/WS surface.
type HandlerConfig struct {
	// Token is the API/service bearer token for CLI and automation. Empty
	// disables token auth; combined with no DB sessions it opens dev mode.
	Token string
	// Origins is the WebSocket CORS allowlist. Empty permits same-host only.
	Origins []string
	// VsockConfig is passed to New() when creating per-sandbox vsock clients.
	VsockConfig Config
	// RateCapacity / RateRefill configure the per-sandbox command rate limit.
	// Defaults: 60 capacity, 1.0 token/s.
	RateCapacity float64
	RateRefill   float64
	// CookieSecure sets the Secure flag on the session cookie (true in prod/HTTPS).
	CookieSecure bool
}

func (c *HandlerConfig) applyDefaults() {
	if c.RateCapacity == 0 {
		c.RateCapacity = 60
	}
	if c.RateRefill == 0 {
		c.RateRefill = 1.0
	}
}

// Deps are the runtime dependencies wired into a Handler. DB, Sessions and
// Snapshots are optional: when nil, user accounts / persistent history /
// resume are disabled and the gateway falls back to token-or-open auth.
type Deps struct {
	Supervisor *orchestrator.Supervisor
	State      *state.Store
	Gate       *hitl.Gate
	DB         *pg.Store
	Sessions   *auth.Sessions
	Snapshots  *orchestrator.SnapshotEngine
	Log        *slog.Logger
}

// Handler bundles all HTTP/WebSocket handlers. It is constructed once and
// registered on the control-plane mux; it is safe for concurrent use.
type Handler struct {
	sup      *orchestrator.Supervisor
	store    *state.Store
	db       *pg.Store
	sessions *auth.Sessions
	snap     *orchestrator.SnapshotEngine
	proxy    *terminalProxy
	log      *slog.Logger
	cfg      HandlerConfig
}

// NewHandler wires dependencies and config into a Handler ready to register on
// an http.ServeMux.
func NewHandler(d Deps, cfg HandlerConfig) *Handler {
	cfg.applyDefaults()
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	var transcripts *pg.TranscriptRepo
	var sandboxes *pg.SandboxRepo
	if d.DB != nil {
		transcripts = d.DB.Transcripts
		sandboxes = d.DB.Sandboxes
	}
	return &Handler{
		sup:      d.Supervisor,
		store:    d.State,
		db:       d.DB,
		sessions: d.Sessions,
		snap:     d.Snapshots,
		proxy:    newTerminalProxy(d.Gate, cfg.VsockConfig, cfg.Origins, log, transcripts, sandboxes),
		log:      log,
		cfg:      cfg,
	}
}

// Register attaches all Phase-4 routes to mux. It is safe to call alongside
// existing health routes already on mux.
func (h *Handler) Register(mux *http.ServeMux) {
	auth := h.requireAuth

	// Auth: login is public (rate-limited inside); the rest require a session.
	mux.HandleFunc("POST /api/login", h.login)
	mux.HandleFunc("POST /api/logout", auth(h.logout))
	mux.HandleFunc("GET /api/me", auth(h.me))
	mux.HandleFunc("GET /api/users", auth(h.listUsers))
	mux.HandleFunc("POST /api/users", auth(h.createUser))

	mux.HandleFunc("GET /api/vms", auth(h.listVMs))
	mux.HandleFunc("POST /api/vms", auth(h.launchVM))
	mux.HandleFunc("DELETE /api/vms/{id}", auth(h.terminateVM))

	// Durable sandbox history + resume (Postgres-backed).
	mux.HandleFunc("GET /api/sandboxes", auth(h.listSandboxes))
	mux.HandleFunc("POST /api/sandboxes/{id}/stop", auth(h.stopSandbox))
	mux.HandleFunc("POST /api/sandboxes/{id}/resume", auth(h.resumeSandbox))
	mux.HandleFunc("PATCH /api/sandboxes/{id}", auth(h.renameSandbox))
	mux.HandleFunc("DELETE /api/sandboxes/{id}", auth(h.deleteSandbox))
	mux.HandleFunc("GET /api/sandboxes/{id}/transcript", auth(h.getTranscript))

	mux.HandleFunc("GET /api/approvals", auth(h.listApprovals))
	mux.HandleFunc("POST /api/approvals/{id}", auth(h.decideApproval))

	// /terminal is WS-upgraded; auth is done inside via requireAuth wrapper.
	mux.HandleFunc("GET /terminal", auth(h.terminal))
}

// ── auth ───────────────────────────────────────────────────────────────────

// requireAuth authenticates a request via, in order: (1) the session cookie
// (browser); (2) the bearer/query API token (CLI, automation, non-browser WS);
// (3) dev-open mode when neither a token nor DB sessions are configured. The
// resolved principal is attached to the request context for ownership + audit.
func (h *Handler) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 1. Cookie session (browser; also carries WS handshake auth).
		if h.sessions != nil {
			if sid := auth.CookieValue(r); sid != "" {
				if u, err := h.sessions.Resolve(r.Context(), sid); err == nil {
					p := auth.Principal{UserID: u.ID, Username: u.Username, Role: u.Role}
					next(w, r.WithContext(auth.WithPrincipal(r.Context(), p)))
					return
				}
			}
		}
		// 2. Bearer header or ?token= query param (browsers can't set headers on
		// a WS handshake). Constant-time compare to avoid a token timing oracle.
		if h.cfg.Token != "" {
			tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if tok == "" {
				tok = r.URL.Query().Get("token")
			}
			if tok != "" && auth.ConstantTimeEqual(tok, h.cfg.Token) {
				p := auth.Principal{Username: "service", Role: pg.RoleAdmin, Service: true}
				next(w, r.WithContext(auth.WithPrincipal(r.Context(), p)))
				return
			}
		}
		// 3. Dev-open mode: no token AND no session backend configured.
		if h.cfg.Token == "" && h.sessions == nil {
			next(w, r)
			return
		}
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
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
	if h.sup == nil {
		writeJSON(w, http.StatusInternalServerError, errBody("supervisor unavailable"))
		return
	}
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
	Egress     bool   `json:"egress,omitempty"` // opt-in egress (needs EGRESS_ENABLED)
}

func (h *Handler) launchVM(w http.ResponseWriter, r *http.Request) {
	if h.sup == nil {
		writeJSON(w, http.StatusInternalServerError, errBody("supervisor unavailable"))
		return
	}
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
		Egress:     req.Egress,
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
	// Record durable history (owned by the caller).
	if h.db != nil {
		p, _ := auth.FromContext(r.Context())
		if err := h.db.Sandboxes.Upsert(r.Context(), pg.Sandbox{
			ID:         inst.ID,
			OwnerID:    p.UserID,
			Status:     pg.SandboxRunning,
			VCPUs:      int(inst.Spec.VCPUs),
			MemMiB:     int(inst.Spec.MemMiB),
			CPUPercent: inst.Spec.CPUPercent,
			PidsMax:    int(inst.Spec.PidsMax),
			CID:        int64(inst.CID),
		}); err != nil {
			h.log.Warn("persist sandbox", "id", inst.ID, "err", err)
		}
		h.audit(r, p.UserID, "sandbox.launch", inst.ID, nil)
	}
	writeJSON(w, http.StatusCreated, instanceToView(inst))
}

func (h *Handler) terminateVM(w http.ResponseWriter, r *http.Request) {
	if h.sup == nil {
		writeJSON(w, http.StatusInternalServerError, errBody("supervisor unavailable"))
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, errBody("missing id"))
		return
	}
	// Ownership: an operator may only terminate their own sandbox (admins/service
	// pass; dev-mode without the data layer is unrestricted).
	if h.db != nil {
		if _, ok := h.authorizeSandbox(w, r, id); !ok {
			return
		}
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
	// Reflect the hard kill in durable history (no snapshot → not resumable).
	if h.db != nil {
		_ = h.db.Sandboxes.MarkStopped(r.Context(), id)
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

	// Attribute the decision to the authenticated principal.
	decidedBy := "anonymous"
	if p, ok := auth.FromContext(r.Context()); ok && p.Username != "" {
		decidedBy = p.Username
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
	// Ownership: an operator may only attach to their own sandbox's shell
	// (admins/service pass; dev-mode without the data layer is unrestricted).
	if h.db != nil {
		if _, ok := h.authorizeSandbox(w, r, sandboxID); !ok {
			return
		}
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
