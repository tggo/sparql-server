package cli

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// writePeople writes n people with a name and an age as N-Triples.
func writePeople(t testing.TB, n int) string {
	t.Helper()
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, "<http://example.org/p%d> <http://xmlns.com/foaf/0.1/name> \"Person %d\" .\n", i, i)
		fmt.Fprintf(&b, "<http://example.org/p%d> <http://xmlns.com/foaf/0.1/age> \"%d\"^^<http://www.w3.org/2001/XMLSchema#integer> .\n", i, i%90)
	}
	p := filepath.Join(t.TempDir(), "people.nt")
	if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestConcurrentQueryThroughput runs a selective query from many clients for
// a fixed time and reports requests per second. It fails only on errors; the
// number is for the log (go test -run Throughput -v ./internal/cli).
func TestConcurrentQueryThroughput(t *testing.T) {
	if testing.Short() {
		t.Skip("load test")
	}
	data := writePeople(t, 10_000)
	for _, store := range []string{"memory", "badger:" + filepath.Join(t.TempDir(), "b")} {
		kind := strings.SplitN(store, ":", 2)[0]
		t.Run(kind, func(t *testing.T) {
			s := start(t, nil, "--store", store, "--data", data, "--access-log=false")
			q := url.Values{"query": {`SELECT ?name WHERE { ?p <http://xmlns.com/foaf/0.1/age> 42 ; <http://xmlns.com/foaf/0.1/name> ?name }`}}.Encode()
			u := s.URL + "/sparql?" + q

			const workers = 16
			const duration = 2 * time.Second
			tr := &http.Transport{MaxIdleConnsPerHost: workers}
			defer tr.CloseIdleConnections()
			client := &http.Client{Transport: tr}
			var ok, failed atomic.Int64
			stop := time.Now().Add(duration)
			var wg sync.WaitGroup
			for range workers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for time.Now().Before(stop) {
						req, _ := http.NewRequest(http.MethodGet, u, nil)
						req.Header.Set("Accept", "application/sparql-results+json")
						resp, err := client.Do(req)
						if err != nil {
							failed.Add(1)
							continue
						}
						io.Copy(io.Discard, resp.Body)
						resp.Body.Close()
						if resp.StatusCode == 200 {
							ok.Add(1)
						} else {
							failed.Add(1)
						}
					}
				}()
			}
			wg.Wait()
			if failed.Load() > 0 {
				t.Errorf("%d failed requests", failed.Load())
			}
			t.Logf("%s: %d workers, 20k triples, %d queries in %s = %.0f req/s",
				kind, workers, ok.Load(), duration, float64(ok.Load())/duration.Seconds())
		})
	}
}

// BenchmarkQuery measures one selective query over HTTP against the memory
// store, including the network stack and JSON encoding.
func BenchmarkQuery(b *testing.B) {
	data := writePeople(b, 10_000)
	s := start(b, nil, "--data", data, "--access-log=false")
	q := url.Values{"query": {`SELECT ?name WHERE { ?p <http://xmlns.com/foaf/0.1/age> 42 ; <http://xmlns.com/foaf/0.1/name> ?name }`}}.Encode()
	u := s.URL + "/sparql?" + q
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			resp, err := http.Get(u)
			if err != nil {
				b.Error(err)
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	})
}
