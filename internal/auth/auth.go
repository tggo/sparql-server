// Package auth is the token check in front of the SPARQL endpoint.
//
// A token is presented as "Authorization: Bearer <token>", or as the password
// of HTTP Basic authentication (any user name) for clients that only speak
// Basic. The update token allows everything; the query token allows queries
// and graph reads.
package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"

	"github.com/tggo/goRDFlib/endpoint"
)

// Realm is sent in WWW-Authenticate challenges.
const Realm = "sparql-server"

// Config is the token configuration.
type Config struct {
	// QueryToken, when set, is required for queries and graph reads (the
	// update token is accepted too).
	QueryToken string
	// UpdateToken, when set, is required for updates and graph writes.
	UpdateToken string
	// AllowAnonymousUpdates allows writes without a token.
	AllowAnonymousUpdates bool
}

// Authorizer returns the endpoint.Authorizer for c.
func Authorizer(c Config) endpoint.Authorizer {
	q, u := digest(c.QueryToken), digest(c.UpdateToken)
	return func(r *http.Request, op endpoint.Operation) error {
		tok, present := Credentials(r)
		isUpdate := present && u != nil && equal(tok, u)
		isQuery := present && q != nil && equal(tok, q)
		badToken := present && !isUpdate && !isQuery && (q != nil || u != nil)

		write := op == endpoint.OpUpdate || op == endpoint.OpGraphWrite
		if !write {
			switch {
			case q == nil && !badToken:
				return nil
			case isQuery || isUpdate:
				return nil
			case badToken:
				return challenge("invalid_token", "invalid token")
			}
			return challenge("", "this endpoint requires a token for queries")
		}

		switch {
		case isUpdate:
			return nil
		case badToken:
			return challenge("invalid_token", "invalid token")
		case c.AllowAnonymousUpdates:
			return nil
		case isQuery:
			return &endpoint.HTTPError{Status: http.StatusForbidden, Err: errors.New("the query token does not allow updates")}
		case u == nil:
			return &endpoint.HTTPError{Status: http.StatusForbidden, Err: endpoint.ErrReadOnly}
		}
		return challenge("", "updates require the update token")
	}
}

// Credentials extracts the presented token from a Bearer or Basic
// Authorization header.
func Credentials(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	scheme, rest, ok := strings.Cut(h, " ")
	if !ok {
		return "", false
	}
	rest = strings.TrimSpace(rest)
	switch strings.ToLower(scheme) {
	case "bearer":
		return rest, rest != ""
	case "basic":
		raw, err := base64.StdEncoding.DecodeString(rest)
		if err != nil {
			return "", false
		}
		_, pass, ok := strings.Cut(string(raw), ":")
		return pass, ok && pass != ""
	}
	return "", false
}

func digest(token string) []byte {
	if token == "" {
		return nil
	}
	d := sha256.Sum256([]byte(token))
	return d[:]
}

// equal compares in constant time. Hashing first makes the comparison
// independent of the token length as well.
func equal(presented string, want []byte) bool {
	d := sha256.Sum256([]byte(presented))
	return subtle.ConstantTimeCompare(d[:], want) == 1
}

func challenge(errCode, msg string) error {
	bearer := `Bearer realm="` + Realm + `"`
	if errCode != "" {
		bearer += `, error="` + errCode + `"`
	}
	h := http.Header{}
	h.Add("WWW-Authenticate", bearer)
	h.Add("WWW-Authenticate", `Basic realm="`+Realm+`", charset="UTF-8"`)
	return &endpoint.HTTPError{
		Status: http.StatusUnauthorized,
		Header: h,
		Err:    errors.New(msg),
	}
}
