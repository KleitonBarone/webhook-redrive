package httpapi

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/KleitonBarone/webhook-redrive/internal/auth"
	"github.com/KleitonBarone/webhook-redrive/internal/logsafe"
)

func TestEveryProtectedRouteRequiresItsPermission(t *testing.T) {
	uuid := "7d178c7d-cbdd-4e47-a158-69e2f5c89770"
	routes := []struct{ method, path, permission string }{
		{"POST", "/v1/endpoints", auth.Endpoints},
		{"GET", "/v1/endpoints", auth.Inspect},
		{"GET", "/v1/endpoints/" + uuid, auth.Inspect},
		{"GET", "/v1/endpoints/" + uuid + "/audit", auth.Inspect},
		{"PUT", "/v1/endpoints/" + uuid, auth.Endpoints},
		{"POST", "/v1/endpoints/" + uuid + "/pause", auth.Endpoints},
		{"POST", "/v1/endpoints/" + uuid + "/resume", auth.Endpoints},
		{"POST", "/v1/endpoints/" + uuid + "/secret-rotations", auth.Endpoints},
		{"POST", "/v1/endpoints/" + uuid + "/secret-retirement", auth.Endpoints},
		{"POST", "/v1/endpoints/" + uuid + "/events", auth.Ingest},
		{"GET", "/v1/events/" + uuid, auth.Inspect},
		{"GET", "/v1/events/" + uuid + "/attempts", auth.Inspect},
		{"POST", "/v1/events/" + uuid + "/replays", auth.Replay},
		{"GET", "/v1/dead-letters", auth.Inspect},
		{"GET", "/v1/events", auth.Inspect},
		{"POST", "/v1/replay-batches", auth.Replay},
		{"GET", "/v1/replay-batches/" + uuid, auth.Inspect},
		{"POST", "/v1/replay-batches/" + uuid + "/run", auth.Replay},
		{"GET", "/metrics", auth.Metrics},
	}
	for _, route := range routes {
		t.Run(route.method+route.path, func(t *testing.T) {
			for _, permission := range []string{"", auth.Ingest, auth.Inspect, auth.Endpoints, auth.Replay, auth.Metrics} {
				s := &fakeStore{}
				security := testSecurity(t)
				security.Authenticator = fakeAuth{permissions: []string{permission}}
				api := New(s, nil, apiClock{now: time.Unix(100, 0)}, discardLogger(), nil, security)
				r := authenticatedRequest(route.method, route.path)
				response := httptest.NewRecorder()
				api.ServeHTTP(response, r)
				if permission != route.permission {
					if response.Code != 403 {
						t.Fatalf("permission %s returned %d", permission, response.Code)
					}
					if s.endpoint.ID != "" || s.event.ID != "" || s.replayInput.RequestID != "" {
						t.Fatal("denied request mutated state")
					}
				} else if response.Code == 401 || response.Code == 403 || response.Code == 503 {
					t.Fatalf("correct permission denied: %d", response.Code)
				}
			}
		})
	}
}

func TestAuthenticationErrorsFailClosedAndDoNotLeak(t *testing.T) {
	for _, tc := range []struct {
		header    string
		duplicate bool
		err       error
		status    int
	}{
		{"", false, nil, 401}, {"Basic " + testToken, false, nil, 401},
		{"Bearer " + testToken, true, nil, 401}, {"Bearer invalid-token", false, nil, 401},
		{"Bearer " + testToken, false, auth.ErrUnauthorized, 401},
		{"Bearer " + testToken, false, errors.New("synthetic-private-database-credential"), 503},
	} {
		var logs bytes.Buffer
		security := testSecurity(t)
		security.Authenticator = fakeAuth{err: tc.err}
		s := &fakeStore{}
		api := New(s, nil, apiClock{}, slog.New(logsafe.New(slog.NewJSONHandler(&logs, nil))), nil, security)
		r := httptest.NewRequest("POST", "/v1/endpoints", strings.NewReader(`{"secret":"private-payload"}`))
		r.Header.Set("Authorization", tc.header)
		if tc.duplicate {
			r.Header.Add("Authorization", tc.header)
		}
		response := httptest.NewRecorder()
		api.ServeHTTP(response, r)
		if response.Code != tc.status || s.endpoint.ID != "" {
			t.Fatalf("status %d want %d", response.Code, tc.status)
		}
		if tc.status == 401 && response.Header().Get("WWW-Authenticate") == "" {
			t.Fatal("missing challenge")
		}
		for _, sensitive := range []string{testToken, "private-payload", "synthetic-private-database-credential"} {
			if strings.Contains(logs.String()+response.Body.String(), sensitive) {
				t.Fatal("sensitive authentication value leaked")
			}
		}
	}
	api := New(&fakeStore{}, nil, apiClock{}, discardLogger(), nil, Security{})
	response := httptest.NewRecorder()
	api.ServeHTTP(response, authenticatedRequest("GET", "/metrics"))
	if response.Code != 503 {
		t.Fatal("missing authenticator did not fail closed")
	}
	response = httptest.NewRecorder()
	api.ServeHTTP(response, httptest.NewRequest("GET", "/healthz", nil))
	if response.Code != 200 {
		t.Fatal("liveness requires authentication")
	}
}

func TestRegistrationRejectsUnapprovedDestinationBeforePersistence(t *testing.T) {
	s := &fakeStore{}
	api := New(s, nil, apiClock{}, discardLogger(), nil, testSecurity(t))
	for _, target := range []string{"http://169.254.169.254/latest/meta-data", "https://not-approved.test/hook"} {
		r := httptest.NewRequest("POST", "/v1/endpoints", strings.NewReader(`{"url":"`+target+`","secret":"synthetic-secret-long"}`))
		r.Header.Set("Authorization", "Bearer "+testToken)
		response := httptest.NewRecorder()
		api.ServeHTTP(response, r)
		if response.Code != 400 || s.endpoint.ID != "" || strings.Contains(response.Body.String(), target) {
			t.Fatal("registration policy bypass")
		}
	}
}
