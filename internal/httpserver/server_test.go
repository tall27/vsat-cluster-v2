package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
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

type recordingRunner struct {
	mu         sync.Mutex
	listJSON   string
	failScript string
	calls      [][]string
}

func (r *recordingRunner) Run(_ context.Context, args ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	copied := append([]string(nil), args...)
	r.calls = append(r.calls, copied)
	if len(args) > 0 && args[0] == "list" {
		return []byte(r.listJSON), nil
	}
	if r.failScript != "" && len(args) > 0 && args[0] == "exec" && strings.Contains(strings.Join(args, " "), r.failScript) {
		return nil, errors.New("uninstall failed")
	}
	return []byte(""), nil
}

func (r *recordingRunner) snapshot() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]string, len(r.calls))
	for i, call := range r.calls {
		out[i] = append([]string(nil), call...)
	}
	return out
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

func TestRemoveUninstallsBeforeDeletingContainer(t *testing.T) {
	runner := &recordingRunner{listJSON: `[{"name":"vsat-a","status":"Running","state":{"network":{}}}]`}
	srv := newTestServerWithRunner(t, runner)
	cookies := setupSessionRequest(t, srv)

	req := postForm("/containers/vsat-a/delete", nil)
	for _, cookie := range cookies {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected redirect after remove, got %d", rec.Code)
	}

	calls := runner.snapshot()
	uninstallIdx, deleteIdx := -1, -1
	for i, call := range calls {
		joined := strings.Join(call, " ")
		if len(call) >= 2 && call[0] == "exec" && call[1] == "vsat-a" && strings.Contains(joined, "vsatctl uninstall --silent --install-dir /opt/vsatellite") {
			uninstallIdx = i
		}
		if len(call) == 3 && call[0] == "delete" && call[1] == "--force" && call[2] == "vsat-a" {
			deleteIdx = i
		}
	}
	if uninstallIdx < 0 {
		t.Fatalf("expected vsatctl uninstall before delete, calls: %+v", calls)
	}
	if deleteIdx < 0 {
		t.Fatalf("expected lxc delete after uninstall, calls: %+v", calls)
	}
	if uninstallIdx > deleteIdx {
		t.Fatalf("expected uninstall before delete, calls: %+v", calls)
	}
}

func TestRemoveBlocksContainerDeleteWhenUninstallFails(t *testing.T) {
	runner := &recordingRunner{
		listJSON:   `[{"name":"vsat-a","status":"Running","state":{"network":{}}}]`,
		failScript: "vsatctl uninstall",
	}
	srv := newTestServerWithRunner(t, runner)
	cookies := setupSessionRequest(t, srv)

	req := postForm("/containers/vsat-a/delete", nil)
	for _, cookie := range cookies {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected dashboard render on uninstall failure, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Could not uninstall VSatellite before removing container") {
		t.Fatalf("expected uninstall error in dashboard, got %q", rec.Body.String())
	}
	for _, call := range runner.snapshot() {
		if len(call) == 3 && call[0] == "delete" && call[1] == "--force" && call[2] == "vsat-a" {
			t.Fatalf("delete should not run after uninstall failure, calls: %+v", runner.snapshot())
		}
	}
}

func TestRemoveRejectsInvalidContainerNameBeforeUninstall(t *testing.T) {
	runner := &recordingRunner{listJSON: `[]`}
	srv := newTestServerWithRunner(t, runner)
	cookies := setupSessionRequest(t, srv)

	req := postForm("/containers/not-vsat/delete", nil)
	for _, cookie := range cookies {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected dashboard render for invalid name, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Invalid container name") {
		t.Fatalf("expected invalid name error, got %q", rec.Body.String())
	}
	for _, call := range runner.snapshot() {
		if len(call) >= 2 && call[0] == "exec" && call[1] == "not-vsat" {
			t.Fatalf("invalid remove should not exec into requested name, calls: %+v", runner.snapshot())
		}
		if len(call) == 3 && call[0] == "delete" && call[2] == "not-vsat" {
			t.Fatalf("invalid remove should not delete requested name, calls: %+v", runner.snapshot())
		}
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

func newTestServerWithRunner(t *testing.T, runner lxdctl.CommandRunner) *Server {
	t.Helper()
	lxd := lxdctl.New(lxdctl.Options{Runner: runner, Max: 4, Prefix: "vsat"})
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

func postForm(path string, form url.Values) *http.Request {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}
