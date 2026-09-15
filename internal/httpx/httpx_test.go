package httpx

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAcceptsGzip(t *testing.T) {
	for header, want := range map[string]bool{
		"":                    false,
		"gzip":                true,
		"deflate, gzip;q=0.5": true,
		"gzip;q=0":            false,
		"GZIP":                true,
		"br":                  false,
		"*":                   true,
		"*;q=0":               false,
		"x-gzip":              true,
		"br, *;q=0.1":         true,
	} {
		if got := AcceptsGzip(header); got != want {
			t.Errorf("AcceptsGzip(%q) = %v", header, got)
		}
	}
}

func get(t *testing.T, h http.Handler, method, acceptEncoding string) *http.Response {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	req, _ := http.NewRequest(method, srv.URL, nil)
	if acceptEncoding != "" {
		req.Header.Set("Accept-Encoding", acceptEncoding)
	}
	// A Transport with compression disabled shows the raw encoding.
	tr := &http.Transport{DisableCompression: true}
	t.Cleanup(tr.CloseIdleConnections)
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestGzip(t *testing.T) {
	big := strings.Repeat("row,value\n", 500)
	bigHandler := Gzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/csv")
		for i := 0; i < len(big); i += 100 {
			io.WriteString(w, big[i:i+100])
		}
	}))

	resp := get(t, bigHandler, http.MethodGet, "gzip")
	if resp.Header.Get("Content-Encoding") != "gzip" || resp.Header.Get("Content-Type") != "text/csv" {
		t.Fatalf("headers = %v", resp.Header)
	}
	if !strings.Contains(strings.Join(resp.Header.Values("Vary"), ","), "Accept-Encoding") {
		t.Error("missing Vary: Accept-Encoding")
	}
	zr, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(zr)
	if err != nil || string(body) != big {
		t.Fatalf("decompressed %d bytes, err %v", len(body), err)
	}

	resp = get(t, bigHandler, http.MethodGet, "")
	if resp.Header.Get("Content-Encoding") != "" {
		t.Error("compressed without Accept-Encoding")
	}

	resp = get(t, bigHandler, http.MethodHead, "gzip")
	if resp.Header.Get("Content-Encoding") != "" {
		t.Error("HEAD compressed")
	}

	small := Gzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, "small")
	}))
	resp = get(t, small, http.MethodGet, "gzip")
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated || resp.Header.Get("Content-Encoding") != "" || string(b) != "small" {
		t.Errorf("small: %d %v %q", resp.StatusCode, resp.Header, b)
	}

	noContent := Gzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	resp = get(t, noContent, http.MethodGet, "gzip")
	if resp.StatusCode != http.StatusNoContent || resp.Header.Get("Content-Encoding") != "" {
		t.Errorf("204: %d %v", resp.StatusCode, resp.Header)
	}

	withLength := Gzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "5000")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, strings.Repeat("a", 5000))
	}))
	resp = get(t, withLength, http.MethodGet, "gzip")
	if resp.Header.Get("Content-Encoding") != "gzip" || resp.Header.Get("Content-Length") == "5000" {
		t.Errorf("Content-Length response: %v", resp.Header)
	}
}

// TestGzipAbortIsNotAComplete checks the property the endpoint relies on: a
// handler that aborts after the response started must not produce a valid
// gzip stream, or the client would take a truncated result for a whole one.
func TestGzipAbortIsNotComplete(t *testing.T) {
	h := Gzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/csv")
		io.WriteString(w, strings.Repeat("partial,row\n", 400))
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	}))
	resp := get(t, h, http.MethodGet, "gzip")
	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("headers = %v", resp.Header)
	}
	zr, err := gzip.NewReader(resp.Body)
	if err != nil {
		return // the stream broke even earlier: fine
	}
	if _, err := io.ReadAll(zr); err == nil {
		t.Fatal("an aborted response decompressed without error")
	}
}

func TestGzipFlushStreams(t *testing.T) {
	release := make(chan struct{})
	h := Gzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "first chunk\n")
		w.(http.Flusher).Flush()
		<-release
		io.WriteString(w, "second chunk\n")
	}))
	resp := get(t, h, http.MethodGet, "gzip")
	defer close(release)
	zr, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len("first chunk\n"))
	done := make(chan error, 1)
	go func() { _, err := io.ReadFull(zr, buf); done <- err }()
	select {
	case err := <-done:
		if err != nil || string(buf) != "first chunk\n" {
			t.Fatalf("read %q, %v", buf, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("flushed data did not reach the client")
	}
}

func TestCORS(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			t.Error("preflight reached the handler")
		}
		w.Write([]byte("ok"))
	})
	h := CORS([]string{"https://app.example"}, next)

	pre := httptest.NewRequest(http.MethodOptions, "/sparql", nil)
	pre.Header.Set("Origin", "https://app.example")
	pre.Header.Set("Access-Control-Request-Method", "POST")
	pre.Header.Set("Access-Control-Request-Headers", "authorization, content-type")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, pre)
	if rec.Code != http.StatusNoContent || rec.Header().Get("Access-Control-Allow-Origin") != "https://app.example" ||
		!strings.Contains(rec.Header().Get("Access-Control-Allow-Headers"), "Authorization") {
		t.Errorf("preflight: %d %v", rec.Code, rec.Header())
	}
	if rec.Header().Get("Access-Control-Allow-Credentials") != "" {
		t.Error("credentials must not be allowed")
	}

	other := httptest.NewRequest(http.MethodGet, "/sparql", nil)
	other.Header.Set("Origin", "https://evil.example")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, other)
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("disallowed origin got CORS headers")
	}

	star := CORS([]string{"*"}, next)
	rec = httptest.NewRecorder()
	star.ServeHTTP(rec, other)
	if rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("wildcard: %v", rec.Header())
	}

	if CORS(nil, next) == nil {
		t.Error("nil handler")
	}
}

func TestGate(t *testing.T) {
	var ready atomic.Bool
	h := Gate(&ready, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sparql", nil))
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Errorf("not ready: %d %v", rec.Code, rec.Header())
	}
	ready.Store(true)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sparql", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("ready: %d", rec.Code)
	}
}
