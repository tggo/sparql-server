package cli

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestServeQueryFormats(t *testing.T) {
	dir := dataDir(t)
	s := start(t, nil, "--data", filepath.Join(dir, "*"))

	r := query(t, s.URL, namesQuery, "application/sparql-results+json")
	wantStatus(t, "json", r, 200)
	var doc struct {
		Results struct {
			Bindings []map[string]struct{ Value string } `json:"bindings"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(r.Body), &doc); err != nil || len(doc.Results.Bindings) != 2 || doc.Results.Bindings[0]["name"].Value != "Alice" {
		t.Fatalf("json: %v %s", err, r.Body)
	}

	r = query(t, s.URL, namesQuery, "text/csv")
	wantStatus(t, "csv", r, 200)
	if r.Body != "name\r\nAlice\r\nBob\r\n" || !strings.HasPrefix(r.Header.Get("Content-Type"), "text/csv") {
		t.Errorf("csv: %q %s", r.Body, r.Header.Get("Content-Type"))
	}
	r = query(t, s.URL, namesQuery, "text/tab-separated-values")
	if r.Body != "?name\n\"Alice\"\n\"Bob\"\n" {
		t.Errorf("tsv: %q", r.Body)
	}
	r = query(t, s.URL, namesQuery, "application/sparql-results+xml")
	if !strings.Contains(r.Body, "<literal>Alice</literal>") {
		t.Errorf("xml: %s", r.Body)
	}

	// Named graphs from TriG, POST with a direct body.
	r = do(t, http.MethodPost, s.URL+"/sparql", `SELECT ?t WHERE { GRAPH <http://example.org/g1> { ?b ?p ?t } }`,
		"Content-Type", "application/sparql-query", "Accept", "text/csv")
	if r.Body != "t\r\nWeaving the Web\r\n" {
		t.Errorf("named graph: %q", r.Body)
	}

	r = query(t, s.URL, `CONSTRUCT { ?s ?p ?o } WHERE { ?s ?p ?o }`, "application/n-triples")
	wantStatus(t, "construct", r, 200)
	if strings.Count(r.Body, "\n") != 3 {
		t.Errorf("construct: %q", r.Body)
	}

	r = query(t, s.URL, `ASK { ?s ?p "Bob" }`, "application/sparql-results+json")
	if !strings.Contains(r.Body, `"boolean":true`) && !strings.Contains(r.Body, `"boolean": true`) {
		t.Errorf("ask: %s", r.Body)
	}

	r = query(t, s.URL, `SELEKT * {}`, "text/csv")
	wantStatus(t, "syntax error", r, 400)

	// Service description, read-only: no SPARQL11Update.
	r = do(t, http.MethodGet, s.URL+"/sparql", "", "Accept", "text/turtle")
	wantStatus(t, "service description", r, 200)
	if !strings.Contains(r.Body, "SPARQL11Query") || strings.Contains(r.Body, "SPARQL11Update") {
		t.Errorf("service description: %s", r.Body)
	}

	if code := s.stop(t); code != 0 {
		t.Errorf("exit code %d; logs:\n%s", code, s.logs)
	}
	logs := s.logs.String()
	for _, want := range []string{"msg=listening", "msg=loaded", "msg=ready", "msg=request", "op=query", "rows=2", "msg=stopped"} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs lack %q:\n%s", want, logs)
		}
	}
}

