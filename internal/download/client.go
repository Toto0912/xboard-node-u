// Package download provides the HTTPS transport used for public release assets.
package download

import (
	"errors"
	"net/http"
	"time"
)

type httpsTransport struct{ base http.RoundTripper }

func (t httpsTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "https" {
		return nil, errors.New("download requires HTTPS")
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

func Client(timeout time.Duration, base http.RoundTripper) *http.Client {
	return &http.Client{Timeout: timeout, Transport: httpsTransport{base}}
}
