// Package loader implements the sparql.Loader behind SPARQL Update LOAD: it
// fetches only http(s) URLs under configured prefixes, follows redirects only
// within them, bounds size and time, and never reads local files.
package loader

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tggo/goRDFlib/graph"
	"github.com/tggo/goRDFlib/plugin"
	"github.com/tggo/sparql-server/internal/rdfio"
)

// ErrNotAllowed is returned for a URL outside the allowlist.
var ErrNotAllowed = errors.New("LOAD source is not in the allowlist")

// ErrTooLarge is returned when a document exceeds the size limit.
var ErrTooLarge = errors.New("LOAD document exceeds the size limit")

type prefix struct {
	scheme, host, port, path string
}

// Allowlist is a sparql.Loader restricted to URL prefixes.
type Allowlist struct {
	prefixes []prefix
	client   *http.Client
	maxBytes int64
	timeout  time.Duration
}

// New returns an allowlisting loader. Each prefix is an http:// or https://
// URL; a URL is allowed when its scheme, host and port equal a prefix's and
// its path is the prefix path or below it at a segment boundary
// ("https://ex.org/data" allows /data and /data/x, not /database).
// transport may be nil for http.DefaultTransport's settings.
func New(prefixes []string, timeout time.Duration, maxBytes int64, transport http.RoundTripper) (*Allowlist, error) {
	a := &Allowlist{maxBytes: maxBytes, timeout: timeout}
	for _, p := range prefixes {
		u, err := url.Parse(p)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
			return nil, fmt.Errorf("allow-load %q: want an http(s) URL prefix without user info", p)
		}
		pp, err := normalize(u)
		if err != nil {
			return nil, fmt.Errorf("allow-load %q: %w", p, err)
		}
		a.prefixes = append(a.prefixes, pp)
	}
	if transport == nil {
		transport = http.DefaultTransport
	}
	a.client = &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("LOAD: too many redirects")
			}
			if !a.Allowed(req.URL.String()) {
				return fmt.Errorf("%w: redirect to %s", ErrNotAllowed, req.URL.Redacted())
			}
			return nil
		},
	}
	return a, nil
}

func normalize(u *url.URL) (prefix, error) {
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[u.Scheme]
	}
	p := u.Path
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." {
			return prefix{}, errors.New("dot segments are not allowed in the path")
		}
	}
	if strings.Contains(strings.ToLower(u.RawPath), "%2f") || strings.Contains(strings.ToLower(u.EscapedPath()), "%2e") {
		return prefix{}, errors.New("encoded slashes or dots are not allowed in the path")
	}
	return prefix{scheme: u.Scheme, host: host, port: port, path: p}, nil
}

// Allowed reports whether raw may be loaded.
func (a *Allowlist) Allowed(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Host == "" || !u.IsAbs() {
		return false
	}
	c, err := normalize(u)
	if err != nil {
		return false
	}
	for _, p := range a.prefixes {
		if p.scheme != c.scheme || p.host != c.host || p.port != c.port {
			continue
		}
		if p.path == "" || p.path == "/" || c.path == p.path ||
			strings.HasSuffix(p.path, "/") && strings.HasPrefix(c.path, p.path) ||
			strings.HasPrefix(c.path, p.path+"/") {
			return true
		}
	}
	return false
}

// Load fetches uri and parses it into g. It implements sparql.Loader.
func (a *Allowlist) Load(ctx context.Context, g *graph.Graph, uri string) error {
	if !a.Allowed(uri) {
		return fmt.Errorf("%w: %s", ErrNotAllowed, uri)
	}
	if a.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, a.timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, uri, nil)
	if err != nil {
		return fmt.Errorf("LOAD %s: %w", uri, err)
	}
	req.Header.Set("Accept", "text/turtle, application/n-triples, application/n-quads, application/trig, application/rdf+xml, application/ld+json;q=0.9, */*;q=0.1")
	req.Header.Set("User-Agent", "sparql-server")
	resp, err := a.client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) && errors.Is(ue.Err, ErrNotAllowed) {
			return ue.Err
		}
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return fmt.Errorf("LOAD %s: timed out", uri)
		}
		return fmt.Errorf("LOAD %s: %w", uri, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("LOAD %s: HTTP %d", uri, resp.StatusCode)
	}
	if a.maxBytes > 0 && resp.ContentLength > a.maxBytes {
		return fmt.Errorf("%w (%d bytes): %s", ErrTooLarge, resp.ContentLength, uri)
	}
	body := io.Reader(resp.Body)
	limited := &limitReader{r: resp.Body, left: a.maxBytes}
	if a.maxBytes > 0 {
		body = limited
	}

	final := resp.Request.URL
	f, ok := rdfio.FormatFromMediaType(resp.Header.Get("Content-Type"))
	if !ok {
		f, ok = rdfio.FormatFromPath(final.Path)
	}
	if !ok {
		head := make([]byte, 512)
		n, _ := io.ReadFull(body, head)
		name, sniffed := plugin.FormatFromContent(head[:n])
		if !sniffed {
			return fmt.Errorf("LOAD %s: cannot tell the RDF format (Content-Type %q)", uri, resp.Header.Get("Content-Type"))
		}
		f = rdfio.Format(name)
		body = io.MultiReader(bytes.NewReader(head[:n]), body)
	}
	if _, err := rdfio.ParseInto(ctx, g, body, f, final.String()); err != nil {
		// A parser may report the truncated input before the read error.
		if limited.exceeded || errors.Is(err, ErrTooLarge) {
			return fmt.Errorf("%w: %s", ErrTooLarge, uri)
		}
		return fmt.Errorf("LOAD %s: %w", uri, err)
	}
	return nil
}

// limitReader fails with ErrTooLarge instead of returning a silent EOF, so a
// truncated document is never parsed as a complete one.
type limitReader struct {
	r        io.Reader
	left     int64
	exceeded bool
}

func (l *limitReader) Read(p []byte) (int, error) {
	if l.left <= 0 {
		// Allow the reader to report a clean EOF exactly at the limit.
		var one [1]byte
		n, err := l.r.Read(one[:])
		if n > 0 {
			l.exceeded = true
			return 0, ErrTooLarge
		}
		return 0, err
	}
	if int64(len(p)) > l.left {
		p = p[:l.left]
	}
	n, err := l.r.Read(p)
	l.left -= int64(n)
	return n, err
}
