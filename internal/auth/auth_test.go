package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/yourorg/sandbox-platform/internal/store/pg"
)

func TestHashPassword_VerifyRoundTrip(t *testing.T) {
	const pw = "Admin@123456"
	hash, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if hash == pw {
		t.Fatal("hash must not equal the plaintext")
	}
	if !VerifyPassword(hash, pw) {
		t.Fatal("VerifyPassword rejected the correct password")
	}
	if VerifyPassword(hash, "wrong-password") {
		t.Fatal("VerifyPassword accepted a wrong password")
	}
}

func TestHashPassword_UniqueSaltsPerHash(t *testing.T) {
	const pw = "same-password"
	h1, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("hash 1: %v", err)
	}
	h2, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("hash 2: %v", err)
	}
	if h1 == h2 {
		t.Fatal("two hashes of the same password must differ (per-hash salt)")
	}
	// Both must still verify.
	if !VerifyPassword(h1, pw) || !VerifyPassword(h2, pw) {
		t.Fatal("both salted hashes must verify against the password")
	}
}

func TestVerifyPassword_GarbageHash(t *testing.T) {
	if VerifyPassword("not-a-bcrypt-hash", "anything") {
		t.Fatal("a malformed hash must never verify")
	}
}

func TestConstantTimeEqual(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"token", "token", true},
		{"token", "Token", false},
		{"token", "tokenX", false},
		{"token", "toke", false},
		{"", "", true},
		{"", "x", false},
	}
	for _, c := range cases {
		if got := ConstantTimeEqual(c.a, c.b); got != c.want {
			t.Errorf("ConstantTimeEqual(%q,%q)=%v want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestPrincipal_IsAdmin(t *testing.T) {
	cases := []struct {
		name string
		p    Principal
		want bool
	}{
		{"admin role", Principal{Role: pg.RoleAdmin}, true},
		{"service token", Principal{Service: true, Role: pg.RoleOperator}, true},
		{"operator", Principal{Role: pg.RoleOperator}, false},
		{"empty", Principal{}, false},
	}
	for _, c := range cases {
		if got := c.p.IsAdmin(); got != c.want {
			t.Errorf("%s: IsAdmin()=%v want %v", c.name, got, c.want)
		}
	}
}

func TestPrincipalContext_RoundTrip(t *testing.T) {
	ctx := WithPrincipal(t.Context(), Principal{UserID: "u1", Username: "alice", Role: pg.RoleOperator})
	p, ok := FromContext(ctx)
	if !ok {
		t.Fatal("FromContext returned ok=false after WithPrincipal")
	}
	if p.UserID != "u1" || p.Username != "alice" || p.Role != pg.RoleOperator {
		t.Fatalf("principal round-trip mismatch: %+v", p)
	}
	if _, ok := FromContext(t.Context()); ok {
		t.Fatal("FromContext on a bare context must return ok=false")
	}
}

func TestSessionCookie_Attributes(t *testing.T) {
	rec := httptest.NewRecorder()
	exp := time.Now().Add(time.Hour)
	SetSessionCookie(rec, "sess-abc", exp, true)

	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("expected 1 cookie, got %d", len(cookies))
	}
	c := cookies[0]
	if c.Name != CookieName || c.Value != "sess-abc" {
		t.Fatalf("cookie name/value wrong: %s=%s", c.Name, c.Value)
	}
	if !c.HttpOnly {
		t.Error("session cookie must be HttpOnly (XSS cannot read it)")
	}
	if !c.Secure {
		t.Error("Secure must be set when secure=true")
	}
	if c.SameSite != http.SameSiteStrictMode {
		t.Error("session cookie must be SameSite=Strict (CSRF mitigation)")
	}
	if c.Path != "/" {
		t.Errorf("cookie path = %q, want /", c.Path)
	}
}

func TestSessionCookie_SecureFalseForDev(t *testing.T) {
	rec := httptest.NewRecorder()
	SetSessionCookie(rec, "s", time.Now().Add(time.Hour), false)
	if rec.Result().Cookies()[0].Secure {
		t.Error("Secure must be unset when secure=false (plain-HTTP dev)")
	}
}

func TestCookieValue_ReadsBackWhatWasSet(t *testing.T) {
	rec := httptest.NewRecorder()
	SetSessionCookie(rec, "round-trip-id", time.Now().Add(time.Hour), false)

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, c := range rec.Result().Cookies() {
		r.AddCookie(c)
	}
	if got := CookieValue(r); got != "round-trip-id" {
		t.Fatalf("CookieValue = %q, want round-trip-id", got)
	}

	// A request with no cookie returns "".
	if got := CookieValue(httptest.NewRequest(http.MethodGet, "/", nil)); got != "" {
		t.Fatalf("CookieValue with no cookie = %q, want empty", got)
	}
}

func TestClearSessionCookie_Expires(t *testing.T) {
	rec := httptest.NewRecorder()
	ClearSessionCookie(rec, false)
	c := rec.Result().Cookies()[0]
	if c.Name != CookieName || c.Value != "" || c.MaxAge >= 0 {
		t.Fatalf("clear cookie must blank the value and set MaxAge<0: %+v", c)
	}
}
