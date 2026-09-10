package main

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/KleitonBarone/webhook-redrive/internal/signature"
)

type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time { return c.now }

func TestFlakyReceiverCountsPerEventAndVerifiesSignatures(t *testing.T) {
	now := time.Unix(100, 0)
	r := &receiver{secret: []byte("synthetic-secret"), clock: fixedClock{now}, tolerance: time.Minute,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	handler := r.deliver(500, 0)
	for _, test := range []struct {
		event string
		valid bool
		want  int
	}{
		{"event-a", false, 401}, {"event-a", true, 500}, {"event-b", true, 500},
		{"event-a", true, 500}, {"event-a", true, 204}, {"event-b", true, 500},
	} {
		body := []byte("{}")
		req := httptest.NewRequest(http.MethodPost, "/flaky?failures=2", bytes.NewReader(body))
		req.Header.Set("X-Webhook-ID", test.event)
		req.Header.Set("X-Webhook-Timestamp", strconv.FormatInt(now.Unix(), 10))
		if test.valid {
			req.Header.Set("X-Webhook-Signature", signature.Sign(r.secret, now, body))
		}
		res := httptest.NewRecorder()
		handler(res, req)
		if res.Code != test.want {
			t.Fatalf("%+v returned %d", test, res.Code)
		}
	}
}
