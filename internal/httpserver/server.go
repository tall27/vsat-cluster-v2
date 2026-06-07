// Package httpserver wires the web app: routing, the auth/config gates, HTTPS,
// and the handlers for login/setup, container lifecycle and the web terminal.
package httpserver

import (
	"context"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/tall27/vsat-cluster-v2/internal/auth"
	"github.com/tall27/vsat-cluster-v2/internal/config"
	"github.com/tall27/vsat-cluster-v2/internal/lxdctl"
	"github.com/tall27/vsat-cluster-v2/internal/webterm"
	"github.com/tall27/vsat-cluster-v2/web"
)

// Options configures the Server.
type Options struct {
	Store         *config.Store
	LXD           *lxdctl.Client
	LXCBin        string
	Sudo          bool
	Host          string // display label, e.g. the public IP/DNS
	SecureCookies bool
	Logger        *log.Logger
}

// Server holds runtime state and handlers.
type Server struct {
	store    *config.Store
	lxd      *lxdctl.Client
	tmpl     *templates
	term     *webterm.Handler
	host     string
	secureCk bool
	logger   *log.Logger

	mu       sync.RWMutex
	cfg      *config.Config
	sessions *auth.SessionManager
}

// New builds a Server, loading config if it already exists.
func New(opts Options) (*Server, error) {
	tmpl, err := loadTemplates()
	if err != nil {
		return nil, err
	}
	lxcBin := opts.LXCBin
	if lxcBin == "" {
		lxcBin = "lxc"
	}
	s := &Server{
		store:    opts.Store,
		lxd:      opts.LXD,
		tmpl:     tmpl,
		host:     opts.Host,
		secureCk: opts.SecureCookies,
		logger:   opts.Logger,
	}
	if s.logger == nil {
		s.logger = log.Default()
	}
	s.term = webterm.NewHandler(func(ctx context.Context, container string) *exec.Cmd {
		args := s.lxd.ShellArgs(container)
		var cmd *exec.Cmd
		if opts.Sudo {
			cmd = exec.CommandContext(ctx, "sudo", append([]string{"-n", lxcBin}, args...)...)
		} else {
			cmd = exec.CommandContext(ctx, lxcBin, args...)
		}
		// systemd services start with no controlling terminal, so TERM is usually
		// unset; `lxc exec` only forwards TERM into the container if it has one to
		// forward. Without it the in-container shell has no terminfo entry and
		// cursor-addressing tools (e.g. `vsatctl preflight`) fail outright.
		cmd.Env = append(os.Environ(), "TERM=xterm-256color")
		return cmd
	})
	// Load existing config if present.
	if cfg, err := opts.Store.Load(); err == nil {
		s.applyConfig(cfg)
	}
	return s, nil
}

// applyConfig swaps in a new config and derived session manager.
func (s *Server) applyConfig(cfg *config.Config) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg = cfg
	s.sessions = auth.NewSessionManager(cfg.SessionSecret, 12*time.Hour, s.secureCk)
}

func (s *Server) currentConfig() *config.Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

func (s *Server) sessionManager() *auth.SessionManager {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sessions
}

func (s *Server) isConfigured() bool { return s.currentConfig() != nil }

// Handler returns the fully-wired HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	staticFS, _ := fs.Sub(web.Static, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", cacheStatic(http.FileServer(http.FS(staticFS)))))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })

	mux.HandleFunc("GET /setup", s.handleSetupForm)
	mux.HandleFunc("POST /setup", s.handleSetupSubmit)
	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.handleLoginSubmit)
	mux.HandleFunc("POST /logout", s.handleLogout)

	mux.Handle("GET /{$}", s.protected(http.HandlerFunc(s.handleDashboard)))
	mux.Handle("GET /partials/containers", s.protected(http.HandlerFunc(s.handleContainerFragment)))
	mux.Handle("POST /containers", s.protected(http.HandlerFunc(s.handleAdd)))
	mux.Handle("POST /containers/{name}/delete", s.protected(http.HandlerFunc(s.handleRemove)))
	mux.Handle("GET /vsat/{name}/terminal", s.protected(http.HandlerFunc(s.handleTerminalPage)))
	mux.Handle("GET /vsat/{name}/terminal/ws", s.protected(http.HandlerFunc(s.handleTerminalWS)))

	return s.withConfigGate(mux)
}

// protected gates a handler behind a valid session.
func (s *Server) protected(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sm := s.sessionManager()
		if sm == nil {
			http.Redirect(w, r, "/setup", http.StatusSeeOther)
			return
		}
		sm.RequireSession(next).ServeHTTP(w, r)
	})
}

// withConfigGate redirects to /setup until the app is configured, and away from
// /setup once it is. Static assets and health checks are always allowed.
func (s *Server) withConfigGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if strings.HasPrefix(p, "/static/") || p == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		configured := s.isConfigured()
		switch {
		case !configured && p != "/setup":
			http.Redirect(w, r, "/setup", http.StatusSeeOther)
		case configured && p == "/setup":
			http.Redirect(w, r, "/", http.StatusSeeOther)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

func cacheStatic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		next.ServeHTTP(w, r)
	})
}
