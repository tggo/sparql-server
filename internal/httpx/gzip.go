// Package httpx holds the HTTP middleware the endpoint package leaves to the
// binary: gzip, CORS and a readiness gate.
package httpx

import (
	"compress/gzip"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// gzipMinSize is the smallest body worth compressing.
const gzipMinSize = 1024

var gzipPool = sync.Pool{New: func() any {
	w, _ := gzip.NewWriterLevel(nil, gzip.DefaultCompression)
	return w
}}

// Gzip compresses responses for clients that accept gzip.
//
// Bodies are buffered until gzipMinSize bytes, a Flush, or the end of the
// handler, and then sent compressed or as they are. The handler's Flush
// flushes the compressor, so streamed results still stream.
//
// A handler that panics (including with http.ErrAbortHandler, which the
// endpoint uses to abort a response that failed after it started) leaves the
// compressed stream unterminated: the gzip trailer is only written when the
// handler returns normally, so a client never mistakes a truncated result for
// a complete one.
func Gzip(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead || !AcceptsGzip(r.Header.Get("Accept-Encoding")) {
			next.ServeHTTP(w, r)
			return
		}
		gw := &gzipWriter{ResponseWriter: w}
		next.ServeHTTP(gw, r)
		// Not deferred on purpose: see the doc comment.
		gw.finish()
	})
}

// AcceptsGzip reports whether an Accept-Encoding value allows gzip.
func AcceptsGzip(header string) bool {
	star := false
	for _, part := range strings.Split(header, ",") {
		name, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		name = strings.ToLower(strings.TrimSpace(name))
		q := 1.0
		for _, p := range strings.Split(params, ";") {
			k, v, ok := strings.Cut(strings.TrimSpace(p), "=")
			if ok && strings.EqualFold(strings.TrimSpace(k), "q") {
				if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
					q = f
				}
			}
		}
		switch name {
		case "gzip", "x-gzip":
			return q > 0
		case "*":
			star = q > 0
		}
	}
	return star
}

type gzipWriter struct {
	http.ResponseWriter
	status   int
	buf      []byte
	decided  bool
	compress bool
	gz       *gzip.Writer
}

func (g *gzipWriter) WriteHeader(code int) {
	if g.decided || g.status != 0 {
		return
	}
	if code < 200 {
		// Informational responses pass straight through.
		g.ResponseWriter.WriteHeader(code)
		return
	}
	g.status = code
	h := g.Header()
	if code == http.StatusNoContent || code == http.StatusNotModified || h.Get("Content-Encoding") != "" {
		g.decide(false)
		return
	}
	if cl := h.Get("Content-Length"); cl != "" {
		if n, err := strconv.ParseInt(cl, 10, 64); err == nil {
			g.decide(n >= gzipMinSize)
		}
	}
}

func (g *gzipWriter) Write(p []byte) (int, error) {
	if g.status == 0 {
		g.WriteHeader(http.StatusOK)
	}
	if g.decided {
		if g.compress {
			return g.gz.Write(p)
		}
		return g.ResponseWriter.Write(p)
	}
	g.buf = append(g.buf, p...)
	if len(g.buf) >= gzipMinSize {
		if err := g.decideAndDrain(true); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// decide sends the header, compressed or not.
func (g *gzipWriter) decide(compress bool) {
	g.decided = true
	g.compress = compress
	h := g.Header()
	h.Add("Vary", "Accept-Encoding")
	if compress {
		if h.Get("Content-Type") == "" && len(g.buf) > 0 {
			h.Set("Content-Type", http.DetectContentType(g.buf))
		}
		h.Del("Content-Length")
		h.Set("Content-Encoding", "gzip")
		g.gz = gzipPool.Get().(*gzip.Writer)
		g.gz.Reset(g.ResponseWriter)
	}
	status := g.status
	if status == 0 {
		status = http.StatusOK
	}
	g.ResponseWriter.WriteHeader(status)
}

func (g *gzipWriter) decideAndDrain(compress bool) error {
	g.decide(compress)
	buf := g.buf
	g.buf = nil
	if len(buf) == 0 {
		return nil
	}
	var err error
	if compress {
		_, err = g.gz.Write(buf)
	} else {
		_, err = g.ResponseWriter.Write(buf)
	}
	return err
}

// Flush sends what is buffered, compressed, and flushes the connection.
func (g *gzipWriter) Flush() {
	if !g.decided {
		if g.status == 0 {
			g.status = http.StatusOK
		}
		_ = g.decideAndDrain(true)
	}
	if g.compress {
		_ = g.gz.Flush()
	}
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the connection.
func (g *gzipWriter) Unwrap() http.ResponseWriter { return g.ResponseWriter }

func (g *gzipWriter) finish() {
	if !g.decided {
		if g.status == 0 && len(g.buf) == 0 {
			return // nothing written: net/http sends its default response
		}
		_ = g.decideAndDrain(false)
	}
	if g.compress {
		_ = g.gz.Close()
		g.gz.Reset(nil)
		gzipPool.Put(g.gz)
		g.gz = nil
	}
}
