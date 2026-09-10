package httpapi

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/KleitonBarone/webhook-redrive/internal/secret"
)

func TestReplayAndEndpointValidation(t *testing.T) {
	box, err := secret.NewBox(make([]byte, secret.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	api := New(&fakeStore{}, box, apiClock{now: time.Unix(100, 0)}, discardLogger())
	uuid := "7d178c7d-cbdd-4e47-a158-69e2f5c89770"
	for _, test := range []struct {
		path, body string
		want       int
	}{
		{"/v1/events/" + uuid + "/replays", `{"attempt_id":"` + uuid + `","request_id":"` + uuid + `","actor":"operator","reason":"fixed"}`, 202},
		{"/v1/events/" + uuid + "/replays", `{"attempt_id":"` + uuid + `","request_id":"` + uuid + `","actor":"","reason":"fixed"}`, 400},
		{"/v1/events/" + uuid + "/replays", `{"attempt_id":"` + uuid + `","request_id":"invalid","actor":"operator","reason":"fixed"}`, 400},
		{"/v1/events/invalid/replays", `{}`, 400},
		{"/v1/endpoints", `{"url":"http://synthetic.test","secret":"synthetic-secret","max_attempts":0}`, 400},
		{"/v1/endpoints", `{"url":"http://synthetic.test","secret":"synthetic-secret","concurrency_limit":101}`, 400},
		{"/v1/endpoints", `{"url":"http://synthetic.test","secret":"synthetic-secret","rate_limit":0}`, 400},
	} {
		res := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, test.path, bytes.NewBufferString(test.body))
		api.ServeHTTP(res, req)
		if res.Code != test.want {
			t.Errorf("%s body %s: %d want %d", test.path, test.body, res.Code, test.want)
		}
	}
}
