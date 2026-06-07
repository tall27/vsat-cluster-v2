package httpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/tall27/vsat-cluster-v2/internal/config"
	"github.com/tall27/vsat-cluster-v2/internal/lxdctl"
)

type stubRunner struct{ json string }

func (s stubRunner) Run(_ context.Context, args ...string) ([]byte, error) {
	if len(args) > 0 && args[0] == "list" {
		return []byte(s.json), nil
	}
	return []byte(""), nil
}

func newTestServer(t *testing.T) *Server {
	t.Helper()
	lxd := lxdctl.New(lxdctl.Options{Runner: stubRunner{json: "[]"}, Max: 4, Prefix: "vsat"})
	srv, err := New(Options{
		Store:         config.NewStore(t.TempDir()),
		LXD:           lxd,
		Host:          "test-host",
		SecureCookies: false,
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	return srv
}

func TestUnconfiguredRedirectsToSetup(t *testing.T) {
	h := newTestServer(t).Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/setup" {
		t.Fatalf("expected redirect to /setup, got %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestSetupThenDashboard(t *testing.T) {
	h := newTestServer(t).Handler()

	// Complete setup.
	form := url.Values{"password": {"hunter2hunter2"}, "confirm": {"hunter2hunter2"}}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postForm("/setup", form))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("setup: expected 303, got %d", rec.Code)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("setup did not issue a session cookie")
	}

	// Dashboard with the session cookie.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusOK {
		t.Fatalf("dashboard: expected 200, got %d", rec2.Code)
	}
	if !strings.Contains(rec2.Body.String(), "VSatellites") {
		t.Error("dashboard body missing expected content")
	}
}

func TestDashboardRequiresSession(t *testing.T) {
	srv := newTestServer(t)
	// Configure it directly.
	if err := srv.store.Save(&config.Config{PasswordHash: "x", SessionSecret: []byte("0123456789abcdef0123456789abcdef")}); err != nil {
		t.Fatal(err)
	}
	cfg, _ := srv.store.Load()
	srv.applyConfig(cfg)

	h := srv.Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Fatalf("expected redirect to /login, got %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

func postForm(path string, form url.Values) *http.Request {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}
