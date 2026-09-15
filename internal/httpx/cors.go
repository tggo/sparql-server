package httpx

import (
	"net/http"
	"strings"
	"sync/atomic"
)

// CORS allows cross-origin requests from the given origins. An origin
// matches exactly (scheme://host[:port]); "*" alone allows every origin.
//
// Access-Control-Allow-Credentials is never sent: tokens travel in the
// Authorization header, which a script sets explicitly and which does not
// need credentialed requests. That is also what makes "*" safe to offer.
//
// A preflight from an allowed origin is answered here, before
// authentication, as the CORS protocol requires. Requests from other origins
// pass through without CORS headers, so the browser blocks them.
func CORS(origins []string, next http.Handler) http.Handler {
	if len(origins) == 0 {
		return next
	}
	any := len(origins) == 1 && origins[0] == "*"
	allowed := make(map[string]bool, len(origins))
	for _, o := range origins {
		allowed[strings.TrimSuffix(o, "/")] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" {
			next.ServeHTTP(w, r)
			return
		}
		h := w.Header()
		h.Add("Vary", "Origin")
		if !any && !allowed[origin] {
			next.ServeHTTP(w, r)
			return
		}
		if any {
			h.Set("Access-Control-Allow-Origin", "*")
		} else {
			h.Set("Access-Control-Allow-Origin", origin)
		}
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			h.Add("Vary", "Access-Control-Request-Method")
			h.Add("Vary", "Access-Control-Request-Headers")
			h.Set("Access-Control-Allow-Methods", "GET, HEAD, POST, PUT, DELETE, OPTIONS")
			h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Accept, Accept-Encoding")
			h.Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.Set("Access-Control-Expose-Headers", "Location, WWW-Authenticate, Content-Disposition")
		next.ServeHTTP(w, r)
	})
}

// Gate answers 503 with Retry-After until ready is set.
func Gate(ready *atomic.Bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !ready.Load() {
			w.Header().Set("Retry-After", "5")
			w.Header().Set("Cache-Control", "no-store")
			http.Error(w, "the server is still loading its data", http.StatusServiceUnavailable)
			return
		}
		next.ServeHTTP(w, r)
	})
}
