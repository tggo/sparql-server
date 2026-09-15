package config

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestParseStoreSpec(t *testing.T) {
	tests := []struct {
		in      string
		want    StoreSpec
		wantErr string
	}{
		{in: "", want: StoreSpec{Kind: StoreMemory}},
		{in: "memory", want: StoreSpec{Kind: StoreMemory}},
		{in: "badger:/var/lib/x", want: StoreSpec{Kind: StoreBadger, Path: "/var/lib/x"}},
		{in: "sqlite:./data.db", want: StoreSpec{Kind: StoreSQLite, Path: "./data.db"}},
		{in: `badger:C:\data`, want: StoreSpec{Kind: StoreBadger, Path: `C:\data`}},
		{in: "badger:", wantErr: "needs a path"},
		{in: "sqlite", wantErr: "needs a path"},
		{in: "memory:/x", wantErr: "takes no path"},
		{in: "mongo:/x", wantErr: "unknown store"},
	}
	for _, tt := range tests {
		got, err := ParseStoreSpec(tt.in)
		if tt.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("ParseStoreSpec(%q) error = %v, want %q", tt.in, err, tt.wantErr)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("ParseStoreSpec(%q) = %+v, %v; want %+v", tt.in, got, err, tt.want)
		}
	}
	if !(StoreSpec{Kind: StoreSQLite, Path: "x"}).Persistent() || (StoreSpec{Kind: StoreMemory}).Persistent() {
		t.Error("Persistent is wrong")
	}
}

func TestParseGraphFile(t *testing.T) {
	gf, err := ParseGraphFile("http://ex.org/g?id=1=data/a.ttl")
	if err != nil || gf.IRI != "http://ex.org/g?id=1" || gf.Path != "data/a.ttl" {
		t.Errorf("got %+v, %v", gf, err)
	}
	for _, bad := range []string{"a.ttl", "=a.ttl", "http://ex.org/g=", "relative=a.ttl"} {
		if _, err := ParseGraphFile(bad); err == nil {
			t.Errorf("ParseGraphFile(%q) accepted", bad)
		}
	}
}

