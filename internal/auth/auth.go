// Package auth implements operator authentication for the control plane:
// bcrypt password hashing, Postgres-backed login sessions, an opaque session
// cookie (HttpOnly, Secure, SameSite=Strict), and a request-context principal.
//
// The cookie carries an opaque session id — never the API token — so the hard
// rule "the API token is never written to browser storage" is preserved while
// still letting a refresh stay logged in.
package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"

	"github.com/yourorg/sandbox-platform/internal/store/pg"
)

// bcryptCost is the work factor for password hashing (>= 12 recommended).
const bcryptCost = 12

// CookieName is the session cookie key.
const CookieName = "sid"

// ErrInvalidCredentials is returned for a bad username/password.
var ErrInvalidCredentials = errors.New("auth: invalid credentials")

// HashPassword returns a bcrypt hash of plain.
func HashPassword(plain string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(plain), bcryptCost)
	if err != nil {
		return "", fmt.Errorf("auth: hash: %w", err)
	}
	return string(b), nil
}

// VerifyPassword reports whether plain matches the bcrypt hash. bcrypt's compare
// is itself constant-time with respect to the hash.
func VerifyPassword(hash, plain string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
}

// ConstantTimeEqual compares two strings without leaking length-independent
// timing (used for the bearer API/service token).
func ConstantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// Principal is the authenticated identity attached to a request context.
type Principal struct {
	UserID   string
	Username string
	Role     pg.Role
	Service  bool // authenticated via the bearer API_TOKEN (CLI/automation)
}

// IsAdmin reports whether the principal may perform admin-only actions.
func (p Principal) IsAdmin() bool { return p.Service || p.Role == pg.RoleAdmin }

type ctxKey struct{}

// WithPrincipal returns a context carrying p.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// FromContext extracts the principal placed by the auth middleware.
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}

// Sessions manages login sessions backed by Postgres.
type Sessions struct {
	repo *pg.SessionRepo
	ttl  time.Duration
}

// NewSessions returns a session manager with the given TTL (default 24h).
func NewSessions(repo *pg.SessionRepo, ttl time.Duration) *Sessions {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return &Sessions{repo: repo, ttl: ttl}
}

// TTL returns the configured session lifetime.
func (s *Sessions) TTL() time.Duration { return s.ttl }

// Create issues a new session for userID and returns the opaque id + expiry.
func (s *Sessions) Create(ctx context.Context, userID, userAgent, ip string) (string, time.Time, error) {
	id := uuid.NewString()
	exp := time.Now().Add(s.ttl)
	if err := s.repo.Create(ctx, id, userID, exp, userAgent, ip); err != nil {
		return "", time.Time{}, err
	}
	return id, exp, nil
}

// Resolve returns the (non-disabled) user behind a session id, or pg.ErrNotFound.
func (s *Sessions) Resolve(ctx context.Context, id string) (pg.User, error) {
	return s.repo.Resolve(ctx, id)
}

// Revoke deletes a session (logout).
func (s *Sessions) Revoke(ctx context.Context, id string) error {
	return s.repo.Revoke(ctx, id)
}

// SetSessionCookie writes the session cookie. secure should be true in
// production (HTTPS); it may be false for plain-HTTP local development.
func SetSessionCookie(w http.ResponseWriter, id string, expires time.Time, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    id,
		Path:     "/",
		Expires:  expires,
		MaxAge:   int(time.Until(expires).Seconds()),
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
	})
}

// ClearSessionCookie expires the session cookie (logout).
func ClearSessionCookie(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
	})
}

// CookieValue returns the session id from the request cookie, or "".
func CookieValue(r *http.Request) string {
	c, err := r.Cookie(CookieName)
	if err != nil {
		return ""
	}
	return c.Value
}
