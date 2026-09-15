// Package server wires the goRDFlib endpoint, storage, authentication,
// metrics and middleware into an http.Server and runs it until its context
// ends.
package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tggo/goRDFlib/endpoint"
	"github.com/tggo/sparql-server/internal/auth"
	"github.com/tggo/sparql-server/internal/config"
	"github.com/tggo/sparql-server/internal/httpx"
	"github.com/tggo/sparql-server/internal/loader"
	"github.com/tggo/sparql-server/internal/metrics"
	"github.com/tggo/sparql-server/internal/storage"
	"github.com/tggo/sparql-server/internal/ui"
	"github.com/tggo/sparql-server/internal/version"
)

// Options are hooks for embedding and tests.
type Options struct {
	// OnListen is called with the listening address once the listener is open,
	// before data is loaded.
	OnListen func(addr string)
	// LoadTransport replaces the HTTP transport SPARQL LOAD fetches with.
	LoadTransport http.RoundTripper
}

// Run serves cfg until ctx is done, then shuts down gracefully: it stops
// accepting connections, lets running requests finish for
// cfg.ShutdownTimeout, cancels what is still running, and closes the store.
// It returns nil after a clean shutdown.
func Run(ctx context.Context, cfg *config.Serve, log *slog.Logger, opts Options) error {
	st, err := storage.Open(cfg.Store)
	if err != nil {
		return err
	}
	closeStore := sync.OnceValue(st.Close)
	defer closeStore()

	s := &state{cfg: cfg, log: log, st: st, reg: metrics.New(version.String())}
	h, err := s.handler(opts)
	if err != nil {
		return err
	}
	s.h = h
	s.triples = &tripleCounter{st: st, h: h, loaded: &s.loaded}
	s.triples.dirty.Store(true)
	s.reg.Ready = s.ready
	s.reg.Triples = s.triples.get

	baseCtx, cancelBase := context.WithCancel(context.Background())
	defer cancelBase()
	srv := &http.Server{
		Handler:           s.routes(),
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		MaxHeaderBytes:    int(cfg.MaxHeaderBytes),
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
		BaseContext:       func(net.Listener) context.Context { return baseCtx },
	}
	if cfg.TLSCert != "" {
		if _, err := tls.LoadX509KeyPair(cfg.TLSCert, cfg.TLSKey); err != nil {
			return fmt.Errorf("tls: %w", err)
		}
		srv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	s.logStartup(ln.Addr().String())
	if opts.OnListen != nil {
		opts.OnListen(ln.Addr().String())
	}

	serveErr := make(chan error, 1)
	go func() {
		if cfg.TLSCert != "" {
			serveErr <- srv.ServeTLS(ln, cfg.TLSCert, cfg.TLSKey)
		} else {
			serveErr <- srv.Serve(ln)
		}
	}()

	loadCtx, cancelLoad := context.WithCancel(ctx)
	defer cancelLoad()
	loadDone := make(chan error, 1)
	go func() { loadDone <- s.load(loadCtx) }()

	var runErr error
	loadPending := true
wait:
	for {
		select {
		case <-ctx.Done():
			log.Info("shutting down", "reason", context.Cause(ctx))
			break wait
		case err := <-serveErr:
			if !errors.Is(err, http.ErrServerClosed) {
				runErr = err
			}
			break wait
		case err := <-loadDone:
			loadPending = false
			if err != nil {
				runErr = err
				log.Error("loading data failed; shutting down", "err", err)
				break wait
			}
		}
	}

	// Shutdown.
	s.stopping.Store(true)
	cancelLoad()
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancelDrain()
	if err := srv.Shutdown(drainCtx); err != nil {
		log.Warn("requests still running after the shutdown timeout; cancelling them", "timeout", cfg.ShutdownTimeout)
		cancelBase()
		_ = srv.Close()
	}
	if loadPending {
		if err := <-loadDone; err != nil && runErr == nil && !errors.Is(err, context.Canceled) {
			runErr = err
		}
	}

	// Close the store under the write lock, so no evaluation is using it.
	lockCtx, cancelLock := context.WithTimeout(context.Background(), max(cfg.ShutdownTimeout, 5*time.Second))
	defer cancelLock()
	var closeErr error
	if err := h.Write(lockCtx, func() { closeErr = closeStore() }); err != nil {
		log.Warn("a request still holds the dataset; closing the store anyway", "err", err)
		closeErr = closeStore()
	}
	if closeErr != nil {
		log.Error("closing the store", "err", closeErr)
		if runErr == nil {
			runErr = closeErr
		}
	}
	if runErr == nil {
		log.Info("stopped")
	}
	return runErr
}

type state struct {
	cfg      *config.Serve
	log      *slog.Logger
	st       *storage.Store
	h        *endpoint.Handler
	reg      *metrics.Registry
	triples  *tripleCounter
	loaded   atomic.Bool
	stopping atomic.Bool
}

func (s *state) ready() bool { return s.loaded.Load() && !s.stopping.Load() }

func (s *state) handler(opts Options) (*endpoint.Handler, error) {
	cfg := s.cfg
	eo := []endpoint.Option{
		endpoint.WithLogger(s.log),
		endpoint.WithQueryTimeout(cfg.QueryTimeout),
		endpoint.WithUpdateTimeout(cfg.UpdateTimeout),
		endpoint.WithMaxRequestBytes(cfg.MaxRequestBytes),
		endpoint.WithMaxGraphBytes(cfg.MaxGraphBytes),
		endpoint.WithMaxResultRows(cfg.MaxResultRows),
		endpoint.WithAuthorizer(auth.Authorizer(auth.Config{
			QueryToken:            cfg.QueryToken,
			UpdateToken:           cfg.UpdateToken,
			AllowAnonymousUpdates: cfg.AllowAnonymousUpdates,
		})),
		endpoint.WithRequestHook(s.hook),
	}
	if !cfg.UpdatesEnabled() {
		eo = append(eo, endpoint.WithReadOnly())
	}
	if cfg.BaseIRI != "" {
		eo = append(eo, endpoint.WithBaseIRI(cfg.BaseIRI))
	}
	if cfg.PublicURL != "" {
		eo = append(eo, endpoint.WithRequestIRI(publicRequestIRI(cfg.PublicURL)))
	}
	if len(cfg.AllowLoad) > 0 {
		l, err := loader.New(cfg.AllowLoad, cfg.LoadTimeout, cfg.LoadMaxBytes, opts.LoadTransport)
		if err != nil {
			return nil, err
		}
		eo = append(eo, endpoint.WithLoader(l))
	}
	return endpoint.NewForStore(s.st.Dataset, eo...), nil
}

// publicRequestIRI rebuilds a request IRI from the configured public URL and
// the request path, for deployments behind a reverse proxy.
func publicRequestIRI(public string) func(*http.Request) string {
	public = strings.TrimSuffix(public, "/")
	return func(r *http.Request) string {
		p := r.URL.EscapedPath()
		if r.RequestURI != "" && strings.HasPrefix(r.RequestURI, "/") {
			p, _, _ = strings.Cut(r.RequestURI, "?")
		}
		return public + p
	}
}

func (s *state) hook(r *http.Request, info endpoint.RequestInfo) {
	if s.cfg.Metrics {
		s.reg.Observe(info)
	}
	if info.Op == endpoint.OpUpdate || info.Op == endpoint.OpGraphWrite {
		// Even a failed update can have applied some operations.
		s.triples.dirty.Store(true)
	}
	if !s.cfg.AccessLog {
		return
	}
	level := slog.LevelInfo
	if info.Status >= 500 && info.Status != endpoint.StatusClientClosedRequest {
		level = slog.LevelWarn
	}
	attrs := []slog.Attr{
		slog.String("method", info.Method),
		slog.String("path", requestPath(r)),
		slog.String("op", info.Op.String()),
		slog.Int("status", info.Status),
		slog.Float64("duration_ms", float64(info.Duration.Microseconds())/1000),
		slog.Int("rows", info.Rows),
		slog.Int64("bytes", info.Bytes),
		slog.String("remote", r.RemoteAddr),
	}
	if info.Err != nil {
		attrs = append(attrs, slog.String("err", info.Err.Error()))
	}
	s.log.LogAttrs(r.Context(), level, "request", attrs...)
}

// requestPath is the path as the client sent it; http.StripPrefix rewrites
// r.URL.Path but not RequestURI.
func requestPath(r *http.Request) string {
	if strings.HasPrefix(r.RequestURI, "/") {
		p, _, _ := strings.Cut(r.RequestURI, "?")
		return p
	}
	return r.URL.Path
}

func (s *state) routes() http.Handler {
	cfg := s.cfg
	h := s.h
	gate := func(next http.Handler) http.Handler { return s.reg.Track(httpx.Gate(&s.loaded, next)) }
	gsp := func(prefix string) http.Handler { return gate(http.StripPrefix(prefix, h.GraphStoreHandler())) }

	mux := http.NewServeMux()
	mux.Handle("/sparql", gate(h))
	mux.Handle("/graph-store", gsp("/graph-store"))
	mux.Handle("/graph-store/", gsp("/graph-store"))

	if ds := cfg.Dataset; ds != "" {
		p := "/" + ds
		mux.Handle(p, gate(h))
		mux.Handle(p+"/sparql", gate(h.QueryHandler()))
		mux.Handle(p+"/query", gate(h.QueryHandler()))
		mux.Handle(p+"/update", gate(h.UpdateHandler()))
		mux.Handle(p+"/data", gsp(p+"/data"))
		mux.Handle(p+"/data/", gsp(p+"/data"))
		mux.Handle(p+"/get", readOnly(gsp(p+"/get")))
	}

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		switch {
		case s.stopping.Load():
			http.Error(w, "shutting down", http.StatusServiceUnavailable)
		case !s.loaded.Load():
			http.Error(w, "loading data", http.StatusServiceUnavailable)
		default:
			_, _ = w.Write([]byte("ready\n"))
		}
	})
	if cfg.Metrics {
		mux.Handle("/metrics", s.reg)
	}
	if cfg.UI {
		u := ui.Handler()
		mux.Handle("GET /{$}", u)
		mux.Handle("GET /ui/", u)
	}

	var root http.Handler = mux
	if cfg.Gzip {
		root = httpx.Gzip(root)
	}
	return httpx.CORS(cfg.CORSOrigins, root)
}

