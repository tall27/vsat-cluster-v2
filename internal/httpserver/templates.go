package httpserver

import (
	"fmt"
	"html/template"

	"github.com/tall27/vsat-cluster-v2/web"
)

// templates holds one parsed template set per page, each combining the shared
// layout + partials with the page body.
type templates struct {
	pages map[string]*template.Template
}

func loadTemplates() (*templates, error) {
	shared := []string{"templates/layout.html", "templates/partials.html"}
	pages := map[string]string{
		"login":      "templates/login.html",
		"setup":      "templates/setup.html",
		"dashboard":  "templates/dashboard.html",
		"terminal":   "templates/terminal.html",
		"monitoring": "templates/monitoring.html",
	}
	out := &templates{pages: make(map[string]*template.Template, len(pages))}
	for name, file := range pages {
		files := append(append([]string{}, shared...), file)
		tmpl, err := template.New("layout").ParseFS(web.Templates, files...)
		if err != nil {
			return nil, fmt.Errorf("parse template %s: %w", name, err)
		}
		out.pages[name] = tmpl
	}
	// Standalone fragment for htmx polling (no layout).
	frag, err := template.New("containers").ParseFS(web.Templates, "templates/partials.html")
	if err != nil {
		return nil, fmt.Errorf("parse partials: %w", err)
	}
	out.pages["_containers"] = frag
	return out, nil
}
