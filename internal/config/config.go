// Package config defines the command-line flags of every subcommand, their
// environment variable equivalents and the validation of the result.
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// StoreKind is a storage backend.
type StoreKind string

// The storage backends.
const (
	StoreMemory StoreKind = "memory"
	StoreBadger StoreKind = "badger"
	StoreSQLite StoreKind = "sqlite"
)

// StoreSpec is a parsed --store value: "memory", "badger:/dir" or
// "sqlite:/file.db".
type StoreSpec struct {
	Kind StoreKind
	Path string
}

func (s StoreSpec) String() string {
	if s.Kind == StoreMemory || s.Kind == "" {
		return string(StoreMemory)
	}
	return string(s.Kind) + ":" + s.Path
}

// Persistent reports whether the store survives a restart.
func (s StoreSpec) Persistent() bool { return s.Kind == StoreBadger || s.Kind == StoreSQLite }

// ParseStoreSpec parses a --store value. The path is everything after the
// first colon, so Windows paths (badger:C:\data) work.
func ParseStoreSpec(v string) (StoreSpec, error) {
	v = strings.TrimSpace(v)
	if v == "" || v == "memory" {
		return StoreSpec{Kind: StoreMemory}, nil
	}
	kind, path, ok := strings.Cut(v, ":")
	switch StoreKind(kind) {
	case StoreBadger, StoreSQLite:
		if !ok || strings.TrimSpace(path) == "" {
			return StoreSpec{}, fmt.Errorf("store %q needs a path, e.g. %s:/var/lib/sparql-server/data", v, kind)
		}
		return StoreSpec{Kind: StoreKind(kind), Path: path}, nil
	case StoreMemory:
		return StoreSpec{}, fmt.Errorf("store %q: the memory store takes no path", v)
	}
	return StoreSpec{}, fmt.Errorf("unknown store %q (want memory, badger:/path or sqlite:/path)", v)
}

// GraphFile is a parsed --graph value: a file loaded into a named graph.
type GraphFile struct {
	IRI  string
	Path string
}

// ParseGraphFile parses "iri=path". The path starts after the last "=",
// because an IRI is more likely than a file name to contain one.
func ParseGraphFile(v string) (GraphFile, error) {
	i := strings.LastIndexByte(v, '=')
	if i <= 0 || i == len(v)-1 {
		return GraphFile{}, fmt.Errorf("graph %q: want <iri>=<file>", v)
	}
	gf := GraphFile{IRI: strings.TrimSpace(v[:i]), Path: strings.TrimSpace(v[i+1:])}
	if err := checkAbsoluteIRI(gf.IRI); err != nil {
		return GraphFile{}, fmt.Errorf("graph %q: %w", v, err)
	}
	return gf, nil
}

func checkAbsoluteIRI(s string) error {
	u, err := url.Parse(s)
	if err != nil || u.Scheme == "" {
		return fmt.Errorf("%q is not an absolute IRI", s)
	}
	return nil
}

// Common holds the flags shared by serve, load and query.
type Common struct {
	StoreRaw  string
	Store     StoreSpec
	Data      []string
	GraphsRaw []string
	Graphs    []GraphFile
	BaseIRI   string
	LogFormat string
	LogLevel  string
}

func (c *Common) register(fs *flag.FlagSet, withData bool) {
	fs.StringVar(&c.StoreRaw, "store", "memory", "storage: memory, badger:/dir or sqlite:/file.db")
	if withData {
		listFlag(fs, &c.Data, "data", "RDF file, directory or glob to load at startup (repeatable); N-Quads and TriG keep their named graphs")
	}
	listFlag(fs, &c.GraphsRaw, "graph", "load a file into a named graph: <iri>=<file> (repeatable)")
	fs.StringVar(&c.BaseIRI, "base-iri", "", "base IRI for relative IRIs in loaded files, queries and updates")
	fs.StringVar(&c.LogFormat, "log-format", "text", "log format: text or json")
	fs.StringVar(&c.LogLevel, "log-level", "info", "log level: debug, info, warn or error")
}

func (c *Common) validate() error {
	var errs []error
	st, err := ParseStoreSpec(c.StoreRaw)
	if err != nil {
		errs = append(errs, err)
	}
	c.Store = st
	c.Graphs = nil
	for _, raw := range c.GraphsRaw {
		gf, err := ParseGraphFile(raw)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		c.Graphs = append(c.Graphs, gf)
	}
	if c.BaseIRI != "" {
		if err := checkAbsoluteIRI(c.BaseIRI); err != nil {
			errs = append(errs, fmt.Errorf("base-iri: %w", err))
		}
	}
	switch c.LogFormat {
	case "text", "json":
	default:
		errs = append(errs, fmt.Errorf("log-format %q: want text or json", c.LogFormat))
	}
	switch strings.ToLower(c.LogLevel) {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("log-level %q: want debug, info, warn or error", c.LogLevel))
	}
	return errors.Join(errs...)
}

