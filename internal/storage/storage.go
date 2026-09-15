// Package storage opens the configured goRDFlib store and loads data files
// into it.
package storage

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tggo/goRDFlib/graph"
	"github.com/tggo/goRDFlib/store"
	"github.com/tggo/goRDFlib/store/badgerstore"
	"github.com/tggo/goRDFlib/store/sqlitestore"
	"github.com/tggo/sparql-server/internal/config"
	"github.com/tggo/sparql-server/internal/rdfio"
)

// Store is an open store and the dataset over it.
type Store struct {
	Spec    config.StoreSpec
	Dataset *graph.Dataset
	closer  func() error
}

// Open opens the store described by spec, creating its directory or file.
func Open(spec config.StoreSpec) (*Store, error) {
	var (
		st     store.Store
		closer = func() error { return nil }
	)
	switch spec.Kind {
	case config.StoreMemory, "":
		st = store.NewMemoryStore()
	case config.StoreBadger:
		if err := os.MkdirAll(spec.Path, 0o750); err != nil {
			return nil, fmt.Errorf("badger store: %w", err)
		}
		bs, err := badgerstore.New(badgerstore.WithDir(spec.Path))
		if err != nil {
			if strings.Contains(err.Error(), "lock") {
				return nil, fmt.Errorf("badger store %s: %w (is another process using it?)", spec.Path, err)
			}
			return nil, fmt.Errorf("badger store %s: %w", spec.Path, err)
		}
		st, closer = bs, bs.Close
	case config.StoreSQLite:
		if dir := filepath.Dir(spec.Path); dir != "" {
			if err := os.MkdirAll(dir, 0o750); err != nil {
				return nil, fmt.Errorf("sqlite store: %w", err)
			}
		}
		ss, err := sqlitestore.New(sqlitestore.WithFile(spec.Path))
		if err != nil {
			return nil, fmt.Errorf("sqlite store %s: %w", spec.Path, err)
		}
		st, closer = ss, ss.Close
	default:
		return nil, fmt.Errorf("unknown store kind %q", spec.Kind)
	}
	return &Store{Spec: spec, Dataset: graph.NewDataset(graph.WithStore(st)), closer: closer}, nil
}

// Close closes the store. It is safe to call more than once.
func (s *Store) Close() error {
	c := s.closer
	s.closer = func() error { return nil }
	return c()
}

// TripleCount returns the number of triples in the default graph plus every
// named graph. On persistent stores it scans an index, so cache it.
func (s *Store) TripleCount() int {
	st := s.Dataset.Store()
	n := st.Len(store.DefaultGraph)
	for c := range st.Contexts(nil) {
		if !store.IsDefaultGraph(c) {
			n += st.Len(c)
		}
	}
	return n
}

// Load loads data files (paths, directories or globs, keeping the graphs the
// documents name) and graph files (each into its named graph).
func Load(ctx context.Context, s *Store, data []string, graphs []config.GraphFile, base string, log *slog.Logger) ([]rdfio.FileResult, error) {
	paths, err := rdfio.ExpandPaths(data)
	if err != nil {
		return nil, err
	}
	type job struct{ path, graph string }
	var jobs []job
	for _, p := range paths {
		jobs = append(jobs, job{path: p})
	}
	for _, g := range graphs {
		jobs = append(jobs, job{path: g.Path, graph: g.IRI})
	}
	var results []rdfio.FileResult
	for _, j := range jobs {
		if err := ctx.Err(); err != nil {
			return results, err
		}
		start := time.Now()
		res, err := rdfio.LoadFile(ctx, s.Dataset, j.path, j.graph, base)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return results, err
			}
			return results, fmt.Errorf("loading %w", err)
		}
		attrs := []any{"file", res.Path, "statements", res.Statements, "duration", time.Since(start).Round(time.Millisecond)}
		if res.Graph != "" {
			attrs = append(attrs, "graph", res.Graph)
		}
		log.Info("loaded", attrs...)
		results = append(results, res)
	}
	return results, nil
}