// readOnly allows only GET and HEAD (Fuseki's /get route).
func readOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			w.Header().Set("Allow", "GET, HEAD, OPTIONS")
			http.Error(w, "this route only reads graphs", http.StatusMethodNotAllowed)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *state) load(ctx context.Context) error {
	cfg := s.cfg
	start := time.Now()
	if len(cfg.Data) > 0 || len(cfg.Graphs) > 0 {
		var loadErr error
		if err := s.h.Write(ctx, func() {
			_, loadErr = storage.Load(ctx, s.st, cfg.Data, cfg.Graphs, cfg.BaseIRI, s.log)
		}); err != nil {
			return err
		}
		if loadErr != nil {
			return loadErr
		}
	}
	s.loaded.Store(true)
	n, _ := s.triples.get()
	s.log.Info("ready", "triples", n, "load_duration", time.Since(start).Round(time.Millisecond))
	return nil
}

func (s *state) logStartup(addr string) {
	cfg := s.cfg
	scheme := "http"
	if cfg.TLSCert != "" {
		scheme = "https"
	}
	writes := "disabled (read-only)"
	switch {
	case cfg.UpdateToken != "":
		writes = "enabled with token"
	case cfg.AllowAnonymousUpdates:
		writes = "enabled WITHOUT authentication"
	}
	s.log.Info("listening",
		"addr", addr, "scheme", scheme, "version", version.String(), "store", cfg.Store.String(),
		"updates", writes, "query_token", cfg.QueryToken != "", "load", len(cfg.AllowLoad) > 0,
		"cors", strings.Join(cfg.CORSOrigins, ","), "gzip", cfg.Gzip)
	if cfg.AllowAnonymousUpdates {
		s.log.Warn("--allow-anonymous-updates: anyone who can reach this server can change or delete all data; use it only on a trusted local machine")
	}
	if (cfg.UpdateToken != "" || cfg.QueryToken != "") && cfg.TLSCert == "" && !isLoopback(addr) {
		s.log.Warn("tokens travel in clear text without TLS; terminate TLS here (--tls-cert/--tls-key) or in a proxy in front")
	}
	if cfg.WriteTimeout > 0 && cfg.QueryTimeout >= cfg.WriteTimeout {
		s.log.Warn("write-timeout is not longer than query-timeout; slow queries will lose their connection before they time out",
			"write_timeout", cfg.WriteTimeout, "query_timeout", cfg.QueryTimeout)
	}
	if cfg.Store.Persistent() && len(cfg.Data) > 0 {
		s.log.Info("--data is loaded into the persistent store on every start; triples are deduplicated but blank nodes are not, so prefer `sparql-server load` once")
	}
}

func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// tripleCounter caches the dataset size. Counting scans an index on the
// persistent stores, so it is recomputed only after a write.
type tripleCounter struct {
	st     *storage.Store
	h      *endpoint.Handler
	loaded *atomic.Bool
	dirty  atomic.Bool

	mu    sync.Mutex
	n     int
	valid bool
}

func (t *tripleCounter) get() (int, bool) {
	if !t.loaded.Load() {
		return 0, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.dirty.Swap(false) || !t.valid {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var n int
		if err := t.h.Read(ctx, func() { n = t.st.TripleCount() }); err != nil {
			t.dirty.Store(true)
			return t.n, t.valid
		}
		t.n, t.valid = n, true
	}
	return t.n, t.valid
}
