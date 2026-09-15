// Package rdfio parses RDF documents into a store-backed dataset: format
// detection by file extension or media type, batched writes, named graphs
// from N-Quads and TriG, and no remote JSON-LD contexts.
package rdfio

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/piprate/json-gold/ld"
	"github.com/tggo/goRDFlib/graph"
	"github.com/tggo/goRDFlib/jsonld"
	"github.com/tggo/goRDFlib/nq"
	"github.com/tggo/goRDFlib/nt"
	"github.com/tggo/goRDFlib/plugin"
	"github.com/tggo/goRDFlib/rdfxml"
	"github.com/tggo/goRDFlib/store"
	"github.com/tggo/goRDFlib/term"
	"github.com/tggo/goRDFlib/trig"
	"github.com/tggo/goRDFlib/turtle"
)

// Format is an RDF syntax, named as goRDFlib's plugin package names it.
type Format string

// The formats.
const (
	Turtle   Format = "turtle"
	NTriples Format = "nt"
	NQuads   Format = "nquads"
	TriG     Format = "trig"
	RDFXML   Format = "xml"
	JSONLD   Format = "json-ld"
)

// Quads reports whether the format carries named graphs.
func (f Format) Quads() bool { return f == NQuads || f == TriG }

// FormatFromPath detects the format from a file name. A trailing .gz is
// ignored for detection (the file is decompressed when read).
func FormatFromPath(path string) (Format, bool) {
	name := strings.TrimSuffix(strings.ToLower(path), ".gz")
	f, ok := plugin.FormatFromFilename(name)
	return Format(f), ok
}

// FormatFromMediaType detects the format from a Content-Type header value.
// text/plain is not accepted: it is too often a server's default.
func FormatFromMediaType(ct string) (Format, bool) {
	mt, _, _ := strings.Cut(ct, ";")
	mt = strings.ToLower(strings.TrimSpace(mt))
	if mt == "text/plain" || mt == "" {
		return "", false
	}
	f, ok := plugin.FormatFromMIME(mt)
	return Format(f), ok
}

// ErrRemoteContext is returned when a JSON-LD document references a remote
// @context. Loading it would make the server fetch a URL named by the data.
var ErrRemoteContext = errors.New("remote JSON-LD contexts are not loaded")

type noRemoteContexts struct{}

func (noRemoteContexts) LoadDocument(u string) (*ld.RemoteDocument, error) {
	return nil, fmt.Errorf("%w: %s", ErrRemoteContext, u)
}

// batchSize is how many quads are handed to the store at once. Badger and
// SQLite write one transaction per AddN call.
const batchSize = 10_000

// Options control a parse.
type Options struct {
	// Base is the base IRI for relative IRIs. Empty means the parser default.
	Base string
	// Target, when set, receives every triple regardless of the graph the
	// document puts it in. Nil keeps the document's graphs (the default graph
	// for triple formats).
	Target term.Term
}

// batcher accumulates quads and flushes them into a store.
type batcher struct {
	ctx   context.Context
	st    store.Store
	buf   []term.Quad
	count int
}

func (b *batcher) add(s term.Subject, p term.URIRef, o term.Term, g term.Term) error {
	var gs term.Subject = store.DefaultGraph
	if g != nil {
		if sub, ok := g.(term.Subject); ok && !store.IsDefaultGraph(g) {
			gs = sub
		}
	}
	b.buf = append(b.buf, term.Quad{Triple: term.Triple{Subject: s, Predicate: p, Object: o}, Graph: gs})
	b.count++
	if len(b.buf) >= batchSize {
		return b.flush()
	}
	return nil
}

func (b *batcher) flush() error {
	if err := b.ctx.Err(); err != nil {
		return err
	}
	if len(b.buf) > 0 {
		b.st.AddN(b.buf)
		b.buf = b.buf[:0]
	}
	return nil
}

// Parse reads a document in format f from r into ds and returns the number
// of statements parsed. Line-based formats are streamed in batches; the
// others are parsed into memory first, so a malformed document adds nothing.
// ctx is checked between batches.
func Parse(ctx context.Context, ds *graph.Dataset, r io.Reader, f Format, opt Options) (int, error) {
	b := &batcher{ctx: ctx, st: ds.Store()}
	target := func(g term.Term) term.Term {
		if opt.Target != nil {
			return opt.Target
		}
		return g
	}
	var err error
	switch f {
	case NTriples:
		var opts []nt.Option
		if opt.Base != "" {
			opts = append(opts, nt.WithBase(opt.Base))
		}
		err = nt.ParseStream(r, func(s term.Subject, p term.URIRef, o term.Term) error {
			return b.add(s, p, o, target(nil))
		}, opts...)
	case NQuads:
		var opts []nq.Option
		if opt.Base != "" {
			opts = append(opts, nq.WithBase(opt.Base))
		}
		err = nq.ParseStream(r, func(s term.Subject, p term.URIRef, o term.Term, g term.Term) error {
			return b.add(s, p, o, target(g))
		}, opts...)
	case TriG:
		tmp := graph.NewDataset()
		var opts []trig.Option
		if opt.Base != "" {
			opts = append(opts, trig.WithBase(opt.Base))
		}
		if err = trig.ParseDataset(tmp, r, opts...); err == nil {
			for q := range tmp.Quads(nil, nil, nil) {
				if err = b.add(q.Subject, q.Predicate, q.Object, target(q.Graph)); err != nil {
					break
				}
			}
		}
	case Turtle, RDFXML, JSONLD:
		g := graph.NewGraph()
		if err = parseGraph(g, r, f, opt.Base); err == nil {
			for t := range g.Triples(nil, nil, nil) {
				if err = b.add(t.Subject, t.Predicate, t.Object, target(nil)); err != nil {
					break
				}
			}
		}
	default:
		return 0, fmt.Errorf("unsupported RDF format %q", f)
	}
	if err == nil {
		err = b.flush()
	}
	return b.count, err
}

