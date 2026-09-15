// Package ui serves the embedded query page: one HTML file, one script and
// one stylesheet, no external resources, under a strict Content-Security-Policy.
package ui

import (
	"embed"
	"net/http"
)

//go:embed static
var static embed.FS

var files = map[string]struct{ name, contentType string }{
	"/":           {"static/index.html", "text/html; charset=utf-8"},
	"/ui/app.js":  {"static/app.js", "text/javascript; charset=utf-8"},
	"/ui/app.css": {"static/app.css", "text/css; charset=utf-8"},
}

// CSP allows only the page's own script and stylesheet and requests to its
// own origin.
const CSP = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; " +
	"base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// Handler serves "/", "/ui/app.js" and "/ui/app.css".
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		b, err := static.ReadFile(f.name)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		h := w.Header()
		h.Set("Content-Type", f.contentType)
		h.Set("Content-Security-Policy", CSP)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-cache")
		if r.Method == http.MethodHead {
			return
		}
		_, _ = w.Write(b)
	})
}
