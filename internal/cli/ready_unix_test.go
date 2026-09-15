//go:build unix

package cli

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestReadyAfterLoad feeds startup data through a FIFO, so loading blocks
// until the test writes it: the server must listen, answer /healthz, and
// refuse SPARQL requests and /readyz until the data is in.
func TestReadyAfterLoad(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "data.nt")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr := make(chan string, 1)
	logs := &syncBuffer{}
	env := Env{Stdout: io.Discard, Stderr: logs}
	env.ServerOptions.OnListen = func(a string) { addr <- a }
	done := make(chan int, 1)
	go func() { done <- Main(ctx, []string{"serve", "--listen", "127.0.0.1:0", "--data", fifo}, env) }()
	base := "http://" + <-addr

	wantStatus(t, "healthz while loading", do(t, http.MethodGet, base+"/healthz", ""), 200)
	wantStatus(t, "readyz while loading", do(t, http.MethodGet, base+"/readyz", ""), 503)
	r := query(t, base, namesQuery, "text/csv")
	wantStatus(t, "query while loading", r, 503)
	if r.Header.Get("Retry-After") == "" {
		t.Error("no Retry-After")
	}
	if m := do(t, http.MethodGet, base+"/metrics", ""); !strings.Contains(m.Body, "sparql_server_ready 0") {
		t.Error("metrics report ready while loading")
	}

	f, err := os.OpenFile(fifo, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`<http://ex/a> <http://xmlns.com/foaf/0.1/name> "Late" .` + "\n")
	f.Close()

	deadline := time.Now().Add(5 * time.Second)
	for do(t, http.MethodGet, base+"/readyz", "").Status != 200 {
		if time.Now().After(deadline) {
			t.Fatalf("not ready after loading; logs:\n%s", logs)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if r := query(t, base, namesQuery, "text/csv"); r.Body != "name\r\nLate\r\n" {
		t.Errorf("after load: %q", r.Body)
	}
	cancel()
	if code := <-done; code != 0 {
		t.Errorf("exit %d", code)
	}
}