func parseGraph(g *graph.Graph, r io.Reader, f Format, base string) error {
	switch f {
	case Turtle:
		var opts []turtle.Option
		if base != "" {
			opts = append(opts, turtle.WithBase(base))
		}
		return turtle.Parse(g, r, opts...)
	case RDFXML:
		var opts []rdfxml.Option
		if base != "" {
			opts = append(opts, rdfxml.WithBase(base))
		}
		return rdfxml.Parse(g, r, opts...)
	case JSONLD:
		opts := []jsonld.Option{jsonld.WithDocumentLoader(noRemoteContexts{})}
		if base != "" {
			opts = append(opts, jsonld.WithBase(base))
		}
		return jsonld.Parse(g, r, opts...)
	}
	return fmt.Errorf("unsupported RDF format %q", f)
}

// ParseInto parses a document into one graph g (the target of a SPARQL LOAD):
// every triple goes to g whatever graph the document names. The document is
// parsed completely before g is touched.
func ParseInto(ctx context.Context, g *graph.Graph, r io.Reader, f Format, base string) (int, error) {
	tmp := graph.NewDataset()
	n, err := Parse(ctx, tmp, r, f, Options{Base: base, Target: store.DefaultGraph})
	if err != nil {
		return 0, err
	}
	b := &batcher{ctx: ctx, st: g.Store()}
	id := g.Identifier()
	for t := range tmp.DefaultContext().Triples(nil, nil, nil) {
		if err := b.add(t.Subject, t.Predicate, t.Object, id); err != nil {
			return 0, err
		}
	}
	return n, b.flush()
}

// FileResult reports one loaded file.
type FileResult struct {
	Path       string
	Graph      string // empty for the document's own graphs
	Statements int
}

// ExpandPaths turns --data values into file paths: a glob is expanded, a
// directory contributes its files with a known RDF extension (not
// recursively), a plain path is kept. A pattern that matches nothing is an
// error, so a typo does not start an empty server.
func ExpandPaths(patterns []string) ([]string, error) {
	var out []string
	for _, p := range patterns {
		matches := []string{p}
		if strings.ContainsAny(p, "*?[") {
			m, err := filepath.Glob(p)
			if err != nil {
				return nil, fmt.Errorf("data %q: %w", p, err)
			}
			if len(m) == 0 {
				return nil, fmt.Errorf("data %q matches no files", p)
			}
			matches = m
		}
		for _, m := range matches {
			fi, err := os.Stat(m)
			if err != nil {
				return nil, fmt.Errorf("data: %w", err)
			}
			if !fi.IsDir() {
				out = append(out, m)
				continue
			}
			entries, err := os.ReadDir(m)
			if err != nil {
				return nil, fmt.Errorf("data: %w", err)
			}
			var files []string
			for _, e := range entries {
				if e.Type().IsRegular() {
					if _, ok := FormatFromPath(e.Name()); ok {
						files = append(files, filepath.Join(m, e.Name()))
					}
				}
			}
			if len(files) == 0 {
				return nil, fmt.Errorf("data %q: directory has no RDF files", m)
			}
			sort.Strings(files)
			out = append(out, files...)
		}
	}
	return out, nil
}

// LoadFile parses the file at path into ds. graphIRI, when not empty, is the
// named graph that receives every triple. base overrides the base IRI, which
// is otherwise the file's own file:// URL.
func LoadFile(ctx context.Context, ds *graph.Dataset, path, graphIRI, base string) (FileResult, error) {
	res := FileResult{Path: path, Graph: graphIRI}
	f, ok := FormatFromPath(path)
	if !ok {
		return res, fmt.Errorf("%s: unknown RDF format (extensions: .ttl .nt .nq .trig .rdf .owl .xml .jsonld .json, optionally .gz)", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return res, err
	}
	defer file.Close()
	var r io.Reader = file
	if strings.HasSuffix(strings.ToLower(path), ".gz") {
		zr, err := gzip.NewReader(file)
		if err != nil {
			return res, fmt.Errorf("%s: %w", path, err)
		}
		defer zr.Close()
		r = zr
	}
	if base == "" {
		if abs, err := filepath.Abs(path); err == nil {
			base = (&url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}).String()
		}
	}
	opt := Options{Base: base}
	if graphIRI != "" {
		u, err := term.NewURIRef(graphIRI)
		if err != nil {
			return res, fmt.Errorf("graph %q: %w", graphIRI, err)
		}
		opt.Target = u
	}
	res.Statements, err = Parse(ctx, ds, r, f, opt)
	if err != nil {
		return res, fmt.Errorf("%s: %w", path, err)
	}
	return res, nil
}
