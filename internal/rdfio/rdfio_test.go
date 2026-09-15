package rdfio

import (
	"compress/gzip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tggo/goRDFlib/graph"
	"github.com/tggo/goRDFlib/store"
	"github.com/tggo/goRDFlib/term"
)

func write(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func graphLen(ds *graph.Dataset, iri string) int {
	if iri == "" {
		return ds.Store().Len(store.DefaultGraph)
	}
	return ds.Store().Len(term.NewURIRefUnsafe(iri))
}

func TestFormatDetection(t *testing.T) {
	for path, want := range map[string]Format{
		"a.ttl": Turtle, "A.NT": NTriples, "x/y.nq": NQuads, "b.trig": TriG, "c.rdf": RDFXML, "d.jsonld": JSONLD, "dump.nt.gz": NTriples,
	} {
		if got, ok := FormatFromPath(path); !ok || got != want {
			t.Errorf("FormatFromPath(%q) = %q, %v", path, got, ok)
		}
	}
	if _, ok := FormatFromPath("a.txt"); ok {
		t.Error("a.txt detected")
	}
	if f, ok := FormatFromMediaType("text/turtle; charset=utf-8"); !ok || f != Turtle {
		t.Error("text/turtle")
	}
	if _, ok := FormatFromMediaType("text/plain"); ok {
		t.Error("text/plain must not be trusted")
	}
}

func TestLoadFileGraphs(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	ds := graph.NewDataset()

	nq := write(t, dir, "a.nq", `<http://ex/s> <http://ex/p> "1" <http://ex/g1> .
<http://ex/s> <http://ex/p> "2" .
_:b <http://ex/p> "3" <http://ex/g2> .
`)
	res, err := LoadFile(ctx, ds, nq, "", "")
	if err != nil || res.Statements != 3 {
		t.Fatalf("nq: %+v %v", res, err)
	}
	if graphLen(ds, "") != 1 || graphLen(ds, "http://ex/g1") != 1 || graphLen(ds, "http://ex/g2") != 1 {
		t.Errorf("nq graphs: default %d g1 %d g2 %d", graphLen(ds, ""), graphLen(ds, "http://ex/g1"), graphLen(ds, "http://ex/g2"))
	}

	trig := write(t, dir, "b.trig", `@prefix ex: <http://ex/> . ex:g3 { ex:s ex:p "4" } ex:s ex:p "5" .`)
	if _, err := LoadFile(ctx, ds, trig, "", ""); err != nil {
		t.Fatal(err)
	}
	if graphLen(ds, "http://ex/g3") != 1 || graphLen(ds, "") != 2 {
		t.Errorf("trig graphs: g3 %d default %d", graphLen(ds, "http://ex/g3"), graphLen(ds, ""))
	}

	// A target graph overrides the document's graphs.
	if _, err := LoadFile(ctx, ds, trig, "http://ex/target", ""); err != nil {
		t.Fatal(err)
	}
	if graphLen(ds, "http://ex/target") != 2 {
		t.Errorf("target graph has %d triples", graphLen(ds, "http://ex/target"))
	}

	// Relative IRIs resolve against the file URL unless a base is given.
	rel := write(t, dir, "rel.ttl", `<thing> <http://ex/p> "6" .`)
	if _, err := LoadFile(ctx, ds, rel, "http://ex/rel", "http://base.example/"); err != nil {
		t.Fatal(err)
	}
	found := false
	for tr := range ds.GetContext(term.NewURIRefUnsafe("http://ex/rel")).Triples(nil, nil, nil) {
		found = tr.Subject.String() == "http://base.example/thing"
	}
	if !found {
		t.Error("base IRI not applied")
	}

	// gzip-compressed N-Triples.
	gzPath := filepath.Join(dir, "c.nt.gz")
	f, _ := os.Create(gzPath)
	zw := gzip.NewWriter(f)
	zw.Write([]byte("<http://ex/z> <http://ex/p> \"7\" .\n"))
	zw.Close()
	f.Close()
	if res, err := LoadFile(ctx, ds, gzPath, "http://ex/gz", ""); err != nil || res.Statements != 1 {
		t.Errorf("gz: %+v %v", res, err)
	}
}

func TestLoadFileErrors(t *testing.T) {
	dir := t.TempDir()
	ds := graph.NewDataset()
	ctx := context.Background()
	if _, err := LoadFile(ctx, ds, write(t, dir, "x.txt", "hello"), "", ""); err == nil || !strings.Contains(err.Error(), "unknown RDF format") {
		t.Errorf("txt: %v", err)
	}
	bad := write(t, dir, "bad.ttl", `@prefix ex: <http://ex/> . ex:a ex:p "ok" . ex:b ex:p `)
	if _, err := LoadFile(ctx, ds, bad, "", ""); err == nil {
		t.Error("malformed turtle accepted")
	}
	if ds.Store().Len(store.DefaultGraph) != 0 {
		t.Error("a malformed Turtle file added triples")
	}
	remote := write(t, dir, "r.jsonld", `{"@context": "https://schema.org/", "@id": "http://ex/a", "name": "x"}`)
	if _, err := LoadFile(ctx, ds, remote, "", ""); !errors.Is(err, ErrRemoteContext) {
		t.Errorf("remote context: %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := LoadFile(cancelled, ds, write(t, dir, "ok.nt", `<http://ex/a> <http://ex/p> "1" .`), "", ""); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
}

func TestExpandPaths(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.ttl", "")
	write(t, dir, "b.nt", "")
	write(t, dir, "readme.md", "")
	sub := filepath.Join(dir, "sub")
	os.Mkdir(sub, 0o755)
	write(t, sub, "c.ttl", "")

	got, err := ExpandPaths([]string{dir})
	if err != nil || len(got) != 2 {
		t.Errorf("directory: %v %v", got, err)
	}
	got, err = ExpandPaths([]string{filepath.Join(dir, "*.ttl"), filepath.Join(sub, "c.ttl")})
	if err != nil || len(got) != 2 {
		t.Errorf("glob: %v %v", got, err)
	}
	for _, bad := range []string{filepath.Join(dir, "*.nq"), filepath.Join(dir, "missing.ttl"), filepath.Join(dir, "readme.md", "x")} {
		if _, err := ExpandPaths([]string{bad}); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	empty := t.TempDir()
	if _, err := ExpandPaths([]string{empty}); err == nil {
		t.Error("empty directory accepted")
	}
}