func TestUpdateAuthorization(t *testing.T) {
	const insert = `INSERT DATA { <http://ex/a> <http://ex/p> "1" }`

	t.Run("read-only by default", func(t *testing.T) {
		s := start(t, nil)
		wantStatus(t, "update", update(t, s.URL, insert), 403)
		wantStatus(t, "update with a token", update(t, s.URL, insert, bearer(updateToken)...), 403)
		wantStatus(t, "gsp put", do(t, http.MethodPut, s.URL+"/graph-store?graph=http://ex/g", `<http://ex/a> <http://ex/p> "1" .`, "Content-Type", "application/n-triples"), 403)
	})

	t.Run("update token", func(t *testing.T) {
		s := start(t, []string{"SPARQL_SERVER_UPDATE_TOKEN=" + updateToken})
		r := update(t, s.URL, insert)
		wantStatus(t, "no token", r, 401)
		if len(r.Header.Values("WWW-Authenticate")) == 0 {
			t.Error("no WWW-Authenticate")
		}
		r = update(t, s.URL, insert, bearer("wrong-token-wrong-token")...)
		wantStatus(t, "wrong token", r, 401)
		if !strings.Contains(r.Header.Get("WWW-Authenticate"), "invalid_token") {
			t.Errorf("WWW-Authenticate = %q", r.Header.Get("WWW-Authenticate"))
		}
		wantStatus(t, "token", update(t, s.URL, insert, bearer(updateToken)...), 204)

		req, _ := http.NewRequest(http.MethodPost, s.URL+"/sparql", strings.NewReader(`INSERT DATA { <http://ex/b> <http://ex/p> "2" }`))
		req.Header.Set("Content-Type", "application/sparql-update")
		req.SetBasicAuth("jena", updateToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 204 {
			t.Errorf("basic auth: %d", resp.StatusCode)
		}

		r = query(t, s.URL, `SELECT (COUNT(*) AS ?n) WHERE { ?s ?p ?o }`, "text/csv")
		if r.Body != "n\r\n2\r\n" {
			t.Errorf("count after updates: %q", r.Body)
		}
		// Service description advertises updates now.
		if r := do(t, http.MethodGet, s.URL+"/sparql", "", "Accept", "text/turtle"); !strings.Contains(r.Body, "SPARQL11Update") {
			t.Error("service description lacks SPARQL11Update")
		}
	})

	t.Run("query token", func(t *testing.T) {
		s := start(t, nil, "--query-token", queryToken, "--update-token", updateToken)
		wantStatus(t, "no token", query(t, s.URL, namesQuery, "text/csv"), 401)
		wantStatus(t, "query token", query(t, s.URL, namesQuery, "text/csv", bearer(queryToken)...), 200)
		wantStatus(t, "update token reads", query(t, s.URL, namesQuery, "text/csv", bearer(updateToken)...), 200)
		wantStatus(t, "query token writes", update(t, s.URL, insert, bearer(queryToken)...), 403)
		wantStatus(t, "graph read", do(t, http.MethodGet, s.URL+"/graph-store?default", "", "Accept", "text/turtle"), 401)
		// Operational endpoints stay open.
		wantStatus(t, "healthz", do(t, http.MethodGet, s.URL+"/healthz", ""), 200)
	})

	t.Run("anonymous updates", func(t *testing.T) {
		s := start(t, nil, "--allow-anonymous-updates")
		wantStatus(t, "update", update(t, s.URL, insert), 204)
		if !strings.Contains(s.logs.String(), "allow-anonymous-updates") {
			t.Error("no startup warning")
		}
	})

	t.Run("token file", func(t *testing.T) {
		env := Env{Stderr: &syncBuffer{}, Stdout: io.Discard, ReadFile: func(string) ([]byte, error) { return nil, fmt.Errorf("unreadable") }}
		if code := Main(t.Context(), []string{"serve", "--update-token-file", "/x"}, env); code != ExitUsage {
			t.Errorf("unreadable token file: exit %d", code)
		}
	})
}

func TestGraphStore(t *testing.T) {
	s := start(t, nil, "--update-token", updateToken)
	auth := bearer(updateToken)
	nt := "<http://ex/a> <http://ex/p> \"1\" .\n<http://ex/a> <http://ex/p> \"2\" .\n"

	r := do(t, http.MethodPut, s.URL+"/graph-store?graph=http://ex/g", nt, append([]string{"Content-Type", "application/n-triples"}, auth...)...)
	wantStatus(t, "put", r, 201)
	r = do(t, http.MethodGet, s.URL+"/graph-store?graph=http://ex/g", "", "Accept", "application/n-triples")
	wantStatus(t, "get", r, 200)
	if strings.Count(r.Body, "\n") != 2 {
		t.Errorf("get: %q", r.Body)
	}
	// The same graph through SPARQL.
	if r := query(t, s.URL, `SELECT (COUNT(*) AS ?n) WHERE { GRAPH <http://ex/g> { ?s ?p ?o } }`, "text/csv"); r.Body != "n\r\n2\r\n" {
		t.Errorf("graph via sparql: %q", r.Body)
	}
	r = do(t, http.MethodPost, s.URL+"/graph-store?default", `<http://ex/d> <http://ex/p> "3" .`, append([]string{"Content-Type", "text/turtle"}, auth...)...)
	if r.Status != 200 && r.Status != 201 && r.Status != 204 {
		t.Errorf("post default: %d %s", r.Status, r.Body)
	}
	// Direct identification: the graph IRI is the request URL.
	direct := s.URL + "/graph-store/people"
	wantStatus(t, "direct put", do(t, http.MethodPut, direct, nt, append([]string{"Content-Type", "application/n-triples"}, auth...)...), 201)
	if r := query(t, s.URL, fmt.Sprintf(`ASK { GRAPH <%s> { ?s ?p "2" } }`, direct), "application/sparql-results+json"); !strings.Contains(r.Body, "true") {
		t.Errorf("direct graph not found: %q", r.Body)
	}
	wantStatus(t, "delete without token", do(t, http.MethodDelete, s.URL+"/graph-store?graph=http://ex/g", ""), 401)
	wantStatus(t, "delete", do(t, http.MethodDelete, s.URL+"/graph-store?graph=http://ex/g", "", auth...), 200)
	wantStatus(t, "get deleted", do(t, http.MethodGet, s.URL+"/graph-store?graph=http://ex/g", ""), 404)
	// Indirect GSP on the SPARQL endpoint URL works too.
	wantStatus(t, "gsp on /sparql", do(t, http.MethodGet, s.URL+"/sparql?graph="+url.QueryEscape(direct), "", "Accept", "text/turtle"), 200)
}

func TestFusekiRoutes(t *testing.T) {
	s := start(t, nil, "--dataset", "ds", "--allow-anonymous-updates", "--data", filepath.Join(dataDir(t), "people.ttl"))
	r := do(t, http.MethodGet, s.URL+"/ds/query?"+url.Values{"query": {namesQuery}}.Encode(), "", "Accept", "text/csv")
	wantStatus(t, "/ds/query", r, 200)
	r = do(t, http.MethodPost, s.URL+"/ds/update", `INSERT DATA { <http://ex/x> <http://xmlns.com/foaf/0.1/name> "Zed" }`, "Content-Type", "application/sparql-update")
	wantStatus(t, "/ds/update", r, 204)
	wantStatus(t, "update on /ds/query", do(t, http.MethodPost, s.URL+"/ds/query", `CLEAR ALL`, "Content-Type", "application/sparql-update"), 400)
	r = do(t, http.MethodGet, s.URL+"/ds/sparql?"+url.Values{"query": {namesQuery}}.Encode(), "", "Accept", "text/csv")
	if r.Body != "name\r\nAlice\r\nBob\r\nZed\r\n" {
		t.Errorf("/ds/sparql: %q", r.Body)
	}
	wantStatus(t, "/ds/data put", do(t, http.MethodPut, s.URL+"/ds/data?graph=http://ex/g", `<http://ex/a> <http://ex/p> "1" .`, "Content-Type", "application/n-triples"), 201)
	wantStatus(t, "/ds/get", do(t, http.MethodGet, s.URL+"/ds/get?graph=http://ex/g", ""), 200)
	wantStatus(t, "/ds/get put", do(t, http.MethodPut, s.URL+"/ds/get?graph=http://ex/g", `<http://ex/a> <http://ex/p> "1" .`, "Content-Type", "application/n-triples"), 405)
	wantStatus(t, "/ds", do(t, http.MethodGet, s.URL+"/ds?"+url.Values{"query": {namesQuery}}.Encode(), "", "Accept", "text/csv"), 200)
	wantStatus(t, "other dataset", do(t, http.MethodGet, s.URL+"/other/query", ""), 404)
}

func TestCORSAndGzip(t *testing.T) {
	s := start(t, nil, "--cors-origin", "https://app.example", "--update-token", updateToken,
		"--data", filepath.Join(dataDir(t), "people.ttl"))

	r := do(t, http.MethodOptions, s.URL+"/sparql", "",
		"Origin", "https://app.example", "Access-Control-Request-Method", "POST", "Access-Control-Request-Headers", "authorization,content-type")
	wantStatus(t, "preflight", r, 204)
	if r.Header.Get("Access-Control-Allow-Origin") != "https://app.example" || !strings.Contains(r.Header.Get("Access-Control-Allow-Methods"), "POST") {
		t.Errorf("preflight headers: %v", r.Header)
	}
	r = query(t, s.URL, namesQuery, "text/csv", "Origin", "https://app.example")
	if r.Header.Get("Access-Control-Allow-Origin") != "https://app.example" {
		t.Errorf("simple request headers: %v", r.Header)
	}
	r = query(t, s.URL, namesQuery, "text/csv", "Origin", "https://evil.example")
	if r.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Error("disallowed origin allowed")
	}

	// Gzip: a large result is compressed, a small one is not.
	tr := &http.Transport{DisableCompression: true}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr}
	big := `SELECT ?i WHERE { VALUES ?x { 1 2 3 4 5 6 7 8 9 10 } VALUES ?y { 1 2 3 4 5 6 7 8 9 10 } VALUES ?z { 1 2 3 4 5 6 7 8 9 10 } BIND(?x*100+?y*10+?z AS ?i) }`
	req, _ := http.NewRequest(http.MethodGet, s.URL+"/sparql?"+url.Values{"query": {big}}.Encode(), nil)
	req.Header.Set("Accept", "application/sparql-results+json")
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("big result: %d %v", resp.StatusCode, resp.Header)
	}
	zr, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Results struct{ Bindings []any } `json:"results"`
	}
	if err := json.NewDecoder(zr).Decode(&doc); err != nil || len(doc.Results.Bindings) != 1000 {
		t.Fatalf("decoded %d bindings, %v", len(doc.Results.Bindings), err)
	}

	req, _ = http.NewRequest(http.MethodGet, s.URL+"/sparql?"+url.Values{"query": {`ASK {}`}}.Encode(), nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp2, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.Header.Get("Content-Encoding") != "" {
		t.Error("tiny ASK result compressed")
	}
}

