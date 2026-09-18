package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KleitonBarone/webhook-redrive/internal/delivery"
	"github.com/KleitonBarone/webhook-redrive/internal/httpapi"
	"github.com/KleitonBarone/webhook-redrive/internal/id"
	"github.com/KleitonBarone/webhook-redrive/internal/logsafe"
	"github.com/KleitonBarone/webhook-redrive/internal/secret"
	"github.com/KleitonBarone/webhook-redrive/internal/store"
	"github.com/KleitonBarone/webhook-redrive/internal/testdb"
	"github.com/KleitonBarone/webhook-redrive/signature"
)

// TestEndpointLifecycleDemo uses real HTTP and PostgreSQL with a virtual clock.
// It demonstrates planned rotation and outage recovery without waiting hours.
func TestEndpointLifecycleDemo(t *testing.T) {
	ctx := context.Background()
	databaseURL := testdb.URL(t)
	s, err := store.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := &demoClock{}
	c.nanos.Store(time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC).UnixNano())
	if err := s.Migrate(ctx, c.Now()); err != nil {
		t.Fatal(err)
	}
	box, _ := secret.NewBox(make([]byte, secret.KeySize))
	const oldKey = "synthetic-lifecycle-old-secret"
	const newKey = "synthetic-lifecycle-new-secret"
	const reason = "synthetic-maintenance-private-reason"
	payload := []byte("{\n \"private\":\"synthetic-lifecycle-private-payload\"\n}\n")
	var retired, healthy atomic.Bool
	var oldSends, newSends atomic.Int64
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil || !bytes.Equal(body, payload) {
			t.Error("body changed")
			w.WriteHeader(400)
			return
		}
		keys := [][]byte{[]byte(newKey)}
		if !retired.Load() {
			keys = append(keys, []byte(oldKey))
		}
		if signature.VerifyRequestKeys(keys, r, body, c.Now(), 5*time.Minute) != nil {
			t.Error("unverifiable rotation delivery")
			w.WriteHeader(401)
			return
		}
		if signature.VerifyRequest([]byte(oldKey), r, body, c.Now(), 5*time.Minute) == nil {
			oldSends.Add(1)
		} else {
			newSends.Add(1)
		}
		switch r.URL.Path {
		case "/held":
			close(started)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		case "/outage":
			if !healthy.Load() {
				w.WriteHeader(503)
				return
			}
		}
		w.WriteHeader(204)
	}))
	defer receiver.Close()
	// Ensure a failed test releases the receiver before httptest.Close waits.
	defer unblock()
	var logs lockedBuffer
	logger := slog.New(logsafe.New(slog.NewJSONHandler(&logs, nil)))
	security := testSecurityFor(t, s, c.Now(), receiver.URL, 7*24*time.Hour)
	api := httptest.NewServer(httpapi.New(s, box, c, logger, nil, security))
	defer api.Close()
	newWorker := func(db *store.Store) *delivery.Worker {
		t.Helper()
		w, err := delivery.NewWorker(db, box, c, logger, delivery.Config{Destinations: security.Destinations, WorkerID: "lifecycle", Lease: 20 * time.Second, RequestTimeout: 10 * time.Second, BatchSize: 1, PollPeriod: time.Millisecond, Jitter: func() float64 { return 1 }})
		if err != nil {
			t.Fatal(err)
		}
		return w
	}
	worker := newWorker(s)
	ingest := func(endpointID string) store.Event {
		t.Helper()
		r, _ := http.NewRequest("POST", api.URL+"/v1/endpoints/"+endpointID+"/events", bytes.NewReader(payload))
		r.Header.Set("Authorization", "Bearer "+testToken)
		r.Header.Set("X-Event-Type", "synthetic.lifecycle")
		response, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != 202 {
			t.Fatalf("ingest %d", response.StatusCode)
		}
		var event store.Event
		if err := json.NewDecoder(response.Body).Decode(&event); err != nil {
			t.Fatal(err)
		}
		return event
	}
	run := func(w *delivery.Worker, want int) {
		t.Helper()
		if n, err := w.RunOnce(ctx); err != nil || n != want {
			t.Fatalf("dispatch %d want %d: %v", n, want, err)
		}
	}
	var endpoint store.Endpoint
	call(t, api.URL+"/v1/endpoints", "POST", map[string]any{"url": receiver.URL + "/held", "secret": oldKey, "retry_profile": "outage"}, 201, &endpoint)
	base := api.URL + "/v1/endpoints/" + endpoint.ID
	var listed struct {
		Endpoints []store.Endpoint `json:"endpoints"`
	}
	call(t, api.URL+"/v1/endpoints", "GET", nil, 200, &listed)
	if len(listed.Endpoints) != 1 || listed.Endpoints[0].ID != endpoint.ID {
		t.Fatal("endpoint listing lost registration")
	}
	call(t, api.URL+"/v1/endpoints?after="+endpoint.ID, "GET", nil, 200, &listed)
	if len(listed.Endpoints) != 0 {
		t.Fatal("endpoint cursor repeated entry")
	}
	call(t, api.URL+"/v1/endpoints/00000000-0000-4000-8000-000000000099", "GET", nil, 404, nil)
	first := ingest(endpoint.ID)
	done := make(chan error, 1)
	go func() { _, err := worker.RunOnce(ctx); done <- err }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("held delivery did not start")
	}
	call(t, base+"/pause", "POST", map[string]any{"expected_version": endpoint.Version, "reason": reason}, 200, &endpoint)
	queued := ingest(endpoint.ID)
	run(worker, 0)
	call(t, base+"/secret-rotations", "POST", map[string]any{"expected_version": endpoint.Version, "reason": reason, "secret": newKey, "overlap_seconds": 300}, 200, &endpoint)
	call(t, base, "PUT", map[string]any{"expected_version": endpoint.Version, "reason": reason, "url": receiver.URL + "/repaired", "retry_profile": "outage"}, 200, &endpoint)
	// A rejected destination edit cannot change configuration or its version.
	call(t, base, "PUT", map[string]any{"expected_version": endpoint.Version, "reason": reason, "url": "http://169.254.169.254/"}, 400, nil)
	unblock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var firstState store.Event
	call(t, api.URL+"/v1/events/"+first.ID, "GET", nil, 200, &firstState)
	if firstState.State != "succeeded" {
		t.Fatal("pause discarded in-flight outcome")
	}
	t.Log("1. Pause retained newly ingested work; an already-running old-key delivery completed.")
	call(t, base+"/secret-retirement", "POST", map[string]any{"expected_version": endpoint.Version, "reason": reason}, 409, nil)
	call(t, base+"/resume", "POST", map[string]any{"expected_version": endpoint.Version, "reason": reason}, 200, &endpoint)
	run(worker, 1)
	history, err := s.ListAttempts(ctx, queued.ID)
	if err != nil || history[0].State != "succeeded" || *history[0].SigningVersion != 2 || *history[0].EndpointVersion != endpoint.Version {
		t.Fatal("queued work missed updated configuration")
	}
	c.Advance(5 * time.Minute)
	call(t, base+"/secret-retirement", "POST", map[string]any{"expected_version": endpoint.Version, "reason": reason}, 200, &endpoint)
	c.Advance(5 * time.Minute) // Drain the receiver's signature acceptance window.
	retired.Store(true)
	ingest(endpoint.ID)
	run(worker, 1)
	if oldSends.Load() != 1 || newSends.Load() != 2 {
		t.Fatal("incorrect signing transition")
	}
	var audit struct {
		Entries []store.EndpointAudit `json:"entries"`
	}
	call(t, base+"/audit", "GET", nil, 200, &audit)
	if len(audit.Entries) != 6 {
		t.Fatalf("audit count %d", len(audit.Entries))
	}
	var laterAudit struct {
		Entries []store.EndpointAudit `json:"entries"`
	}
	call(t, base+"/audit?after=4", "GET", nil, 200, &laterAudit)
	if len(laterAudit.Entries) != 2 || laterAudit.Entries[0].Version != 5 {
		t.Fatal("audit cursor repeated or lost history")
	}
	for _, entry := range audit.Entries {
		if entry.PrincipalID != "00000000-0000-4000-8000-000000000001" {
			t.Fatal("unauthenticated audit")
		}
	}
	encoded, _ := json.Marshal(audit)
	if bytes.Contains(encoded, []byte(oldKey)) || bytes.Contains(encoded, []byte(newKey)) {
		t.Fatal("audit exposed signing secret")
	}
	t.Log("2. Queued work used the repaired URL and new key; retirement waited for overlap, and receiver verification continued.")

	// A long outage uses the persisted policy, including across a fresh pool.
	call(t, base, "PUT", map[string]any{"expected_version": endpoint.Version, "reason": reason, "url": receiver.URL + "/outage", "retry_profile": "outage"}, 200, &endpoint)
	outage := ingest(endpoint.ID)
	start := c.Now()
	for i := 0; i < 7; i++ {
		run(worker, 1)
		history, err = s.ListAttempts(ctx, outage.ID)
		if err != nil {
			t.Fatal(err)
		}
		next := history[len(history)-1]
		if next.State != "pending" || !next.ExpiresAt.Equal(start.Add(24*time.Hour)) {
			t.Fatal("outage deadline changed")
		}
		c.Advance(next.AvailableAt.Sub(c.Now()))
	}
	if c.Now().Sub(start) < 6*time.Hour {
		t.Fatal("did not exercise prolonged outage")
	}
	restarted, err := store.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	worker = newWorker(restarted)
	healthy.Store(true)
	run(worker, 1)
	state, err := s.GetEvent(ctx, outage.ID)
	if err != nil || state.State != "succeeded" {
		t.Fatal("outage recovery failed")
	}
	t.Log("3. Persisted retries survived a fresh worker/database pool and recovered after more than six simulated hours.")
	healthy.Store(false)
	expiring := ingest(endpoint.ID)
	start = c.Now()
	for i := 0; i < 21; i++ {
		_, err := worker.RunOnce(ctx)
		if err != nil {
			t.Fatal(err)
		}
		history, err = s.ListAttempts(ctx, expiring.ID)
		if err != nil {
			t.Fatal(err)
		}
		last := history[len(history)-1]
		if last.State == "dead_letter" {
			break
		}
		if last.State != "pending" {
			t.Fatalf("unexpected outage state %s", last.State)
		}
		c.Advance(last.AvailableAt.Sub(c.Now()))
	}
	last := history[len(history)-1]
	if last.State != "dead_letter" || last.ErrorCode == nil || *last.ErrorCode != "event_expired" || c.Now().Sub(start) != 24*time.Hour {
		t.Fatal("outage extended past deadline")
	}
	healthy.Store(true)
	run(worker, 0)
	requestID, _ := id.New()
	var replay store.ReplayResult
	call(t, api.URL+"/v1/events/"+expiring.ID+"/replays", "POST", store.ReplayRequest{AttemptID: last.ID, RequestID: requestID, Reason: reason}, 202, &replay)
	run(worker, 1)
	state, err = s.GetEvent(ctx, expiring.ID)
	if err != nil || state.State != "succeeded" {
		t.Fatal("explicit recovery failed")
	}
	t.Log("4. Continuous failure expired at 24 simulated hours; only authenticated explicit replay opened a new cycle.")
	for _, private := range []string{oldKey, newKey, reason, testToken, "synthetic-lifecycle-private-payload", receiver.URL} {
		if strings.Contains(logs.String(), private) {
			t.Fatal("lifecycle logs leaked private value")
		}
	}
}