// Serve is the configuration of the serve subcommand.
type Serve struct {
	Common

	Listen    string
	PublicURL string
	Dataset   string

	UpdateToken           string
	UpdateTokenFile       string
	QueryToken            string
	QueryTokenFile        string
	AllowAnonymousUpdates bool

	AllowLoad    []string
	LoadTimeout  time.Duration
	LoadMaxBytes int64

	CORSOrigins []string
	TLSCert     string
	TLSKey      string

	QueryTimeout    time.Duration
	UpdateTimeout   time.Duration
	MaxRequestBytes int64
	MaxGraphBytes   int64
	MaxResultRows   int

	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	MaxHeaderBytes    int64
	ShutdownTimeout   time.Duration

	Gzip      bool
	Metrics   bool
	UI        bool
	AccessLog bool
}

// Defaults that are referenced by tests and documentation.
const (
	DefaultListen          = ":8080"
	DefaultQueryTimeout    = 30 * time.Second
	DefaultUpdateTimeout   = 60 * time.Second
	DefaultMaxRequestBytes = 10 << 20
	DefaultMaxGraphBytes   = 64 << 20
	DefaultShutdownTimeout = 30 * time.Second
)

// NewServeFlags returns the flag set of the serve subcommand bound to s.
func NewServeFlags(s *Serve, out io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(out)
	s.Common.register(fs, true)

	fs.StringVar(&s.Listen, "listen", DefaultListen, "address to listen on")
	fs.StringVar(&s.PublicURL, "public-url", "", "public base URL (scheme://host[:port]) when behind a reverse proxy; used for request IRIs")
	fs.StringVar(&s.Dataset, "dataset", "", "also serve Fuseki-style routes /<dataset>/query, /update, /data, /get")

	fs.StringVar(&s.UpdateToken, "update-token", "", "bearer token that enables SPARQL updates and Graph Store writes")
	fs.StringVar(&s.UpdateTokenFile, "update-token-file", "", "read the update token from this file")
	fs.StringVar(&s.QueryToken, "query-token", "", "bearer token required for queries and graph reads")
	fs.StringVar(&s.QueryTokenFile, "query-token-file", "", "read the query token from this file")
	fs.BoolVar(&s.AllowAnonymousUpdates, "allow-anonymous-updates", false, "allow updates and Graph Store writes without a token (local use only)")

	listFlag(fs, &s.AllowLoad, "allow-load", "enable SPARQL LOAD for URLs under this http(s) prefix (repeatable)")
	durationFlag(fs, &s.LoadTimeout, "load-timeout", 30*time.Second, "time limit for fetching one LOAD document")
	bytesFlag(fs, &s.LoadMaxBytes, "load-max-bytes", 64<<20, "size limit of one LOAD document")

	listFlag(fs, &s.CORSOrigins, "cors-origin", "allowed CORS origin, exact match, or * alone (repeatable)")
	fs.StringVar(&s.TLSCert, "tls-cert", "", "TLS certificate file (PEM)")
	fs.StringVar(&s.TLSKey, "tls-key", "", "TLS private key file (PEM)")

	durationFlag(fs, &s.QueryTimeout, "query-timeout", DefaultQueryTimeout, "time limit for one query (0 = none)")
	durationFlag(fs, &s.UpdateTimeout, "update-timeout", DefaultUpdateTimeout, "time limit for one update or Graph Store write (0 = none)")
	bytesFlag(fs, &s.MaxRequestBytes, "max-request-bytes", DefaultMaxRequestBytes, "size limit of a query or update request body")
	bytesFlag(fs, &s.MaxGraphBytes, "max-graph-bytes", DefaultMaxGraphBytes, "size limit of a Graph Store PUT or POST body")
	fs.IntVar(&s.MaxResultRows, "max-result-rows", 0, "fail (422) results with more rows or triples than this (0 = no limit)")

	durationFlag(fs, &s.ReadHeaderTimeout, "read-header-timeout", 10*time.Second, "time limit for reading request headers")
	durationFlag(fs, &s.ReadTimeout, "read-timeout", 5*time.Minute, "time limit for reading a whole request")
	durationFlag(fs, &s.WriteTimeout, "write-timeout", 10*time.Minute, "time limit for writing a response, counted from the end of the request headers")
	durationFlag(fs, &s.IdleTimeout, "idle-timeout", 2*time.Minute, "keep-alive idle timeout")
	bytesFlag(fs, &s.MaxHeaderBytes, "max-header-bytes", 64<<10, "size limit of request headers")
	durationFlag(fs, &s.ShutdownTimeout, "shutdown-timeout", DefaultShutdownTimeout, "how long to let running requests finish on SIGINT/SIGTERM")

	fs.BoolVar(&s.Gzip, "gzip", true, "gzip responses for clients that accept it")
	fs.BoolVar(&s.Metrics, "metrics", true, "serve Prometheus metrics at /metrics")
	fs.BoolVar(&s.UI, "ui", true, "serve the query page at /")
	fs.BoolVar(&s.AccessLog, "access-log", true, "log one line per SPARQL and Graph Store request")
	return fs
}

var datasetNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// reservedRoutes are top-level paths a --dataset name must not shadow.
var reservedRoutes = map[string]bool{
	"sparql": true, "graph-store": true, "healthz": true, "readyz": true, "metrics": true, "ui": true,
}

// Validate checks s, reads token files and fills the parsed fields. readFile
// is os.ReadFile outside tests.
func (s *Serve) Validate(readFile func(string) ([]byte, error)) error {
	if readFile == nil {
		readFile = os.ReadFile
	}
	errs := []error{s.Common.validate()}

	var err error
	if s.UpdateToken, err = tokenValue("update-token", s.UpdateToken, s.UpdateTokenFile, readFile); err != nil {
		errs = append(errs, err)
	}
	if s.QueryToken, err = tokenValue("query-token", s.QueryToken, s.QueryTokenFile, readFile); err != nil {
		errs = append(errs, err)
	}
	if s.UpdateToken != "" && s.QueryToken != "" && s.UpdateToken == s.QueryToken {
		errs = append(errs, errors.New("query-token and update-token must differ"))
	}
	if s.UpdateToken != "" && s.AllowAnonymousUpdates {
		errs = append(errs, errors.New("allow-anonymous-updates and update-token exclude each other"))
	}
	if len(s.AllowLoad) > 0 && !s.UpdatesEnabled() {
		errs = append(errs, errors.New("allow-load needs updates enabled (update-token or allow-anonymous-updates)"))
	}
	for _, p := range s.AllowLoad {
		u, err := url.Parse(p)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
			errs = append(errs, fmt.Errorf("allow-load %q: want an http:// or https:// URL prefix with a host and no user info", p))
		}
	}
	for _, o := range s.CORSOrigins {
		if o == "*" {
			if len(s.CORSOrigins) > 1 {
				errs = append(errs, errors.New("cors-origin * must be the only origin"))
			}
			continue
		}
		u, err := url.Parse(o)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" {
			errs = append(errs, fmt.Errorf("cors-origin %q: want scheme://host[:port]", o))
		}
	}
	if (s.TLSCert == "") != (s.TLSKey == "") {
		errs = append(errs, errors.New("tls-cert and tls-key must be given together"))
	}
	if s.PublicURL != "" {
		u, err := url.Parse(s.PublicURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" {
			errs = append(errs, fmt.Errorf("public-url %q: want scheme://host[:port][/path]", s.PublicURL))
		}
	}
	if s.Dataset != "" && (!datasetNameRe.MatchString(s.Dataset) || reservedRoutes[s.Dataset]) {
		errs = append(errs, fmt.Errorf("dataset %q: want letters, digits, '_', '.', '-' and not a built-in route", s.Dataset))
	}
	for name, d := range map[string]time.Duration{
		"query-timeout": s.QueryTimeout, "update-timeout": s.UpdateTimeout, "read-header-timeout": s.ReadHeaderTimeout,
		"read-timeout": s.ReadTimeout, "write-timeout": s.WriteTimeout, "idle-timeout": s.IdleTimeout,
		"shutdown-timeout": s.ShutdownTimeout, "load-timeout": s.LoadTimeout,
	} {
		if d < 0 {
			errs = append(errs, fmt.Errorf("%s must not be negative", name))
		}
	}
	if s.MaxResultRows < 0 {
		errs = append(errs, errors.New("max-result-rows must not be negative"))
	}
	if s.MaxHeaderBytes <= 0 || s.MaxHeaderBytes > 1<<30 {
		errs = append(errs, errors.New("max-header-bytes must be between 1 and 1GiB"))
	}
	return errors.Join(errs...)
}

// UpdatesEnabled reports whether updates and Graph Store writes are possible.
func (s *Serve) UpdatesEnabled() bool { return s.UpdateToken != "" || s.AllowAnonymousUpdates }

func tokenValue(name, direct, file string, readFile func(string) ([]byte, error)) (string, error) {
	if direct != "" && file != "" {
		return "", fmt.Errorf("%s and %s-file exclude each other", name, name)
	}
	if file != "" {
		b, err := readFile(file)
		if err != nil {
			return "", fmt.Errorf("%s-file: %w", name, err)
		}
		direct = strings.TrimRight(string(b), "\r\n")
		if direct == "" {
			return "", fmt.Errorf("%s-file %s is empty", name, file)
		}
	}
	if direct != "" && len(direct) < 16 {
		return "", fmt.Errorf("%s must be at least 16 characters", name)
	}
	if strings.ContainsAny(direct, " \t\r\n") {
		return "", fmt.Errorf("%s must not contain whitespace", name)
	}
	return direct, nil
}

