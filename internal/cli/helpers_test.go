package cli

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	updateToken = "update-token-for-tests-0123"
	queryToken  = "query-token-for-tests-0123"
)

// syncBuffer is a bytes.Buffer safe for the server's log goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// testServer is a sparql-server running in this process.
type testServer struct {
	URL    string
	logs   *syncBuffer
	cancel context.CancelFunc
	done   chan struct{} // closed when Main returned
	code   int
}

// stop shuts the server down gracefully and returns its exit code.
func (s *testServer) stop(t *testing.T) int {
	t.Helper()
	s.cancel()
	select {
	case <-s.done:
		return s.code
	case <-time.After(30 * time.Second):
		t.Fatalf("server did not stop; logs:\n%s", s.logs)
		return -1
	}
}

// start runs `sparql-server serve` with args on a random port and waits until
// it is ready.
func start(t testing.TB, environ []string, args ...string) *testServer {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	s := &testServer{logs: &syncBuffer{}, cancel: cancel, done: make(chan struct{})}
	addr := make(chan string, 1)
	env := Env{
		Stdout:  io.Discard,
		Stderr:  s.logs,
		Environ: environ,
	}
	env.ServerOptions.OnListen = func(a string) { addr <- a }
	full := append([]string{"serve", "--listen", "127.0.0.1:0", "--shutdown-timeout", "5s"}, args...)
	go func() {
		s.code = Main(ctx, full, env)
		close(s.done)
	}()

	select {
	case a := <-addr:
		s.URL = "http://" + a
	case <-s.done:
		cancel()
		t.Fatalf("server exited with %d before listening; logs:\n%s", s.code, s.logs)
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatalf("server did not listen; logs:\n%s", s.logs)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := http.Get(s.URL + "/readyz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("server not ready; logs:\n%s", s.logs)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Cleanup(func() {
		cancel()
		select {
		case <-s.done:
		case <-time.After(30 * time.Second):
		}
	})
	return s
}

type result struct {
	Status int
	Header http.Header
	Body   string
}

func do(t *testing.T, method, u string, body string, headers ...string) result {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, u, r)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return result{Status: resp.StatusCode, Header: resp.Header, Body: string(b)}
}

func query(t *testing.T, base, q, accept string, headers ...string) result {
	t.Helper()
	return do(t, http.MethodGet, base+"/sparql?"+url.Values{"query": {q}}.Encode(), "", append([]string{"Accept", accept}, headers...)...)
}

func update(t *testing.T, base, u string, headers ...string) result {
	t.Helper()
	return do(t, http.MethodPost, base+"/sparql", u, append([]string{"Content-Type", "application/sparql-update"}, headers...)...)
}

func bearer(tok string) []string { return []string{"Authorization", "Bearer " + tok} }

func wantStatus(t *testing.T, what string, r result, want int) {
	t.Helper()
	if r.Status != want {
		t.Fatalf("%s: status %d, want %d; body: %s", what, r.Status, want, r.Body)
	}
}

const peopleTTL = `@prefix ex: <http://example.org/> .
@prefix foaf: <http://xmlns.com/foaf/0.1/> .
ex:alice foaf:name "Alice" ; foaf:knows ex:bob .
ex:bob foaf:name "Bob" .
`

const graphsTriG = `@prefix ex: <http://example.org/> .
ex:g1 { ex:book ex:title "Weaving the Web" }
`

// dataDir writes the test data files and returns their directory.
func dataDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range map[string]string{"people.ttl": peopleTTL, "graphs.trig": graphsTriG} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const namesQuery = `SELECT ?name WHERE { ?s <http://xmlns.com/foaf/0.1/name> ?name } ORDER BY ?name`
