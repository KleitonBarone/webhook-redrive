package httpapi

import (
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReadinessSeparatesDatabaseFailureFromLiveness(t *testing.T) {
	s := &fakeStore{readinessError: errors.New("postgres://synthetic-password/private")}
	api := New(s, nil, apiClock{}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, Security{})
	for _, tc := range []struct {
		path string
		want int
	}{{"/healthz", 200}, {"/readyz", 503}} {
		w := httptest.NewRecorder()
		api.ServeHTTP(w, httptest.NewRequest("GET", tc.path, nil))
		if w.Code != tc.want || strings.Contains(w.Body.String(), "synthetic") {
			t.Fatalf("%s: %d %s", tc.path, w.Code, w.Body)
		}
	}
	s.readinessError = nil
	w := httptest.NewRecorder()
	api.ServeHTTP(w, httptest.NewRequest("GET", "/readyz", nil))
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
}