func TestParseBytes(t *testing.T) {
	tests := map[string]int64{
		"0": 0, "1024": 1024, "1KiB": 1024, "64MiB": 64 << 20, "10MB": 10_000_000, "2G": 2 << 30, "512k": 512 << 10, " 3 gib ": 3 << 30,
	}
	for in, want := range tests {
		if got, err := ParseBytes(in); err != nil || got != want {
			t.Errorf("ParseBytes(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "-1", "ten", "1.5MB", "99999999999999999GiB"} {
		if _, err := ParseBytes(bad); err == nil {
			t.Errorf("ParseBytes(%q) accepted", bad)
		}
	}
	if FormatBytes(64<<20) != "64MiB" || FormatBytes(1000) != "1000" {
		t.Error("FormatBytes")
	}
}

func TestEnvName(t *testing.T) {
	if got := EnvName("update-token"); got != "SPARQL_SERVER_UPDATE_TOKEN" {
		t.Errorf("got %s", got)
	}
}

func parseServe(t *testing.T, args []string, environ []string) (*Serve, []string, error) {
	t.Helper()
	var s Serve
	fs := NewServeFlags(&s, io.Discard)
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	unknown, err := ApplyEnv(fs, environ, KnownEnv())
	return &s, unknown, err
}

func TestApplyEnvPrecedence(t *testing.T) {
	s, unknown, err := parseServe(t,
		[]string{"--listen", "127.0.0.1:9000", "--data", "a.ttl"},
		[]string{
			"SPARQL_SERVER_LISTEN=0.0.0.0:1",                                 // loses to the flag
			"SPARQL_SERVER_DATA=ignored.ttl",                                 // loses to the flag
			"SPARQL_SERVER_QUERY_TIMEOUT=5s",                                 // wins over the default
			"SPARQL_SERVER_CORS_ORIGIN=https://a.example, https://b.example", // list
			"SPARQL_SERVER_GZIP=false",
			"SPARQL_SERVER_MAX_GRAPH_BYTES=1MiB",
			"SPARQL_SERVER_LISTN=typo",
			"HOME=/root",
		})
	if err != nil {
		t.Fatal(err)
	}
	if s.Listen != "127.0.0.1:9000" {
		t.Errorf("listen = %s", s.Listen)
	}
	if len(s.Data) != 1 || s.Data[0] != "a.ttl" {
		t.Errorf("data = %v", s.Data)
	}
	if s.QueryTimeout != 5*time.Second {
		t.Errorf("query timeout = %v", s.QueryTimeout)
	}
	if len(s.CORSOrigins) != 2 || s.CORSOrigins[1] != "https://b.example" {
		t.Errorf("cors = %v", s.CORSOrigins)
	}
	if s.Gzip {
		t.Error("gzip should be off")
	}
	if s.MaxGraphBytes != 1<<20 {
		t.Errorf("max graph bytes = %d", s.MaxGraphBytes)
	}
	if s.UpdateTimeout != DefaultUpdateTimeout || s.MaxRequestBytes != DefaultMaxRequestBytes {
		t.Error("defaults not kept")
	}
	if len(unknown) != 1 || unknown[0] != "SPARQL_SERVER_LISTN" {
		t.Errorf("unknown = %v", unknown)
	}
}

func TestApplyEnvInvalidValue(t *testing.T) {
	_, _, err := parseServe(t, nil, []string{"SPARQL_SERVER_QUERY_TIMEOUT=soon", "SPARQL_SERVER_METRICS=maybe"})
	if err == nil || !strings.Contains(err.Error(), "SPARQL_SERVER_QUERY_TIMEOUT") || !strings.Contains(err.Error(), "SPARQL_SERVER_METRICS") {
		t.Errorf("err = %v", err)
	}
}

func validServe(t *testing.T, args ...string) *Serve {
	t.Helper()
	s, _, err := parseServe(t, args, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestServeValidate(t *testing.T) {
	files := map[string]string{"/run/secrets/tok": "0123456789abcdef-file\n", "/empty": "\n"}
	readFile := func(p string) ([]byte, error) {
		if v, ok := files[p]; ok {
			return []byte(v), nil
		}
		return nil, errors.New("no such file")
	}
	const tok = "0123456789abcdef"
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "defaults"},
		{name: "token", args: []string{"--update-token", tok}},
		{name: "token file", args: []string{"--update-token-file", "/run/secrets/tok"}},
		{name: "short token", args: []string{"--update-token", "short"}, wantErr: "at least 16"},
		{name: "token and file", args: []string{"--update-token", tok, "--update-token-file", "/run/secrets/tok"}, wantErr: "exclude each other"},
		{name: "missing file", args: []string{"--query-token-file", "/nope"}, wantErr: "no such file"},
		{name: "empty file", args: []string{"--query-token-file", "/empty"}, wantErr: "is empty"},
		{name: "same tokens", args: []string{"--update-token", tok, "--query-token", tok}, wantErr: "must differ"},
		{name: "anonymous and token", args: []string{"--update-token", tok, "--allow-anonymous-updates"}, wantErr: "exclude each other"},
		{name: "load without updates", args: []string{"--allow-load", "https://data.example/"}, wantErr: "needs updates enabled"},
		{name: "load file prefix", args: []string{"--allow-anonymous-updates", "--allow-load", "file:///etc/"}, wantErr: "http:// or https://"},
		{name: "load ok", args: []string{"--allow-anonymous-updates", "--allow-load", "https://data.example/dumps/"}},
		{name: "cors star alone", args: []string{"--cors-origin", "*"}},
		{name: "cors star mixed", args: []string{"--cors-origin", "*", "--cors-origin", "https://a.example"}, wantErr: "only origin"},
		{name: "cors path", args: []string{"--cors-origin", "https://a.example/app"}, wantErr: "scheme://host"},
		{name: "tls half", args: []string{"--tls-cert", "c.pem"}, wantErr: "together"},
		{name: "bad store", args: []string{"--store", "badger"}, wantErr: "needs a path"},
		{name: "bad graph", args: []string{"--graph", "x.ttl"}, wantErr: "<iri>=<file>"},
		{name: "dataset reserved", args: []string{"--dataset", "metrics"}, wantErr: "dataset"},
		{name: "dataset ok", args: []string{"--dataset", "ds"}},
		{name: "negative timeout", args: []string{"--query-timeout", "-1s"}, wantErr: "negative"},
		{name: "log format", args: []string{"--log-format", "xml"}, wantErr: "log-format"},
		{name: "public url", args: []string{"--public-url", "example.org"}, wantErr: "public-url"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := validServe(t, tt.args...)
			err := s.Validate(readFile)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want %q", err, tt.wantErr)
			}
		})
	}

	s := validServe(t, "--update-token-file", "/run/secrets/tok")
	if err := s.Validate(readFile); err != nil || s.UpdateToken != "0123456789abcdef-file" || !s.UpdatesEnabled() {
		t.Errorf("token from file = %q, %v", s.UpdateToken, err)
	}
	if validServe(t).UpdatesEnabled() {
		t.Error("updates must be disabled by default")
	}
}

