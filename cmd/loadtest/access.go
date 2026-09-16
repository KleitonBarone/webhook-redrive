package main

import (
	"net/http"
	"net/url"
)

// Attach API credentials only to the configured API origin, never the receiver.
type apiTransport struct {
	base  *http.Transport
	api   *url.URL
	token string
}

func (t *apiTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	request := r.Clone(r.Context())
	if r.URL.Scheme == t.api.Scheme && r.URL.Host == t.api.Host {
		request.Header.Set("Authorization", "Bearer "+t.token)
	}
	return t.base.RoundTrip(request)
}

func (t *apiTransport) CloseIdleConnections() { t.base.CloseIdleConnections() }
