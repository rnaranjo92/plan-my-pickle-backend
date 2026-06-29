package gateway

import (
	"io"
	"net/http"
	"strings"
)

// rtFunc is an http.RoundTripper backed by a function, so tests can return
// canned responses without a real network call.
type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// newClient builds an *http.Client whose transport is the given RoundTripper.
func newClient(rt rtFunc) *http.Client { return &http.Client{Transport: rt} }

// resp builds a canned *http.Response with the given status and body.
func resp(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}
