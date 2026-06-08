package httpserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/tall27/vsat-cluster-v2/internal/auth"
	"github.com/tall27/vsat-cluster-v2/internal/config"
	"github.com/tall27/vsat-cluster-v2/internal/lxdctl"
	"github.com/tall27/vsat-cluster-v2/internal/metrics"
)

// pageData is the unified view model passed to templates.
type pageData struct {
	ShowNav bool
	Host    string
	Error   string

	// dashboard / container fragment
	Containers    []lxdctl.Container
	Used          int
	Max           int
	CanAdd        bool
	Prefix        string
	SuggestedName string

	// terminal
	Name string
}

func (s *Server) render(w http.ResponseWriter, page string, data pageData) {
	tmpl, ok := s.tmpl.pages[page]
	if !ok {
		http.Error(w, "template not found", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, "layout", data); err != nil {
		s.logger.Printf("render %s: %v", page, err)
	}
}

// --- setup ---------------------------------------------------------------

func (s *Server) handleSetupForm(w http.ResponseWriter, _ *http.Request) {
	s.render(w, "setup", pageData{})
}

func (s *Server) handleSetupSubmit(w http.ResponseWriter, r *http.Request) {
	password := r.FormValue("password")
	confirm := r.FormValue("confirm")
	if len(password) < 8 {
		s.render(w, "setup", pageData{Error: "Password must be at least 8 characters."})
		return
	}
	if password != confirm {
		s.render(w, "setup", pageData{Error: "Passwords do not match."})
		return
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		s.render(w, "setup", pageData{Error: "Could not hash password."})
		return
	}
	secret, err := config.NewSessionSecret()
	if err != nil {
		s.render(w, "setup", pageData{Error: "Could not generate session secret."})
		return
	}
	cfg := &config.Config{
		PasswordHash:   hash,
		SessionSecret:  secret,
		InstancePrefix: s.lxd.Prefix,
		MaxContainers:  s.lxd.Max,
	}
	if err := s.store.Save(cfg); err != nil {
		s.render(w, "setup", pageData{Error: "Could not save config: " + err.Error()})
		return
	}
	s.applyConfig(cfg)
	s.sessionManager().Issue(w)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// --- login / logout ------------------------------------------------------

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	if sm := s.sessionManager(); sm != nil && sm.Valid(r) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, "login", pageData{})
}

func (s *Server) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	cfg := s.currentConfig()
	if cfg == nil {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	if !auth.VerifyPassword(cfg.PasswordHash, r.FormValue("password")) {
		// Constant-ish delay to blunt brute force.
		time.Sleep(400 * time.Millisecond)
		s.render(w, "login", pageData{Error: "Incorrect password."})
		return
	}
	s.sessionManager().Issue(w)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if sm := s.sessionManager(); sm != nil {
		sm.Clear(w)
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// --- dashboard / containers ---------------------------------------------

func (s *Server) buildContainerView(r *http.Request) (pageData, error) {
	containers, err := s.lxd.List(r.Context())
	if err != nil {
		return pageData{}, err
	}
	used := len(containers)
	return pageData{
		ShowNav:       true,
		Host:          s.host,
		Containers:    containers,
		Used:          used,
		Max:           s.lxd.Max,
		CanAdd:        used < s.lxd.Max,
		Prefix:        s.lxd.Prefix,
		SuggestedName: s.suggestName(containers),
	}, nil
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	data, err := s.buildContainerView(r)
	if err != nil {
		s.render(w, "dashboard", pageData{ShowNav: true, Host: s.host, Max: s.lxd.Max, Error: "Could not list containers: " + err.Error()})
		return
	}
	s.render(w, "dashboard", data)
}

func (s *Server) handleContainerFragment(w http.ResponseWriter, r *http.Request) {
	data, err := s.buildContainerView(r)
	if err != nil {
		http.Error(w, "list error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.pages["_containers"].ExecuteTemplate(w, "containers", data); err != nil {
		s.logger.Printf("render fragment: %v", err)
	}
}

func (s *Server) handleAdd(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	if err := s.lxd.Add(r.Context(), name); err != nil {
		data, berr := s.buildContainerView(r)
		if berr != nil {
			data = pageData{ShowNav: true, Host: s.host, Max: s.lxd.Max}
		}
		data.Error = "Could not add container: " + err.Error()
		s.render(w, "dashboard", data)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleRemove(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.lxd.Remove(r.Context(), name); err != nil {
		data, berr := s.buildContainerView(r)
		if berr != nil {
			data = pageData{ShowNav: true, Host: s.host, Max: s.lxd.Max}
		}
		data.Error = "Could not remove container: " + err.Error()
		s.render(w, "dashboard", data)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// --- terminal ------------------------------------------------------------

func (s *Server) handleTerminalPage(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.lxd.ValidateName(name); err != nil {
		http.Error(w, "invalid container name", http.StatusBadRequest)
		return
	}
	s.render(w, "terminal", pageData{ShowNav: true, Host: s.host, Name: name})
}

func (s *Server) handleTerminalWS(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.lxd.ValidateName(name); err != nil {
		http.Error(w, "invalid container name", http.StatusBadRequest)
		return
	}
	s.term.ServeWS(w, r, name)
}

// --- monitoring ----------------------------------------------------------

func (s *Server) handleMonitoringPage(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.lxd.ValidateName(name); err != nil {
		http.Error(w, "invalid container name", http.StatusBadRequest)
		return
	}
	s.render(w, "monitoring", pageData{ShowNav: true, Host: s.host, Name: name})
}

func (s *Server) handleMonitoringData(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.lxd.ValidateName(name); err != nil {
		http.Error(w, "invalid container name", http.StatusBadRequest)
		return
	}
	snap, ok := s.metrics.Snapshot(name)
	w.Header().Set("Content-Type", "application/json")
	if !ok {
		// Container exists but the collector hasn't derived a rate yet
		// (needs two polls) — return empty series rather than an error so
		// the chart can render its "waiting for data" state.
		json.NewEncoder(w).Encode(struct {
			Ready bool `json:"ready"`
		}{false})
		return
	}
	json.NewEncoder(w).Encode(struct {
		Ready bool `json:"ready"`
		metrics.Snapshot
	}{true, snap})
}

// suggestName proposes the next free vsat-<letter> name.
func (s *Server) suggestName(containers []lxdctl.Container) string {
	used := make(map[string]bool, len(containers))
	for _, c := range containers {
		used[c.Name] = true
	}
	letters := "abcdefghijklmnopqrstuvwxyz"
	for _, ch := range letters {
		candidate := fmt.Sprintf("%s-%c", s.lxd.Prefix, ch)
		if !used[candidate] {
			return candidate
		}
	}
	return s.lxd.Prefix + "-new"
}
