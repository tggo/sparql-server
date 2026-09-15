// Package cli implements the sparql-server command line: the serve, load,
// query and version subcommands.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/tggo/goRDFlib/endpoint"
	"github.com/tggo/sparql-server/internal/config"
	"github.com/tggo/sparql-server/internal/server"
	"github.com/tggo/sparql-server/internal/storage"
	"github.com/tggo/sparql-server/internal/version"
)

// Env is the process environment of one invocation.
type Env struct {
	Stdin    io.Reader
	Stdout   io.Writer
	Stderr   io.Writer
	Environ  []string
	ReadFile func(string) ([]byte, error)
	// Server hooks, for tests.
	ServerOptions server.Options
}

// Exit codes.
const (
	ExitOK    = 0
	ExitError = 1
	ExitUsage = 2
)

const usage = `sparql-server is a SPARQL 1.1 endpoint in a single binary.

Usage:
  sparql-server [serve] [flags]          run the server (the default)
  sparql-server load [flags] <files...>  load RDF files into a persistent store
  sparql-server query [flags] <query>    run a query against a store and print the result
  sparql-server version                  print version information

Run "sparql-server <command> -h" for the flags of a command. Every flag can
also be set with an environment variable: --update-token is
SPARQL_SERVER_UPDATE_TOKEN, and repeatable flags take a comma-separated list.
`

// Main runs the command line and returns the exit code.
func Main(ctx context.Context, args []string, env Env) int {
	if env.ReadFile == nil {
		env.ReadFile = os.ReadFile
	}
	if env.Stdin == nil {
		env.Stdin = strings.NewReader("")
	}
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "serve":
		return runServe(ctx, args, env)
	case "load":
		return runLoad(ctx, args, env)
	case "query":
		return runQuery(ctx, args, env)
	case "version", "--version":
		fmt.Fprint(env.Stdout, version.Long())
		return ExitOK
	case "help":
		fmt.Fprint(env.Stdout, usage)
		return ExitOK
	}
	fmt.Fprintf(env.Stderr, "unknown command %q\n\n%s", cmd, usage)
	return ExitUsage
}

// parse parses args into fs, applies the environment and reports unknown
// SPARQL_SERVER_* variables. done is true when the command should exit with
// code (help was requested or parsing failed).
func parse(fs *flag.FlagSet, args []string, env Env, synopsis string) (code int, done bool) {
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: %s\n\nFlags (environment variable in brackets):\n", synopsis)
		fs.VisitAll(func(f *flag.Flag) {
			def := ""
			if f.DefValue != "" && f.DefValue != "false" && f.DefValue != "[]" {
				def = fmt.Sprintf(" (default %s)", f.DefValue)
			}
			fmt.Fprintf(fs.Output(), "  --%s  [%s]\n      %s%s\n", f.Name, config.EnvName(f.Name), f.Usage, def)
		})
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK, true
		}
		return ExitUsage, true
	}
	unknown, err := config.ApplyEnv(fs, env.Environ, config.KnownEnv())
	for _, u := range unknown {
		fmt.Fprintf(env.Stderr, "warning: environment variable %s is not a sparql-server setting\n", u)
	}
	if err != nil {
		fmt.Fprintf(env.Stderr, "error: %v\n", err)
		return ExitUsage, true
	}
	return 0, false
}

func newLogger(w io.Writer, format, level string) *slog.Logger {
	var lv slog.Level
	_ = lv.UnmarshalText([]byte(level))
	o := &slog.HandlerOptions{Level: lv}
	if format == "json" {
		return slog.New(slog.NewJSONHandler(w, o))
	}
	return slog.New(slog.NewTextHandler(w, o))
}

func usageError(w io.Writer, err error) int {
	fmt.Fprintf(w, "error: %s\n", strings.ReplaceAll(err.Error(), "\n", "\nerror: "))
	return ExitUsage
}

func runServe(ctx context.Context, args []string, env Env) int {
	var cfg config.Serve
	fs := config.NewServeFlags(&cfg, env.Stderr)
	if code, done := parse(fs, args, env, "sparql-server serve [flags]"); done {
		return code
	}
	if fs.NArg() > 0 {
		return usageError(env.Stderr, fmt.Errorf("serve takes no arguments (got %q); use --data for files", fs.Args()))
	}
	if err := cfg.Validate(env.ReadFile); err != nil {
		return usageError(env.Stderr, err)
	}
	log := newLogger(env.Stderr, cfg.LogFormat, cfg.LogLevel)
	if err := server.Run(ctx, &cfg, log, env.ServerOptions); err != nil {
		log.Error("server failed", "err", err)
		return ExitError
	}
	return ExitOK
}