func TestOperationalEndpoints(t *testing.T) {
	s := start(t, nil, "--data", filepath.Join(dataDir(t), "*"), "--update-token", updateToken, "--max-result-rows", "2")

	wantStatus(t, "healthz", do(t, http.MethodGet, s.URL+"/healthz", ""), 200)
	wantStatus(t, "readyz", do(t, http.MethodGet, s.URL+"/readyz", ""), 200)
	wantStatus(t, "row limit", query(t, s.URL, `SELECT * WHERE { ?s ?p ?o }`, "text/csv"), 422)
	query(t, s.URL, namesQuery, "text/csv")
	update(t, s.URL, `INSERT DATA { <http://ex/n> <http://ex/p> "new" }`, bearer(updateToken)...)

	r := do(t, http.MethodGet, s.URL+"/metrics", "")
	wantStatus(t, "metrics", r, 200)
	for _, want := range []string{
		`sparql_server_requests_total{op="query",status="200"} 1`,
		`sparql_server_requests_total{op="query",status="422"} 1`,
		`sparql_server_requests_total{op="update",status="204"} 1`,
		`sparql_server_request_duration_seconds_count{op="query"} 2`,
		`sparql_server_result_rows_total{op="query"} 2`,
		`sparql_server_in_flight_requests 0`,
		`sparql_server_ready 1`,
		`sparql_server_dataset_triples 5`, // 3 + 1 loaded, 1 inserted
	} {
		if !strings.Contains(r.Body, want) {
			t.Errorf("metrics lack %q:\n%s", want, r.Body)
		}
	}

	r = do(t, http.MethodGet, s.URL+"/", "")
	wantStatus(t, "ui", r, 200)
	if !strings.Contains(r.Header.Get("Content-Security-Policy"), "default-src 'none'") || !strings.Contains(r.Body, "<textarea") {
		t.Errorf("ui: %v", r.Header)
	}
	if r := do(t, http.MethodGet, s.URL+"/ui/app.js", ""); r.Status != 200 || !strings.Contains(r.Header.Get("Content-Type"), "javascript") || strings.Contains(r.Body, "innerHTML") {
		t.Errorf("app.js: %d %s", r.Status, r.Header.Get("Content-Type"))
	}
	wantStatus(t, "unknown path", do(t, http.MethodGet, s.URL+"/nope", ""), 404)

	off := start(t, nil, "--metrics=false", "--ui=false")
	wantStatus(t, "metrics disabled", do(t, http.MethodGet, off.URL+"/metrics", ""), 404)
	wantStatus(t, "ui disabled", do(t, http.MethodGet, off.URL+"/", ""), 404)
}

