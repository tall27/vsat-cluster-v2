// Package web embeds the HTML templates and static assets for the web app.
package web

import "embed"

// Templates holds the html/template sources.
//
//go:embed templates
var Templates embed.FS

// Static holds CSS/JS assets served under /static/.
//
//go:embed static
var Static embed.FS
