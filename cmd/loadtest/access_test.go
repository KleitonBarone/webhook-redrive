package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestAPICredentialNeverReachesReceiver(t *testing.T) {
	const token = "synthetic-api-token"
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Error("API missing credential")
		}
		w.WriteHeader(204)
	}))
	defer api.Close()
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("API credential sent to receiver")
		}
		w.WriteHeader(204)
	}))
	defer receiver.Close()
	u, _ := url.Parse(api.URL)
	client := &http.Client{Transport: &apiTransport{base: http.DefaultTransport.(*http.Transport).Clone(), api: u, token: token}}
	defer client.CloseIdleConnections()
	for _, target := range []string{api.URL, receiver.URL} {
		response, err := client.Get(target)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
	}
}
