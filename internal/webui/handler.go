// Package webui serves the accountless management UI. It has no dependency on
// the gateway's storage, credentials or quota implementation: the browser uses
// the same HTTP API as any other client.
package webui

import (
	"embed"
	"net/http"
	"strconv"
)

//go:embed assets/*
var assets embed.FS

var routes = map[string]struct{ file, contentType string }{
	"/":                  {"assets/index.html", "text/html; charset=utf-8"},
	"/assets/app.css":    {"assets/app.css", "text/css; charset=utf-8"},
	"/assets/app.js":     {"assets/app.js", "text/javascript; charset=utf-8"},
	"/assets/api.js":     {"assets/api.js", "text/javascript; charset=utf-8"},
	"/assets/confirm.js": {"assets/confirm.js", "text/javascript; charset=utf-8"},
	"/assets/icon.svg":   {"assets/icon.svg", "image/svg+xml"},
	"/assets/logo.png":   {"assets/logo.png", "image/png"},
}

// Handler composes the embedded UI with an existing API handler. Only known
// asset paths are served here; API routes and unknown paths retain their
// existing behavior. Assets are embedded in the executable, including Docker.
func Handler(api http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asset, ok := routes[r.URL.Path]
		if !ok || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
			api.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
		if r.URL.RawQuery != "" {
			http.Error(w, "Query parameters are not supported.", http.StatusBadRequest)
			return
		}
		data, err := assets.ReadFile(asset.file)
		if err != nil {
			http.Error(w, "UI unavailable.", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", asset.contentType)
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write(data)
		}
	})
}
