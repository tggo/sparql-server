package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tggo/goRDFlib/endpoint"
)

func TestExposition(t *testing.T) {
	m := New("v1.2.3")
	m.Ready = func() bool { return true }
	m.Triples = func() (int, bool) { return 42, true }
	m.Observe(endpoint.RequestInfo{Op: endpoint.OpQuery, Status: 200, Duration: 3 * time.Millisecond, Rows: 10, Bytes: 100})
	m.Observe(endpoint.RequestInfo{Op: endpoint.OpQuery, Status: 200, Duration: 2 * time.Second, Rows: 5, Bytes: 50})
	m.Observe(endpoint.RequestInfo{Op: endpoint.OpUpdate, Status: 401, Duration: time.Millisecond})

	block := make(chan struct{})
	entered := make(chan struct{})
	tracked := m.Track(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-block
	}))
	go tracked.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	<-entered

	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	close(block)
	out := rec.Body.String()
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Errorf("content type %q", ct)
	}
	for _, want := range []string{
		`sparql_server_requests_total{op="query",status="200"} 2`,
		`sparql_server_requests_total{op="update",status="401"} 1`,
		`sparql_server_request_duration_seconds_bucket{op="query",le="0.005"} 1`,
		`sparql_server_request_duration_seconds_bucket{op="query",le="1.0"} 1`,
		`sparql_server_request_duration_seconds_bucket{op="query",le="2.5"} 2`,
		`sparql_server_request_duration_seconds_bucket{op="query",le="+Inf"} 2`,
		`sparql_server_request_duration_seconds_count{op="query"} 2`,
		`sparql_server_result_rows_total{op="query"} 15`,
		`sparql_server_response_bytes_total{op="query"} 150`,
		`sparql_server_in_flight_requests 1`,
		`sparql_server_ready 1`,
		`sparql_server_dataset_triples 42`,
		`sparql_server_build_info{version="v1.2.3",goversion="go`,
		`# TYPE sparql_server_request_duration_seconds histogram`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("exposition lacks %q\n%s", want, out)
		}
	}
	// Every non-comment line is "name{labels} value" or "name value".
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		if f := strings.Fields(line); len(f) != 2 || !strings.HasPrefix(f[0], "sparql_server_") {
			t.Errorf("malformed line %q", line)
		}
	}

	rec = httptest.NewRecorder()
	m.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/metrics", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /metrics = %d", rec.Code)
	}
}
