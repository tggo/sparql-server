package loader

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tggo/goRDFlib/graph"
	"github.com/tggo/goRDFlib/term"
)

func TestAllowed(t *testing.T) {
	a, err := New([]string{"https://data.example/dumps/", "http://mirror.example:8080/rdf", "https://Whole.Example"}, 0, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string]bool{
		"https://data.example/dumps/a.ttl":                 true,
		"https://data.example:443/dumps/a.ttl":             true,
		"https://DATA.example/dumps/x/y.nt?v=1":            true,
		"https://data.example/dumps":                       false, // prefix ends with a slash
		"https://data.example/dumpster/a.ttl":              false,
		"https://data.example/other/a.ttl":                 false,
		"http://data.example/dumps/a.ttl":                  false, // scheme differs
		"https://data.example:8443/dumps/a.ttl":            false, // port differs
		"https://data.example.evil/dumps/a.ttl":            false,
		"https://evil.example/https://data.example/dumps/": false,
		"https://user@data.example/dumps/a.ttl":            false,
		"https://data.example/dumps/../secret":             false,
		"https://data.example/dumps/%2e%2e/secret":         false,
		"https://data.example/dumps/a%2Fb":                 false,
		"http://mirror.example:8080/rdf":                   true,
		"http://mirror.example:8080/rdf/x.ttl":             true,
		"http://mirror.example:8080/rdfx":                  false,
		"http://mirror.example/rdf/x.ttl":                  false,
		"https://whole.example/anything":                   true,
		"file:///etc/passwd":                               false,
		"/etc/passwd":                                      false,
		"data.example/dumps/a.ttl":                         false,
	}
	for u, want := range tests {
		if got := a.Allowed(u); got != want {
			t.Errorf("Allowed(%q) = %v, want %v", u, got, want)
		}
	}
	for _, bad := range []string{"file:///data/", "ftp://x/", "https://u:p@x/", "/local/"} {
		if _, err := New([]string{bad}, 0, 0, nil); err == nil {
			t.Errorf("New accepted prefix %q", bad)
		}
	}
}

const doc = `@prefix ex: <http://example.org/> . ex:a ex:p "x" . ex:b ex:p "y" .`

func newGraph() *graph.Graph {
	return graph.NewGraph(graph.WithIdentifier(term.NewURIRefUnsafe("http://example.org/g")))
}

func TestLoad(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/data/typed", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/turtle; charset=utf-8")
		w.Write([]byte(doc))
	})
	mux.HandleFunc("/data/by-extension.ttl", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write([]byte(doc))
	})
	mux.HandleFunc("/data/sniffed", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(`<http://example.org/a> <http://example.org/p> "x" .` + "\n"))
	})
	mux.HandleFunc("/data/redirect-inside", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/data/typed", http.StatusFound)
	})
	mux.HandleFunc("/data/redirect-outside", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/private/secret.ttl", http.StatusFound)
	})
	mux.HandleFunc("/private/secret.ttl", func(w http.ResponseWriter, r *http.Request) {
		t.Error("the loader fetched a URL outside the allowlist")
		w.Write([]byte(doc))
	})
	mux.HandleFunc("/data/big.nt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/n-triples")
		// No Content-Length: the limit must hold while streaming.
		w.(http.Flusher).Flush()
		for range 200 {
			w.Write([]byte(`<http://example.org/a> <http://example.org/p> "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" .` + "\n"))
		}
	})
	mux.HandleFunc("/data/slow.ttl", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	})
	mux.HandleFunc("/data/missing.ttl", http.NotFound)
	mux.HandleFunc("/data/remote-context.jsonld", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/ld+json")
		w.Write([]byte(`{"@context": "http://127.0.0.1:1/ctx.jsonld", "@id": "http://example.org/a", "name": "x"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	a, err := New([]string{srv.URL + "/data/"}, 300*time.Millisecond, 1024, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	for _, path := range []string{"/data/typed", "/data/by-extension.ttl", "/data/redirect-inside"} {
		g := newGraph()
		if err := a.Load(ctx, g, srv.URL+path); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if g.Len() != 2 {
			t.Errorf("%s: loaded %d triples, want 2", path, g.Len())
		}
	}
	g := newGraph()
	if err := a.Load(ctx, g, srv.URL+"/data/sniffed"); err != nil || g.Len() != 1 {
		t.Errorf("sniffed: %v, %d triples", err, g.Len())
	}

	fails := map[string]string{
		"/private/secret.ttl":         "allowlist",
		"/data/redirect-outside":      "allowlist",
		"/data/big.nt":                "size limit",
		"/data/slow.ttl":              "timed out",
		"/data/missing.ttl":           "HTTP 404",
		"/data/remote-context.jsonld": "remote JSON-LD contexts",
	}
	for path, want := range fails {
		g := newGraph()
		err := a.Load(ctx, g, srv.URL+path)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", path, err, want)
		}
		if g.Len() != 0 {
			t.Errorf("%s: a failed load added %d triples", path, g.Len())
		}
	}
	if err := a.Load(ctx, newGraph(), "file:///etc/passwd"); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("file URL: %v", err)
	}
}
