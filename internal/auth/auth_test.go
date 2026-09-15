package auth

import (
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tggo/goRDFlib/endpoint"
)

const (
	qtok = "query-token-0123456789"
	utok = "update-token-0123456789"
)

func request(authz string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/sparql", nil)
	if authz != "" {
		r.Header.Set("Authorization", authz)
	}
	return r
}

func basic(pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte("anyone:"+pass))
}

func status(err error) int {
	if err == nil {
		return 200
	}
	var he *endpoint.HTTPError
	if errors.As(err, &he) {
		return he.Status
	}
	return 403
}

func TestAuthorizer(t *testing.T) {
	read, write := endpoint.OpQuery, endpoint.OpUpdate
	tests := []struct {
		name  string
		cfg   Config
		authz string
		op    endpoint.Operation
		want  int
	}{
		{"open read", Config{}, "", read, 200},
		{"open read ignores a token nobody configured", Config{}, "Bearer whatever", read, 200},
		{"read-only write", Config{}, "", write, 403},
		{"anonymous write", Config{AllowAnonymousUpdates: true}, "", write, 200},

		{"write needs token", Config{UpdateToken: utok}, "", write, 401},
		{"write wrong token", Config{UpdateToken: utok}, "Bearer nope", write, 401},
		{"write bearer", Config{UpdateToken: utok}, "Bearer " + utok, write, 200},
		{"write basic", Config{UpdateToken: utok}, basic(utok), write, 200},
		{"graph write", Config{UpdateToken: utok}, "Bearer " + utok, endpoint.OpGraphWrite, 200},
		{"read with update token configured", Config{UpdateToken: utok}, "", read, 200},
		{"read with a wrong token is rejected", Config{UpdateToken: utok}, "Bearer nope", read, 401},

		{"query token required", Config{QueryToken: qtok}, "", read, 401},
		{"query token ok", Config{QueryToken: qtok}, "Bearer " + qtok, read, 200},
		{"graph read with query token", Config{QueryToken: qtok}, "Bearer " + qtok, endpoint.OpGraphRead, 200},
		{"update token reads", Config{QueryToken: qtok, UpdateToken: utok}, "Bearer " + utok, read, 200},
		{"query token cannot write", Config{QueryToken: qtok, UpdateToken: utok}, "Bearer " + qtok, write, 403},
		{"prefix of token", Config{UpdateToken: utok}, "Bearer " + utok[:10], write, 401},
		{"token with suffix", Config{UpdateToken: utok}, "Bearer " + utok + "x", write, 401},
		{"lowercase scheme", Config{UpdateToken: utok}, "bearer " + utok, write, 200},
		{"garbage basic", Config{UpdateToken: utok}, "Basic !!!", write, 401},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Authorizer(tt.cfg)(request(tt.authz), tt.op)
			if got := status(err); got != tt.want {
				t.Fatalf("status = %d (%v), want %d", got, err, tt.want)
			}
			if tt.want == 401 {
				var he *endpoint.HTTPError
				errors.As(err, &he)
				ch := he.Header.Values("WWW-Authenticate")
				if len(ch) != 2 || !strings.HasPrefix(ch[0], `Bearer realm="sparql-server"`) || !strings.HasPrefix(ch[1], "Basic ") {
					t.Fatalf("WWW-Authenticate = %q", ch)
				}
				if strings.Contains(he.Error(), utok) || strings.Contains(he.Error(), qtok) {
					t.Fatal("error leaks a token")
				}
			}
		})
	}
}

func TestCredentials(t *testing.T) {
	for authz, want := range map[string]string{
		"Bearer abc":          "abc",
		"Bearer   abc  ":      "abc",
		basic("secret:colon"): "secret:colon",
		"Digest abc":          "",
		"Bearer":              "",
		"":                    "",
		basic(""):             "",
	} {
		got, ok := Credentials(request(authz))
		if got != want || ok != (want != "") {
			t.Errorf("Credentials(%q) = %q, %v", authz, got, ok)
		}
	}
}
