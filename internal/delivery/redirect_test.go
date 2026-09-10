package delivery

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KleitonBarone/webhook-redrive/internal/secret"
	"github.com/KleitonBarone/webhook-redrive/internal/store"
)

func TestRedirectDoesNotForwardSignedPayload(t *testing.T) {
	var forwarded atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer endpoint.Close()
	box, err := secret.NewBox(make([]byte, secret.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := box.Encrypt([]byte("synthetic-endpoint-secret"))
	if err != nil {
		t.Fatal(err)
	}
	dataStore := &fakeAttemptStore{delivery: store.ClaimedDelivery{
		AttemptID: "attempt", EventID: "event", EndpointURL: endpoint.URL,
		Payload: []byte("{}"), SecretCiphertext: ciphertext,
	}}
	worker := newTestWorker(t, dataStore, box, fixedClock{now: time.Unix(100, 0)}, time.Second)
	if _, err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if forwarded.Load() != 0 {
		t.Fatal("redirect forwarded the signed payload to a different endpoint")
	}
	if dataStore.failures != 1 || dataStore.failureCode != "http_status" {
		t.Fatal("redirect should be a terminal HTTP failure")
	}
}
