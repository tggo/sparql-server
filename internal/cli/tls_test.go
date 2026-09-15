package cli

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func selfSigned(t *testing.T) (certFile, keyFile string, pool *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile, keyFile = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600)
	cert, _ := x509.ParseCertificate(der)
	pool = x509.NewCertPool()
	pool.AddCert(cert)
	return certFile, keyFile, pool
}

func TestTLS(t *testing.T) {
	certFile, keyFile, pool := selfSigned(t)
	logs := &syncBuffer{}
	addr := make(chan string, 1)
	env := Env{Stdout: io.Discard, Stderr: logs}
	env.ServerOptions.OnListen = func(a string) { addr <- a }
	ctx := t.Context()
	done := make(chan int, 1)
	go func() {
		done <- Main(ctx, []string{"serve", "--listen", "127.0.0.1:0", "--tls-cert", certFile, "--tls-key", keyFile,
			"--data", filepath.Join(dataDir(t), "people.ttl")}, env)
	}()
	var a string
	select {
	case a = <-addr:
	case code := <-done:
		t.Fatalf("exited %d: %s", code, logs)
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	u := "https://" + a + "/sparql?" + url.Values{"query": {namesQuery}}.Encode()

	var resp *http.Response
	var err error
	for range 100 {
		req, _ := http.NewRequest(http.MethodGet, u, nil)
		req.Header.Set("Accept", "text/csv")
		resp, err = client.Do(req)
		if err == nil && resp.StatusCode == 200 {
			break
		}
		if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.TLS == nil || string(body) != "name\r\nAlice\r\nBob\r\n" {
		t.Errorf("https query: tls=%v body=%q", resp.TLS != nil, body)
	}
	if !strings.Contains(logs.String(), "scheme=https") {
		t.Errorf("startup log lacks scheme=https:\n%s", logs)
	}
	// Plain HTTP to the TLS port is not served.
	if r, err := http.Get("http://" + a + "/healthz"); err == nil {
		b, _ := io.ReadAll(r.Body)
		r.Body.Close()
		if r.StatusCode == 200 && string(b) == "ok\n" {
			t.Error("plain HTTP served on the TLS listener")
		}
	}
}