func TestLoadValidate(t *testing.T) {
	var l Load
	fs := NewLoadFlags(&l, io.Discard)
	_ = fs.Parse([]string{"a.ttl"})
	if err := l.Validate(fs.Args()); err == nil || !strings.Contains(err.Error(), "persistent store") {
		t.Errorf("memory store accepted: %v", err)
	}
	l = Load{}
	fs = NewLoadFlags(&l, io.Discard)
	_ = fs.Parse([]string{"--store", "badger:/tmp/x"})
	if err := l.Validate(fs.Args()); err == nil || !strings.Contains(err.Error(), "nothing to load") {
		t.Errorf("empty load accepted: %v", err)
	}
	l = Load{}
	fs = NewLoadFlags(&l, io.Discard)
	_ = fs.Parse([]string{"--store", "sqlite:/tmp/x.db", "--graph", "http://ex.org/g=b.nt", "a.ttl"})
	if err := l.Validate(fs.Args()); err != nil || len(l.Files) != 1 || len(l.Graphs) != 1 {
		t.Errorf("valid load rejected: %v %+v", err, l)
	}
}

func TestQueryValidate(t *testing.T) {
	readFile := func(p string) ([]byte, error) {
		if p == "q.rq" {
			return []byte("ASK {}"), nil
		}
		return nil, errors.New("missing")
	}
	run := func(stdin string, args ...string) (*Query, error) {
		var q Query
		fs := NewQueryFlags(&q, io.Discard)
		if err := fs.Parse(args); err != nil {
			t.Fatal(err)
		}
		return &q, q.Validate(fs.Args(), strings.NewReader(stdin), readFile)
	}
	if q, err := run("", "SELECT * {}"); err != nil || q.Text != "SELECT * {}" || q.Format != "auto" {
		t.Errorf("argument: %+v %v", q, err)
	}
	if q, err := run("", "--file", "q.rq", "--format", "json"); err != nil || q.Text != "ASK {}" {
		t.Errorf("file: %+v %v", q, err)
	}
	if q, err := run("ASK { ?s ?p ?o }", "-"); err != nil || q.Text != "ASK { ?s ?p ?o }" {
		t.Errorf("stdin: %+v %v", q, err)
	}
	for name, args := range map[string][]string{
		"none":       {},
		"both":       {"--file", "q.rq", "ASK {}"},
		"two args":   {"ASK", "{}"},
		"bad format": {"--format", "yaml", "ASK {}"},
		"empty":      {""},
		"no file":    {"--file", "nope.rq"},
	} {
		if _, err := run("", args...); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
