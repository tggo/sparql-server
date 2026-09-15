// Package metrics keeps request counters and writes them in the Prometheus
// text exposition format (version 0.0.4). It is hand-written to keep the
// binary free of the client_golang dependency tree; it covers only what the
// server reports.
package metrics

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"net/http"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tggo/goRDFlib/endpoint"
)

// Buckets are the upper bounds of the request duration histogram, in seconds.
var Buckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}

type reqKey struct {
	op     string
	status int
}

type histogram struct {
	counts []uint64 // per bucket, not cumulative; last is +Inf
	sum    float64
	count  uint64
}

// Registry holds the server's metrics. It is safe for concurrent use.
type Registry struct {
	mu        sync.Mutex
	requests  map[reqKey]uint64
	durations map[string]*histogram
	rows      map[string]uint64
	bytes     map[string]uint64

	inFlight atomic.Int64
	start    time.Time

	// Gauges read at scrape time.
	Ready   func() bool
	Triples func() (int, bool)
	Version string
}

// New returns an empty registry.
func New(version string) *Registry {
	return &Registry{
		requests:  make(map[reqKey]uint64),
		durations: make(map[string]*histogram),
		rows:      make(map[string]uint64),
		bytes:     make(map[string]uint64),
		start:     time.Now(),
		Version:   version,
	}
}

// Observe records one finished endpoint request.
func (m *Registry) Observe(info endpoint.RequestInfo) {
	op := info.Op.String()
	secs := info.Duration.Seconds()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests[reqKey{op, info.Status}]++
	h := m.durations[op]
	if h == nil {
		h = &histogram{counts: make([]uint64, len(Buckets)+1)}
		m.durations[op] = h
	}
	i := sort.SearchFloat64s(Buckets, secs)
	h.counts[i]++
	h.sum += secs
	h.count++
	if info.Rows > 0 && info.Status < 400 {
		// A failed request (a 422 over the row limit) returned nothing.
		m.rows[op] += uint64(info.Rows)
	}
	if info.Bytes > 0 {
		m.bytes[op] += uint64(info.Bytes)
	}
}

// Track wraps next so that the in-flight gauge counts its requests.
func (m *Registry) Track(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.inFlight.Add(1)
		defer m.inFlight.Add(-1)
		next.ServeHTTP(w, r)
	})
}

// ServeHTTP writes the exposition.
func (m *Registry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodHead {
		return
	}
	_ = m.Write(w)
}

// Write writes the exposition to out.
func (m *Registry) Write(out io.Writer) error {
	bw := bufio.NewWriter(out)
	p := func(format string, args ...any) { fmt.Fprintf(bw, format, args...) }

	m.mu.Lock()
	keys := make([]reqKey, 0, len(m.requests))
	for k := range m.requests {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].op != keys[j].op {
			return keys[i].op < keys[j].op
		}
		return keys[i].status < keys[j].status
	})
	p("# HELP sparql_server_requests_total SPARQL and Graph Store requests by operation and HTTP status.\n")
	p("# TYPE sparql_server_requests_total counter\n")
	for _, k := range keys {
		p("sparql_server_requests_total{op=%q,status=\"%d\"} %d\n", k.op, k.status, m.requests[k])
	}

	ops := sortedKeys(m.durations)
	p("# HELP sparql_server_request_duration_seconds Time from receiving a request to finishing its response.\n")
	p("# TYPE sparql_server_request_duration_seconds histogram\n")
	for _, op := range ops {
		h := m.durations[op]
		var cum uint64
		for i, b := range Buckets {
			cum += h.counts[i]
			p("sparql_server_request_duration_seconds_bucket{op=%q,le=%q} %d\n", op, formatFloat(b), cum)
		}
		cum += h.counts[len(Buckets)]
		p("sparql_server_request_duration_seconds_bucket{op=%q,le=\"+Inf\"} %d\n", op, cum)
		p("sparql_server_request_duration_seconds_sum{op=%q} %s\n", op, formatFloat(h.sum))
		p("sparql_server_request_duration_seconds_count{op=%q} %d\n", op, h.count)
	}

	p("# HELP sparql_server_result_rows_total Solutions or triples returned (or parsed from Graph Store bodies).\n")
	p("# TYPE sparql_server_result_rows_total counter\n")
	for _, op := range sortedKeys(m.rows) {
		p("sparql_server_result_rows_total{op=%q} %d\n", op, m.rows[op])
	}
	p("# HELP sparql_server_response_bytes_total Response body bytes before compression.\n")
	p("# TYPE sparql_server_response_bytes_total counter\n")
	for _, op := range sortedKeys(m.bytes) {
		p("sparql_server_response_bytes_total{op=%q} %d\n", op, m.bytes[op])
	}
	m.mu.Unlock()

	p("# HELP sparql_server_in_flight_requests Requests being served.\n")
	p("# TYPE sparql_server_in_flight_requests gauge\n")
	p("sparql_server_in_flight_requests %d\n", m.inFlight.Load())

	if m.Ready != nil {
		p("# HELP sparql_server_ready Whether startup data loading finished and the server is not shutting down.\n")
		p("# TYPE sparql_server_ready gauge\n")
		p("sparql_server_ready %d\n", b2i(m.Ready()))
	}
	if m.Triples != nil {
		if n, ok := m.Triples(); ok {
			p("# HELP sparql_server_dataset_triples Triples in the default graph and all named graphs.\n")
			p("# TYPE sparql_server_dataset_triples gauge\n")
			p("sparql_server_dataset_triples %d\n", n)
		}
	}

	p("# HELP sparql_server_build_info Build information.\n")
	p("# TYPE sparql_server_build_info gauge\n")
	p("sparql_server_build_info{version=%q,goversion=%q} 1\n", m.Version, runtime.Version())
	p("# HELP sparql_server_start_time_seconds Start time of the process since the Unix epoch.\n")
	p("# TYPE sparql_server_start_time_seconds gauge\n")
	p("sparql_server_start_time_seconds %s\n", formatFloat(float64(m.start.UnixMilli())/1000))
	p("# HELP sparql_server_goroutines Goroutines that currently exist.\n")
	p("# TYPE sparql_server_goroutines gauge\n")
	p("sparql_server_goroutines %d\n", runtime.NumGoroutine())
	return bw.Flush()
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func formatFloat(f float64) string {
	if math.IsInf(f, 1) {
		return "+Inf"
	}
	s := strconv.FormatFloat(f, 'g', -1, 64)
	if !strings.ContainsAny(s, ".e") {
		s += ".0"
	}
	return s
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
