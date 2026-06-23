//go:build linux

package gateway

import (
	"encoding/json"
	"net"
	"net/http"
	"strings"

	"github.com/yourorg/sandbox-platform/internal/auth"
	"github.com/yourorg/sandbox-platform/internal/store/pg"
)

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type userView struct {
	ID       string  `json:"id"`
	Username string  `json:"username"`
	Role     pg.Role `json:"role"`
}

// login verifies credentials and issues a session cookie. Public + rate-limited.
func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	if h.db == nil || h.sessions == nil {
		writeJSON(w, http.StatusNotImplemented, errBody("accounts not enabled"))
		return
	}
	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("invalid JSON"))
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	if req.Username == "" || req.Password == "" {
		writeJSON(w, http.StatusBadRequest, errBody("username and password required"))
		return
	}

	// Brute-force protection: ~5 attempts burst, refilling at 1 per 10s, keyed
	// by username + client IP.
	if h.store != nil {
		key := "login:" + req.Username + ":" + clientIP(r)
		if ok, err := h.store.Allow(r.Context(), key, 5, 0.1, 1); err == nil && !ok {
			writeJSON(w, http.StatusTooManyRequests, errBody("too many attempts; try again later"))
			return
		}
	}

	u, err := h.db.Users.ByUsername(r.Context(), req.Username)
	if err != nil || u.Disabled || !auth.VerifyPassword(u.PasswordHash, req.Password) {
		h.audit(r, "", "login.failure", req.Username, nil)
		// Uniform response to avoid username enumeration.
		writeJSON(w, http.StatusUnauthorized, errBody("invalid credentials"))
		return
	}

	sid, exp, err := h.sessions.Create(r.Context(), u.ID, r.UserAgent(), clientIP(r))
	if err != nil {
		h.log.Error("create session", "err", err)
		writeJSON(w, http.StatusInternalServerError, errBody("login failed"))
		return
	}
	auth.SetSessionCookie(w, sid, exp, h.cfg.CookieSecure)
	h.audit(r, u.ID, "login.success", u.Username, nil)
	writeJSON(w, http.StatusOK, userView{ID: u.ID, Username: u.Username, Role: u.Role})
}

// logout revokes the current session and clears the cookie.
func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	if h.sessions != nil {
		if sid := auth.CookieValue(r); sid != "" {
			_ = h.sessions.Revoke(r.Context(), sid)
		}
	}
	auth.ClearSessionCookie(w, h.cfg.CookieSecure)
	w.WriteHeader(http.StatusNoContent)
}

// me returns the authenticated principal (used by the SPA to restore a session
// after a page refresh).
func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.FromContext(r.Context())
	if !ok {
		writeJSON(w, http.StatusUnauthorized, errBody("unauthorized"))
		return
	}
	writeJSON(w, http.StatusOK, userView{ID: p.UserID, Username: p.Username, Role: p.Role})
}

// listUsers returns all accounts (admin only).
func (h *Handler) listUsers(w http.ResponseWriter, r *http.Request) {
	if h.db == nil {
		writeJSON(w, http.StatusNotImplemented, errBody("accounts not enabled"))
		return
	}
	if p, ok := auth.FromContext(r.Context()); !ok || !p.IsAdmin() {
		writeJSON(w, http.StatusForbidden, errBody("admin only"))
		return
	}
	users, err := h.db.Users.List(r.Context())
	if err != nil {
		h.log.Error("list users", "err", err)
		writeJSON(w, http.StatusInternalServerError, errBody("list failed"))
		return
	}
	views := make([]userView, 0, len(users))
	for _, u := range users {
		views = append(views, userView{ID: u.ID, Username: u.Username, Role: u.Role})
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": views, "count": len(views)})
}

type createUserRequest struct {
	Username string  `json:"username"`
	Password string  `json:"password"`
	Role     pg.Role `json:"role"`
}

// createUser provisions a new account (admin only).
func (h *Handler) createUser(w http.ResponseWriter, r *http.Request) {
	if h.db == nil {
		writeJSON(w, http.StatusNotImplemented, errBody("accounts not enabled"))
		return
	}
	p, ok := auth.FromContext(r.Context())
	if !ok || !p.IsAdmin() {
		writeJSON(w, http.StatusForbidden, errBody("admin only"))
		return
	}
	var req createUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("invalid JSON"))
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	if len(req.Username) < 3 || len(req.Password) < 8 {
		writeJSON(w, http.StatusBadRequest, errBody("username must be >= 3 chars and password >= 8 chars"))
		return
	}
	if req.Role == "" {
		req.Role = pg.RoleOperator
	}
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errBody("hash failed"))
		return
	}
	u, err := h.db.Users.Create(r.Context(), req.Username, hash, req.Role)
	if err != nil {
		writeJSON(w, http.StatusConflict, errBody("could not create user (username taken?)"))
		return
	}
	h.audit(r, p.UserID, "user.create", u.Username, map[string]any{"role": string(u.Role)})
	writeJSON(w, http.StatusCreated, userView{ID: u.ID, Username: u.Username, Role: u.Role})
}

// audit is a best-effort audit-log helper (no-op when the DB is absent).
func (h *Handler) audit(r *http.Request, userID, action, target string, detail map[string]any) {
	if h.db == nil {
		return
	}
	_ = h.db.Audit.Log(r.Context(), userID, action, target, detail)
}

// clientIP returns a validated client IP (X-Forwarded-For first hop, else the
// RemoteAddr host), or "" when it cannot be parsed as an IP.
func clientIP(r *http.Request) string {
	cand := ""
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		cand = xff
		if i := strings.IndexByte(xff, ','); i > 0 {
			cand = xff[:i]
		}
		cand = strings.TrimSpace(cand)
	} else if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		cand = host
	} else {
		cand = r.RemoteAddr
	}
	if net.ParseIP(cand) == nil {
		return ""
	}
	return cand
}
