package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func runMain(t *testing.T, stdin string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	out, errOut := &syncBuffer{}, &syncBuffer{}
	code = Main(t.Context(), args, Env{Stdin: strings.NewReader(stdin), Stdout: out, Stderr: errOut})
	return code, out.String(), errOut.String()
}

func TestLoadAndQueryCommands(t *testing.T) {
	for _, kind := range []string{"badger", "sqlite"} {
		t.Run(kind, func(t *testing.T) {
			dir := dataDir(t)
			store := kind + ":" + filepath.Join(t.TempDir(), "store")
			extra := filepath.Join(dir, "extra.nt")
			os.WriteFile(extra, []byte(`<http://ex/e> <http://ex/p> "extra" .`+"\n"), 0o644)

			code, out, errOut := runMain(t, "", "load", "--store", store, "--graph", "http://ex/extra="+extra,
				filepath.Join(dir, "people.ttl"), filepath.Join(dir, "graphs.trig"))
			if code != 0 {
				t.Fatalf("load: %d\n%s", code, errOut)
			}
			if !strings.Contains(out, "loaded 5 statements from 3 files") || !strings.Contains(out, "holds 5 triples") {
				t.Errorf("load output: %q", out)
			}

			code, out, errOut = runMain(t, "", "query", "--store", store, "--format", "csv", namesQuery)
			if code != 0 || out != "name\r\nAlice\r\nBob\r\n" {
				t.Errorf("query csv: %d %q %s", code, out, errOut)
			}
			code, out, _ = runMain(t, `SELECT ?o WHERE { GRAPH <http://ex/extra> { ?s ?p ?o } }`, "query", "--store", store, "--format", "json", "-")
			if code != 0 || !strings.Contains(out, `"value":"extra"`) {
				t.Errorf("query json from stdin: %d %q", code, out)
			}
			code, out, _ = runMain(t, "", "query", "--store", store, "--format", "ntriples",
				`CONSTRUCT { ?s ?p ?o } WHERE { GRAPH <http://example.org/g1> { ?s ?p ?o } }`)
			if code != 0 || !strings.Contains(out, `"Weaving the Web"`) {
				t.Errorf("query construct: %d %q", code, out)
			}
			code, out, _ = runMain(t, "", "query", "--store", store, `SELECT (COUNT(*) AS ?n) { ?s ?p ?o }`)
			if code != 0 || out != "?n\n3\n" {
				t.Errorf("query auto (tsv): %d %q", code, out)
			}
			code, _, errOut = runMain(t, "", "query", "--store", store, "--format", "turtle", namesQuery)
			if code != ExitError || !strings.Contains(errOut, "hint") {
				t.Errorf("wrong format: %d %s", code, errOut)
			}
			code, _, errOut = runMain(t, "", "query", "--store", store, `SELECT WHERE`)
			if code != ExitError || errOut == "" {
				t.Errorf("syntax error: %d %s", code, errOut)
			}
			code, _, _ = runMain(t, "", "query", "--store", store, `INSERT DATA { <http://ex/a> <http://ex/b> "c" }`)
			if code != ExitError {
				t.Errorf("an update through query: %d", code)
			}

			// Loading the same file again adds no triples (set semantics).
			if code, out, _ := runMain(t, "", "load", "--store", store, filepath.Join(dir, "people.ttl")); code != 0 || !strings.Contains(out, "holds 5 triples") {
				t.Errorf("reload: %d %q", code, out)
			}
		})
	}
}

func TestQueryCommandWithData(t *testing.T) {
	dir := dataDir(t)
	code, out, errOut := runMain(t, "", "query", "--data", filepath.Join(dir, "*"), "--format", "tsv", namesQuery)
	if code != 0 || out != "?name\n\"Alice\"\n\"Bob\"\n" {
		t.Errorf("memory query: %d %q %s", code, out, errOut)
	}
}