// Load is the configuration of the load subcommand.
type Load struct {
	Common
	Files []string
}

// NewLoadFlags returns the flag set of the load subcommand bound to l.
func NewLoadFlags(l *Load, out io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("load", flag.ContinueOnError)
	fs.SetOutput(out)
	l.Common.register(fs, false)
	return fs
}

// Validate checks l after parsing; args are the positional arguments.
func (l *Load) Validate(args []string) error {
	l.Files = args
	errs := []error{l.Common.validate()}
	if l.Store.Kind == StoreMemory {
		errs = append(errs, errors.New("load needs a persistent store (--store badger:/dir or sqlite:/file.db); a memory store is gone when the command exits"))
	}
	if len(args) == 0 && len(l.Graphs) == 0 {
		errs = append(errs, errors.New("nothing to load: give files as arguments or --graph <iri>=<file>"))
	}
	return errors.Join(errs...)
}

// Query is the configuration of the query subcommand.
type Query struct {
	Common
	Format  string
	File    string
	Timeout time.Duration
	Text    string
}

// QueryFormats maps --format values of the query subcommand to media types.
var QueryFormats = map[string]string{
	"json":     "application/sparql-results+json",
	"xml":      "application/sparql-results+xml",
	"csv":      "text/csv",
	"tsv":      "text/tab-separated-values",
	"turtle":   "text/turtle",
	"ntriples": "application/n-triples",
	"nquads":   "application/n-quads",
	"trig":     "application/trig",
	"jsonld":   "application/ld+json",
	"rdfxml":   "application/rdf+xml",
	// auto: TSV for SELECT and ASK, Turtle for CONSTRUCT.
	"auto": "text/tab-separated-values, text/turtle;q=0.9",
}

// NewQueryFlags returns the flag set of the query subcommand bound to q.
func NewQueryFlags(q *Query, out io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("query", flag.ContinueOnError)
	fs.SetOutput(out)
	q.Common.register(fs, true)
	fs.StringVar(&q.Format, "format", "auto", "output format: auto, json, xml, csv, tsv, turtle, ntriples, nquads, trig, jsonld, rdfxml")
	fs.StringVar(&q.File, "file", "", "read the query from this file (- for stdin)")
	durationFlag(fs, &q.Timeout, "query-timeout", 0, "time limit for the query (0 = none)")
	return fs
}

// Validate checks q; args are the positional arguments (the query text, or
// "-" for stdin). stdin is read when the query comes from it.
func (q *Query) Validate(args []string, stdin io.Reader, readFile func(string) ([]byte, error)) error {
	if readFile == nil {
		readFile = os.ReadFile
	}
	errs := []error{q.Common.validate()}
	if _, ok := QueryFormats[q.Format]; !ok {
		errs = append(errs, fmt.Errorf("format %q is not one of auto, json, xml, csv, tsv, turtle, ntriples, nquads, trig, jsonld, rdfxml", q.Format))
	}
	src := q.File
	switch {
	case q.File != "" && len(args) > 0:
		errs = append(errs, errors.New("give the query either as an argument or with --file, not both"))
	case len(args) > 1:
		errs = append(errs, errors.New("the query must be a single argument; quote it"))
	case len(args) == 1 && args[0] == "-":
		src = "-"
	case len(args) == 1:
		q.Text = args[0]
	case q.File == "":
		errs = append(errs, errors.New("no query: give it as an argument, with --file, or - for stdin"))
	}
	if src == "-" {
		b, err := io.ReadAll(stdin)
		if err != nil {
			errs = append(errs, fmt.Errorf("reading the query from stdin: %w", err))
		}
		q.Text = string(b)
	} else if src != "" {
		b, err := readFile(src)
		if err != nil {
			errs = append(errs, fmt.Errorf("file: %w", err))
		}
		q.Text = string(b)
	}
	if strings.TrimSpace(q.Text) == "" && errors.Join(errs...) == nil {
		errs = append(errs, errors.New("the query is empty"))
	}
	if q.Timeout < 0 {
		errs = append(errs, errors.New("query-timeout must not be negative"))
	}
	return errors.Join(errs...)
}

// KnownEnv returns every environment variable any subcommand reads.
func KnownEnv() map[string]bool {
	known := make(map[string]bool)
	for _, fs := range []*flag.FlagSet{
		NewServeFlags(&Serve{}, io.Discard), NewLoadFlags(&Load{}, io.Discard), NewQueryFlags(&Query{}, io.Discard),
	} {
		fs.VisitAll(func(f *flag.Flag) { known[EnvName(f.Name)] = true })
	}
	return known
}