// TestGracefulShutdown sends a Graph Store PUT whose body arrives slowly,
// cancels the server in the middle, and checks that the request still
// completes and is persisted before the process exits.
func TestGracefulShutdown(t *testing.T) {
	store := "badger:" + filepath.Join(t.TempDir(), "db")
	s := start(t, nil, "--store", store, "--update-token", updateToken)

	pr, pw := io.Pipe()
	req, _ := http.NewRequest(http.MethodPut, s.URL+"/graph-store?graph=http://ex/slow", pr)
	req.Header.Set("Content-Type", "application/n-triples")
	req.Header.Set("Authorization", "Bearer "+updateToken)
	respCh := make(chan *http.Response, 1)
	errCh := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			errCh <- err
			return
		}
		respCh <- resp
	}()
	fmt.Fprintln(pw, `<http://ex/a> <http://ex/p> "1" .`)
	time.Sleep(100 * time.Millisecond) // the request is in flight

	exited := make(chan int, 1)
	go func() { exited <- s.stop(t) }()

	// New connections are refused while the running request drains.
	deadline := time.Now().Add(3 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", strings.TrimPrefix(s.URL, "http://"), 100*time.Millisecond)
		if err != nil {
			break
		}
		c.Close()
		if time.Now().After(deadline) {
			t.Fatal("listener still accepts connections during shutdown")
		}
		time.Sleep(20 * time.Millisecond)
	}
	select {
	case code := <-exited:
		t.Fatalf("server exited (%d) before the running request finished", code)
	default:
	}

	fmt.Fprintln(pw, `<http://ex/a> <http://ex/p> "2" .`)
	pw.Close()
	select {
	case resp := <-respCh:
		resp.Body.Close()
		if resp.StatusCode != 201 {
			t.Fatalf("drained request: %d", resp.StatusCode)
		}
	case err := <-errCh:
		t.Fatalf("drained request failed: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("drained request did not finish")
	}
	if code := <-exited; code != 0 {
		t.Fatalf("exit code %d; logs:\n%s", code, s.logs)
	}

	s2 := start(t, nil, "--store", store)
	if r := query(t, s2.URL, `SELECT (COUNT(*) AS ?n) WHERE { GRAPH <http://ex/slow> { ?s ?p ?o } }`, "text/csv"); r.Body != "n\r\n2\r\n" {
		t.Errorf("after restart: %q", r.Body)
	}
}