func runLoad(ctx context.Context, args []string, env Env) int {
	var cfg config.Load
	fs := config.NewLoadFlags(&cfg, env.Stderr)
	if code, done := parse(fs, args, env, "sparql-server load --store badger:/dir|sqlite:/file.db [--graph <iri>=<file>] [files, directories or globs...]"); done {
		return code
	}
	if err := cfg.Validate(fs.Args()); err != nil {
		return usageError(env.Stderr, err)
	}
	log := newLogger(env.Stderr, cfg.LogFormat, cfg.LogLevel)
	st, err := storage.Open(cfg.Store)
	if err != nil {
		log.Error("opening the store", "err", err)
		return ExitError
	}
	start := time.Now()
	results, loadErr := storage.Load(ctx, st, cfg.Files, cfg.Graphs, cfg.BaseIRI, log)
	total := 0
	for _, r := range results {
		total += r.Statements
	}
	triples := st.TripleCount()
	if err := st.Close(); err != nil && loadErr == nil {
		loadErr = fmt.Errorf("closing the store: %w", err)
	}
	fmt.Fprintf(env.Stdout, "loaded %d statements from %d files in %s; %s now holds %d triples\n",
		total, len(results), time.Since(start).Round(time.Millisecond), cfg.Store, triples)
	if loadErr != nil {
		log.Error("load failed", "err", loadErr)
		return ExitError
	}
	return ExitOK
}

func runQuery(ctx context.Context, args []string, env Env) int {
	var cfg config.Query
	fs := config.NewQueryFlags(&cfg, env.Stderr)
	if code, done := parse(fs, args, env, "sparql-server query [--store ...] [--data ...] [--format auto|json|xml|csv|tsv|turtle|...] <query | --file q.rq | ->"); done {
		return code
	}
	if err := cfg.Validate(fs.Args(), env.Stdin, env.ReadFile); err != nil {
		return usageError(env.Stderr, err)
	}
	log := newLogger(env.Stderr, cfg.LogFormat, cfg.LogLevel)
	if cfg.Store.Persistent() {
		if _, err := os.Stat(cfg.Store.Path); err != nil {
			log.Error("the store does not exist", "store", cfg.Store.String(), "err", err)
			return ExitError
		}
	}
	st, err := storage.Open(cfg.Store)
	if err != nil {
		log.Error("opening the store", "err", err)
		return ExitError
	}
	defer st.Close()
	if len(cfg.Data) > 0 || len(cfg.Graphs) > 0 {
		quiet := newLogger(io.Discard, "text", "error")
		if _, err := storage.Load(ctx, st, cfg.Data, cfg.Graphs, cfg.BaseIRI, quiet); err != nil {
			log.Error("loading data", "err", err)
			return ExitError
		}
	}

	opts := []endpoint.Option{endpoint.WithReadOnly(), endpoint.WithQueryTimeout(cfg.Timeout), endpoint.WithMaxRequestBytes(0)}
	if cfg.BaseIRI != "" {
		opts = append(opts, endpoint.WithBaseIRI(cfg.BaseIRI))
	}
	h := endpoint.NewForStore(st.Dataset, opts...)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://localhost/sparql", strings.NewReader(cfg.Text))
	if err != nil {
		log.Error("building the request", "err", err)
		return ExitError
	}
	req.RequestURI = "/sparql"
	req.Header.Set("Content-Type", "application/sparql-query")
	req.Header.Set("Accept", config.QueryFormats[cfg.Format])

	w := &cliWriter{header: http.Header{}, stdout: env.Stdout, stderr: env.Stderr}
	h.ServeHTTP(w, req)
	if w.status >= 300 {
		if w.status == http.StatusNotAcceptable {
			fmt.Fprintf(env.Stderr, "hint: --format %s does not fit this query form (SELECT/ASK take json, xml, csv, tsv; CONSTRUCT takes turtle, ntriples, nquads, trig, jsonld, rdfxml)\n", cfg.Format)
		}
		return ExitError
	}
	return ExitOK
}

// cliWriter is an http.ResponseWriter that streams a successful body to
// stdout and an error body to stderr.
type cliWriter struct {
	header http.Header
	status int
	stdout io.Writer
	stderr io.Writer
}

func (w *cliWriter) Header() http.Header { return w.header }

func (w *cliWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
}

func (w *cliWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.status >= 300 {
		return w.stderr.Write(p)
	}
	return w.stdout.Write(p)
}