func TestCommandErrors(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		args []string
		code int
		msg  string
	}{
		{[]string{"load", "x.ttl"}, ExitUsage, "persistent store"},
		{[]string{"load", "--store", "badger:" + dir}, ExitUsage, "nothing to load"},
		{[]string{"load", "--store", "badger:" + filepath.Join(dir, "b"), filepath.Join(dir, "missing.ttl")}, ExitError, "missing.ttl"},
		{[]string{"query", "--store", "badger:" + filepath.Join(dir, "never-created"), "ASK {}"}, ExitError, "does not exist"},
		{[]string{"query"}, ExitUsage, "no query"},
		{[]string{"query", "--format", "yaml", "ASK {}"}, ExitUsage, "format"},
	}
	for _, c := range cases {
		code, _, errOut := runMain(t, "", c.args...)
		if code != c.code || !strings.Contains(errOut, c.msg) {
			t.Errorf("%v: exit %d, want %d; stderr %q", c.args, code, c.code, errOut)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "never-created")); err == nil {
		t.Error("query created a store")
	}
}

// TestBinarySignal builds the real binary, starts it on a persistent store,
// and stops it with SIGTERM.
func TestBinarySignal(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	if runtime.GOOS == "windows" {
		t.Skip("no SIGTERM on Windows")
	}
	bin := filepath.Join(t.TempDir(), "sparql-server")
	build := exec.Command("go", "build", "-o", bin, "github.com/tggo/sparql-server/cmd/sparql-server")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	store := "sqlite:" + filepath.Join(t.TempDir(), "s.db")
	dir := dataDir(t)

	cmd := exec.Command(bin, "serve", "--listen", "127.0.0.1:0", "--store", store, "--data", filepath.Join(dir, "people.ttl"), "--log-format", "json")
	cmd.Env = append(os.Environ(), "SPARQL_SERVER_UPDATE_TOKEN="+updateToken)
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var logs strings.Builder
	var mu sync.Mutex
	var addr atomic.Value
	readyCh := make(chan struct{})
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		buf := make([]byte, 4096)
		var once sync.Once
		for {
			n, err := stderr.Read(buf)
			mu.Lock()
			logs.Write(buf[:n])
			s := logs.String()
			mu.Unlock()
			if i := strings.Index(s, `"addr":"`); i >= 0 {
				rest := s[i+len(`"addr":"`):]
				if j := strings.IndexByte(rest, '"'); j >= 0 {
					addr.Store(rest[:j])
				}
			}
			if strings.Contains(s, `"msg":"ready"`) {
				once.Do(func() { close(readyCh) })
			}
			if err != nil {
				return
			}
		}
	}()
	select {
	case <-readyCh:
	case <-time.After(20 * time.Second):
		cmd.Process.Kill()
		t.Fatalf("binary not ready; logs:\n%s", logs.String())
	}
	base := "http://" + addr.Load().(string)
	wantStatus(t, "update", update(t, base, `INSERT DATA { <http://ex/s> <http://xmlns.com/foaf/0.1/name> "Signal" }`, bearer(updateToken)...), 204)

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	// The pipe reaches EOF when the process exits; read everything before Wait.
	select {
	case <-readerDone:
	case <-time.After(20 * time.Second):
		cmd.Process.Kill()
		t.Fatal("binary did not exit on SIGTERM")
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("exit: %v\n%s", err, logs.String())
	}
	mu.Lock()
	final := logs.String()
	mu.Unlock()
	if !strings.Contains(final, `"msg":"shutting down"`) || !strings.Contains(final, `"msg":"stopped"`) {
		t.Errorf("logs:\n%s", final)
	}

	out, err := exec.Command(bin, "query", "--store", store, "--format", "csv", namesQuery).CombinedOutput()
	if err != nil || string(out) != "name\r\nAlice\r\nBob\r\nSignal\r\n" {
		t.Errorf("query after restart: %v %q", err, out)
	}
}
