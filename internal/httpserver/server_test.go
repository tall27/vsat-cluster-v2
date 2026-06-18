package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tall27/vsat-cluster-v2/internal/config"
	"github.com/tall27/vsat-cluster-v2/internal/lxdctl"
	"github.com/tall27/vsat-cluster-v2/internal/metrics"
)

type stubRunner struct{ json string }

func (s stubRunner) Run(_ context.Context, args ...string) ([]byte, error) {
	if len(args) > 0 && args[0] == "list" {
		return []byte(s.json), nil
	}
	return []byte(""), nil
}

func newTestServer(t *testing.T, metricCollector *metrics.Collector) *Server {
	t.Helper()
	lxd := lxdctl.New(lxdctl.Options{Runner: stubRunner{json: "[]"}, Max: 4, Prefix: "vsat"})
	srv, err := New(Options{
		Store:         config.NewStore(t.TempDir()),
		LXD:           lxd,
		Host:          "test-host",
		SecureCookies: false,
		Metrics:       metricCollector,
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	return srv
}

func setupSessionRequest(t *testing.T, srv *Server) []*http.Cookie {
	t.Helper()
	form := url.Values{"password": {"hunter2hunter2"}, "confirm": {"hunter2hunter2"}}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, postForm("/setup", form))
	return rec.Result().Cookies()
}

type testRunner struct {
	outputs [][]byte
	calls   int
}

func (r *testRunner) Run(_ context.Context, _ ...string) ([]byte, error) {
	if len(r.outputs) == 0 {
		return []byte{}, nil
	}
	if r.calls >= len(r.outputs) {
		return r.outputs[len(r.outputs)-1], nil
	}
	out := r.outputs[r.calls]
	r.calls++
	return out, nil
}

func TestUnconfiguredRedirectsToSetup(t *testing.T) {
	h := newTestServer(t, nil).Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/setup" {
		t.Fatalf("expected redirect to /setup, got %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestSetupThenDashboard(t *testing.T) {
	h := newTestServer(t, nil).Handler()

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
	srv := newTestServer(t, nil)
	// Configure it directly.
	t.Helper()
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

func TestMonitoringPageRequiresSession(t *testing.T) {
	srv := newTestServer(t, nil)
	// Configure so config-gate passes and session check is exercised.
	if err := srv.store.Save(&config.Config{PasswordHash: "x", SessionSecret: []byte("0123456789abcdef0123456789abcdef")}); err != nil {
		t.Fatal(err)
	}
	cfg, _ := srv.store.Load()
	srv.applyConfig(cfg)
	h := srv.Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/monitoring", nil))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Fatalf("expected redirect to /login, got %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestMonitoringDataRequiresSession(t *testing.T) {
	srv := newTestServer(t, nil)
	if err := srv.store.Save(&config.Config{PasswordHash: "x", SessionSecret: []byte("0123456789abcdef0123456789abcdef")}); err != nil {
		t.Fatal(err)
	}
	cfg, _ := srv.store.Load()
	srv.applyConfig(cfg)

	h := srv.Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/monitoring/data", nil))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Fatalf("expected redirect to /login, got %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestInstallRequiresAPIKey(t *testing.T) {
	srv := newTestServer(t, nil)
	cookies := setupSessionRequest(t, srv)
	req := postForm("/containers/vsat-a/install", url.Values{"protocol": {"ccm"}})
	for _, cookie := range cookies {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "API key is required") {
		t.Fatalf("expected API key error, got %q", rec.Body.String())
	}
}

func TestMonitoringDataReturnsHostAndContainerRows(t *testing.T) {
	c := metrics.NewCollector(metrics.Options{
		ListFn: func(_ context.Context) ([]metrics.ContainerInfo, error) {
			return []metrics.ContainerInfo{
				{Name: "vsat-a", Status: "Running"},
				{Name: "vsat-b", Status: "Stopped"},
			}, nil
		},
		ReadFile: func(name string) ([]byte, error) {
			return map[string][]byte{
				"/proc/stat":    []byte("cpu  100 100 100 100 100 100 0 0 0 0 0\n"),
				"/proc/meminfo": []byte("MemTotal:       1024 kB\nMemAvailable:  512 kB\n"),
				"/proc/diskstats": []byte(`
8 0 sda 1 0 2 0 0 0 4 0 0 0 0 0 0
`),
				"/proc/net/dev": []byte(`
Inter-|   Receive                                                | Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
 eth0:    10      0    0    0    0    0     0          0         20     0    0    0    0    0    0       0
`),
			}[name], nil
		},
		Runner: &testRunner{},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)
	time.Sleep(20 * time.Millisecond)
	srv := newTestServer(t, c)
	cookies := setupSessionRequest(t, srv)

	req := httptest.NewRequest(http.MethodGet, "/monitoring/data", nil)
	for _, cookie := range cookies {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var payload struct {
		Rows []metrics.UtilizationRow `json:"rows"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(payload.Rows) < 3 {
		t.Fatalf("expected host+containers, got %d rows", len(payload.Rows))
	}
	if payload.Rows[0].Name != "host" || payload.Rows[0].Type != "Host" {
		t.Fatalf("expected host row first, got %+v", payload.Rows[0])
	}
	if payload.Rows[1].Name != "vsat-a" || payload.Rows[2].Name != "vsat-b" {
		t.Fatalf("expected container rows, got %+v", payload.Rows)
	}
}

func postForm(path string, form url.Values) *http.Request {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}