func TestPersistenceAcrossRestart(t *testing.T) {
	for _, kind := range []string{"badger", "sqlite"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			store := kind + ":" + filepath.Join(dir, "store")
			if kind == "sqlite" {
				store += ".db"
			}
			s := start(t, nil, "--store", store, "--update-token", updateToken, "--data", filepath.Join(dataDir(t), "*"))
			wantStatus(t, "update", update(t, s.URL, `INSERT DATA { GRAPH <http://ex/g2> { <http://ex/x> <http://ex/p> "persisted" } }`, bearer(updateToken)...), 204)
			wantStatus(t, "gsp", do(t, http.MethodPut, s.URL+"/graph-store?graph=http://ex/g3", `<http://ex/y> <http://ex/p> "also" .`,
				"Content-Type", "application/n-triples", "Authorization", "Bearer "+updateToken), 201)
			wantStatus(t, "delete", update(t, s.URL, `DELETE DATA { <http://example.org/bob> <http://xmlns.com/foaf/0.1/name> "Bob" }`, bearer(updateToken)...), 204)
			if code := s.stop(t); code != 0 {
				t.Fatalf("exit %d; logs:\n%s", code, s.logs)
			}

			// Restart without --data: everything must come from the store.
			s = start(t, nil, "--store", store)
			r := query(t, s.URL, `SELECT ?g ?o WHERE { GRAPH ?g { ?s ?p ?o } } ORDER BY ?g`, "text/csv")
			want := "g,o\r\nhttp://ex/g2,persisted\r\nhttp://ex/g3,also\r\nhttp://example.org/g1,Weaving the Web\r\n"
			if r.Body != want {
				t.Errorf("named graphs after restart:\n%q\nwant\n%q", r.Body, want)
			}
			if r := query(t, s.URL, namesQuery, "text/csv"); r.Body != "name\r\nAlice\r\n" {
				t.Errorf("default graph after restart: %q", r.Body)
			}
			if code := s.stop(t); code != 0 {
				t.Fatalf("exit %d", code)
			}
		})
	}
}

