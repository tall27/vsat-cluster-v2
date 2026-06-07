package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHashAndVerifyPassword(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if !VerifyPassword(hash, "correct horse battery staple") {
		t.Error("expected matching password to verify")
	}
	if VerifyPassword(hash, "wrong password") {
		t.Error("expected wrong password to fail")
	}
}

func TestHashEmptyRejected(t *testing.T) {
	if _, err := HashPassword("   "); err == nil {
		t.Error("expected error for empty password")
	}
}

func TestSessionIssueAndValidate(t *testing.T) {
	m := NewSessionManager([]byte("super-secret-key"), time.Hour, false)

	rec := httptest.NewRecorder()
	m.Issue(rec)
	cookie := rec.Result().Cookies()[0]
	if cookie.Name != DefaultCookieName || cookie.Value == "" {
		t.Fatalf("unexpected cookie %+v", cookie)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)
	if !m.Valid(req) {
		t.Error("expected valid session")
	}
}

func TestSessionRejectsForgedAndExpired(t *testing.T) {
	m := NewSessionManager([]byte("secret"), time.Hour, false)

	// Forged value.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: DefaultCookieName, Value: "not-a-valid-token"})
	if m.Valid(req) {
		t.Error("forged token should be invalid")
	}

	// Wrong secret cannot validate a cookie from another manager.
	other := NewSessionManager([]byte("different-secret"), time.Hour, false)
	rec := httptest.NewRecorder()
	m.Issue(rec)
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.AddCookie(rec.Result().Cookies()[0])
	if other.Valid(req2) {
		t.Error("cookie signed with a different secret should be invalid")
	}

	// Expired session: craft a token whose expiry is in the past but signed with
	// the correct secret (sign is package-private, so build it directly).
	expiredToken := m.sign(time.Now().Add(-time.Hour))
	if m.verify(expiredToken) {
		t.Error("expired session should be invalid")
	}
}

func TestRequireSessionRedirectsAndAllows(t *testing.T) {
	m := NewSessionManager([]byte("secret"), time.Hour, false)
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	guarded := m.RequireSession(next)

	// No cookie -> redirect to /login.
	rec := httptest.NewRecorder()
	guarded.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Errorf("expected redirect to /login, got %d %q", rec.Code, rec.Header().Get("Location"))
	}

	// Valid cookie -> passes through.
	issue := httptest.NewRecorder()
	m.Issue(issue)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(issue.Result().Cookies()[0])
	rec2 := httptest.NewRecorder()
	guarded.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusTeapot {
		t.Errorf("expected pass-through (418), got %d", rec2.Code)
	}
}
