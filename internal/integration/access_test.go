package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/KleitonBarone/webhook-redrive/internal/auth"
	"github.com/KleitonBarone/webhook-redrive/internal/destination"
	"github.com/KleitonBarone/webhook-redrive/internal/httpapi"
	"github.com/KleitonBarone/webhook-redrive/internal/id"
	"github.com/KleitonBarone/webhook-redrive/internal/store"
	"github.com/KleitonBarone/webhook-redrive/internal/testdb"
)

const testToken = "wr_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func testPolicy(t *testing.T, origin string) *destination.Policy {
	t.Helper()
	p, err := destination.New([]destination.Rule{{Origin: origin, PrivateNetworks: []string{"127.0.0.1/32"}}})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestHTTPReplaySurvivesCredentialRotationAndRejectsRevocation(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, testdb.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := &clock{now: time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)}
	if err := s.Migrate(ctx, c.now); err != nil {
		t.Fatal(err)
	}
	security := testSecurity(t, s, c.now, "https://receiver.test")
	api := httpapi.New(s, nil, c, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, security)
	endpointID, _ := id.New()
	eventID, _ := id.New()
	attemptID, _ := id.New()
	requestID, _ := id.New()
	if err := s.CreateEndpoint(ctx, store.Endpoint{ID: endpointID, URL: "https://receiver.test", CreatedAt: c.now}, []byte("unused-encrypted")); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateEvent(ctx, store.Event{ID: eventID, EndpointID: endpointID, EventType: "test", CreatedAt: c.now}, []byte("{}"), attemptID); err != nil {
		t.Fatal(err)
	}
	claims, err := s.ClaimAvailable(ctx, "test", c.now, time.Minute, 1)
	if err != nil || len(claims) != 1 {
		t.Fatal("claim failed")
	}
	if ok, err := s.Complete(ctx, claims[0], "test", c.now, store.Outcome{Status: 400, Code: "http_status"}); err != nil || !ok {
		t.Fatal("completion failed")
	}
	body, _ := json.Marshal(store.ReplayRequest{AttemptID: attemptID, RequestID: requestID, Reason: "synthetic repair"})
	replay := func(token string, want int) string {
		t.Helper()
		req := httptest.NewRequest("POST", "/v1/events/"+eventID+"/replays", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		res := httptest.NewRecorder()
		api.ServeHTTP(res, req)
		if res.Code != want {
			t.Fatalf("replay status %d want %d", res.Code, want)
		}
		if want != 202 {
			return ""
		}
		var result store.ReplayResult
		if err := json.Unmarshal(res.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result.AttemptID
	}
	original := replay(testToken, 202)
	rotatedToken, err := auth.Generate()
	if err != nil {
		t.Fatal(err)
	}
	hash, _ := auth.Hash(rotatedToken)
	credentialID, _ := id.New()
	if err := s.IssueCredential(ctx, "00000000-0000-4000-8000-000000000001", credentialID, hash, c.now, c.now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeCredential(ctx, "00000000-0000-4000-8000-000000000002", c.now); err != nil {
		t.Fatal(err)
	}
	replay(testToken, 401)
	if replay(rotatedToken, 202) != original {
		t.Fatal("rotation broke replay deduplication")
	}
	c.now = c.now.Add(time.Hour)
	replay(rotatedToken, 401)
	history, err := s.ListAttempts(ctx, eventID)
	if err != nil || len(history) != 2 || history[1].ReplayPrincipalID == nil || *history[1].ReplayPrincipalID != "00000000-0000-4000-8000-000000000001" {
		t.Fatal("invalid replay history")
	}
}

func testSecurity(t *testing.T, s *store.Store, now time.Time, origin string) httpapi.Security {
	return testSecurityFor(t, s, now, origin, time.Hour)
}

func testSecurityFor(t *testing.T, s *store.Store, now time.Time, origin string, lifetime time.Duration) httpapi.Security {
	t.Helper()
	p := auth.Principal{ID: "00000000-0000-4000-8000-000000000001", Name: "synthetic-actor-private", Kind: "operator", Permissions: []string{auth.Ingest, auth.Inspect, auth.Endpoints, auth.Replay, auth.Metrics}}
	if err := s.CreatePrincipal(context.Background(), p, now); err != nil {
		t.Fatal(err)
	}
	hash, _ := auth.Hash(testToken)
	if err := s.IssueCredential(context.Background(), p.ID, "00000000-0000-4000-8000-000000000002", hash, now, now.Add(lifetime)); err != nil {
		t.Fatal(err)
	}
	return httpapi.Security{Authenticator: s, Destinations: testPolicy(t, origin)}
}