func TestLOADAllowlist(t *testing.T) {
	origin := http.NewServeMux()
	origin.HandleFunc("/public/data.ttl", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/turtle")
		io.WriteString(w, `<http://ex/remote> <http://ex/p> "loaded" .`)
	})
	origin.HandleFunc("/private/secret.ttl", func(w http.ResponseWriter, r *http.Request) {
		t.Error("the server fetched a URL outside the allowlist")
	})
	o := httptest.NewServer(origin)
	defer o.Close()

	s := start(t, nil, "--allow-anonymous-updates", "--allow-load", o.URL+"/public/")
	r := update(t, s.URL, fmt.Sprintf(`LOAD <%s/public/data.ttl> INTO GRAPH <http://ex/loaded>`, o.URL))
	wantStatus(t, "allowed LOAD", r, 204)
	if r := query(t, s.URL, `SELECT ?o WHERE { GRAPH <http://ex/loaded> { ?s ?p ?o } }`, "text/csv"); r.Body != "o\r\nloaded\r\n" {
		t.Errorf("loaded graph: %q", r.Body)
	}
	for _, src := range []string{o.URL + "/private/secret.ttl", "file:///etc/passwd", o.URL + "/public/../private/secret.ttl"} {
		r := update(t, s.URL, fmt.Sprintf(`LOAD <%s>`, src))
		if r.Status < 400 || !strings.Contains(r.Body, "allowlist") {
			t.Errorf("LOAD <%s>: %d %s", src, r.Status, r.Body)
		}
	}
	// LOAD SILENT of a refused source succeeds without loading anything.
	wantStatus(t, "silent", update(t, s.URL, `LOAD SILENT <file:///etc/passwd>`), 204)

	plain := start(t, nil, "--allow-anonymous-updates")
	wantStatus(t, "LOAD without allow-load", update(t, plain.URL, fmt.Sprintf(`LOAD <%s/public/data.ttl>`, o.URL)), 403)
}

func TestServeStartupErrors(t *testing.T) {
	run := func(args ...string) (int, string) {
		logs := &syncBuffer{}
		code := Main(t.Context(), args, Env{Stdout: io.Discard, Stderr: logs})
		return code, logs.String()
	}
	if code, out := run("serve", "--listen", "127.0.0.1:0", "--data", filepath.Join(t.TempDir(), "missing.ttl"), "--shutdown-timeout", "1s"); code != ExitError || !strings.Contains(out, "missing.ttl") {
		t.Errorf("missing data file: %d %s", code, out)
	}
	if code, out := run("serve", "--update-token", "short"); code != ExitUsage || !strings.Contains(out, "16") {
		t.Errorf("short token: %d %s", code, out)
	}
	if code, _ := run("serve", "--no-such-flag"); code != ExitUsage {
		t.Errorf("unknown flag: %d", code)
	}
	if code, _ := run("serve", "-h"); code != ExitOK {
		t.Errorf("help: %d", code)
	}
	if code, _ := run("bogus"); code != ExitUsage {
		t.Errorf("unknown command: %d", code)
	}
	if code, out := run("serve", "--listen", "127.0.0.1:0", "--tls-cert", "/nope.pem", "--tls-key", "/nope.key"); code != ExitError || !strings.Contains(out, "tls") {
		t.Errorf("bad tls files: %d %s", code, out)
	}
	logs := &syncBuffer{}
	stdout := &syncBuffer{}
	if code := Main(t.Context(), []string{"version"}, Env{Stdout: stdout, Stderr: logs}); code != 0 || !strings.Contains(stdout.String(), "goRDFlib: v0.5.4") {
		t.Errorf("version: %d %q", code, stdout.String())
	}
}
