package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func changeEndpoint(t *testing.T, s *Store, e Endpoint, action string, now time.Time) Endpoint {
	t.Helper()
	result, err := s.ChangeEndpoint(context.Background(), e.ID, EndpointChange{ExpectedVersion: e.Version, Action: action, Settings: e, Reason: "synthetic maintenance"}, now)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestPauseUpdatesAndReplayPolicy(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	endpointID := insertTestEndpoint(t, s)
	e, err := s.GetEndpoint(ctx, endpointID)
	if err != nil {
		t.Fatal(err)
	}
	first := seed(t, s, endpointID)
	inFlight := claimOne(t, s, testNow)
	e = changeEndpoint(t, s, e, "paused", testNow)
	queued := seed(t, s, endpointID)
	if got, err := s.ClaimAvailable(ctx, "other", testNow, time.Minute, 10); err != nil || len(got) != 0 {
		t.Fatalf("paused claim %v %v", got, err)
	}
	// Pause does not cancel a committed claim or discard its completion.
	finish(t, s, inFlight, testNow, Outcome{Code: "http_status", Status: 400})
	m, err := s.Metrics(ctx, testNow)
	if err != nil || m.Queue["paused"] != 1 || m.Queue["ready"] != 0 {
		t.Fatalf("paused metrics %+v %v", m, err)
	}
	e.URL = "https://synthetic.test/new"
	e.MaxAttempts, e.RetryBaseSeconds, e.RetryCapSeconds, e.EventTTLSeconds = 20, 300, 7200, 86400
	e = changeEndpoint(t, s, e, "updated", testNow)
	if !e.Paused {
		t.Fatal("settings update resumed endpoint")
	}
	e = changeEndpoint(t, s, e, "resumed", testNow)
	c := claimOne(t, s, testNow)
	if c.EventID != queued || c.EndpointURL != e.URL || c.EndpointVersion != e.Version || c.RetryBaseSeconds != 1 {
		t.Fatalf("claim snapshot %+v", c)
	}
	finish(t, s, c, testNow, Outcome{Status: 204})
	if _, err := s.Replay(ctx, first, ReplayRequest{AttemptID: inFlight.AttemptID, RequestID: mustID(t), Actor: "test", Reason: "new policy"}, testNow.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	history, err := s.ListAttempts(ctx, first)
	if err != nil || len(history) != 2 {
		t.Fatal(err)
	}
	if history[0].MaxAttempts != 5 || history[1].MaxAttempts != 20 || history[1].RetryBaseSeconds != 300 || !history[1].ExpiresAt.Equal(testNow.Add(25*time.Hour)) {
		t.Fatalf("policy history %+v", history)
	}
	audit, err := s.ListEndpointAudit(ctx, endpointID, 0)
	if err != nil || len(audit) != 4 || audit[1].Action != "paused" || !audit[2].Configuration.Paused {
		t.Fatalf("audit %+v %v", audit, err)
	}
}

func TestEndpointUpdateCASAndAuditAtomicity(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	e, err := s.GetEndpoint(ctx, insertTestEndpoint(t, s))
	if err != nil {
		t.Fatal(err)
	}
	principal := accessPrincipal(t, s)
	change := EndpointChange{ExpectedVersion: e.Version, Action: "paused", PrincipalID: principal.ID, Reason: "synthetic audit reason"}
	if _, err := s.pool.Exec(ctx, `ALTER TABLE endpoint_audit ADD CONSTRAINT reject_change CHECK(version=1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ChangeEndpoint(ctx, e.ID, change, testNow); err == nil {
		t.Fatal("expected audit insert failure")
	}
	unchanged, err := s.GetEndpoint(ctx, e.ID)
	if err != nil || unchanged.Version != e.Version || unchanged.Paused {
		t.Fatal("mutation committed without audit")
	}
	if _, err := s.pool.Exec(ctx, `ALTER TABLE endpoint_audit DROP CONSTRAINT reject_change`); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := s.ChangeEndpoint(ctx, e.ID, change, testNow); results <- err }()
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		} else if !errors.Is(err, ErrEndpointConflict) {
			t.Fatal(err)
		}
	}
	if success != 1 {
		t.Fatalf("successful edits %d", success)
	}
	audit, err := s.ListEndpointAudit(ctx, e.ID, 1)
	if err != nil || len(audit) != 1 || audit[0].PrincipalID != principal.ID || audit[0].Reason != change.Reason {
		t.Fatal("missing attributed audit")
	}
}

func TestRotationRetirementFencesOldClaims(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	e, err := s.GetEndpoint(ctx, insertTestEndpoint(t, s))
	if err != nil {
		t.Fatal(err)
	}
	event := seed(t, s, e.ID)
	claims, err := s.ClaimAvailable(ctx, "worker", testNow, 10*time.Minute, 1)
	if err != nil || len(claims) != 1 {
		t.Fatal("missing old claim")
	}
	old := claims[0]
	rotation := EndpointChange{ExpectedVersion: e.Version, Action: "rotated", Ciphertext: []byte("synthetic-new-ciphertext"), Overlap: 5 * time.Minute, Reason: "test"}
	e, err = s.ChangeEndpoint(ctx, e.ID, rotation, testNow)
	if err != nil || e.SigningVersion != 2 {
		t.Fatalf("rotation %+v %v", e, err)
	}
	rotation.ExpectedVersion = e.Version
	if _, err := s.ChangeEndpoint(ctx, e.ID, rotation, testNow); !errors.Is(err, ErrEndpointConflict) {
		t.Fatal("overlapping rotations accepted")
	}
	retire := EndpointChange{ExpectedVersion: e.Version, Action: "retired", Reason: "receiver prepared"}
	for _, now := range []time.Time{testNow.Add(time.Minute), testNow.Add(5 * time.Minute)} {
		if _, err := s.ChangeEndpoint(ctx, e.ID, retire, now); !errors.Is(err, ErrEndpointConflict) {
			t.Fatal("retired before overlap or old lease drained")
		}
	}
	e, err = s.ChangeEndpoint(ctx, e.ID, retire, testNow.Add(10*time.Minute))
	if err != nil || e.RetiringSigningVersion != nil {
		t.Fatal("retirement failed", err)
	}
	recovered := claimOne(t, s, testNow.Add(10*time.Minute))
	if recovered.SigningVersion != 2 || string(recovered.SecretCiphertext) != "synthetic-new-ciphertext" || recovered.EventID != event {
		t.Fatal("recovery used retired signing key")
	}
	if ok, err := s.Complete(ctx, old, "worker", testNow.Add(10*time.Minute), Outcome{Status: 204}); err != nil || ok {
		t.Fatal("old completion accepted")
	}
}

func TestExpirationSurvivesPauseRestartAndRetryAfter(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	e, err := s.GetEndpoint(ctx, insertTestEndpoint(t, s))
	if err != nil {
		t.Fatal(err)
	}
	e.EventTTLSeconds = 3600
	e = changeEndpoint(t, s, e, "updated", testNow)
	event := seed(t, s, e.ID)
	claim := claimOne(t, s, testNow)
	later := testNow.Add(24 * time.Hour)
	finish(t, s, claim, testNow, Outcome{Code: "http_status", Status: 429, RetryAt: &later})
	e = changeEndpoint(t, s, e, "paused", testNow)
	history, err := s.ListAttempts(ctx, event)
	if err != nil || len(history) != 2 || !history[1].AvailableAt.Equal(testNow.Add(time.Hour)) {
		t.Fatal("retry escaped deadline")
	}
	// Changing TTL cannot extend the existing cycle.
	e.EventTTLSeconds = 604800
	changeEndpoint(t, s, e, "updated", testNow)
	fresh, err := Open(ctx, s.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if claims, err := fresh.ClaimAvailable(ctx, "restart", testNow.Add(time.Hour), time.Minute, 10); err != nil || len(claims) != 0 {
		t.Fatal("expired event dispatched")
	}
	history, err = fresh.ListAttempts(ctx, event)
	if err != nil || history[1].State != "dead_letter" || *history[1].ErrorCode != "event_expired" || history[1].ClaimCount != 0 {
		t.Fatalf("expiry %+v %v", history, err)
	}
}

func TestExpirationDoesNotStealLiveLease(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	e, err := s.GetEndpoint(ctx, insertTestEndpoint(t, s))
	if err != nil {
		t.Fatal(err)
	}
	e.EventTTLSeconds = 1
	changeEndpoint(t, s, e, "updated", testNow)
	event := seed(t, s, e.ID)
	claim := claimOne(t, s, testNow)
	if got, err := s.ClaimAvailable(ctx, "other", testNow.Add(time.Second), time.Minute, 1); err != nil || len(got) != 0 {
		t.Fatal("live claim reclaimed")
	}
	finish(t, s, claim, testNow.Add(time.Second), Outcome{Status: 204})
	history, err := s.ListAttempts(ctx, event)
	if err != nil || history[0].State != "succeeded" {
		t.Fatal("live success lost")
	}
}

func TestCrashAtDeadlineExpiresWithoutAnotherClaim(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	e, err := s.GetEndpoint(ctx, insertTestEndpoint(t, s))
	if err != nil {
		t.Fatal(err)
	}
	e.EventTTLSeconds = 10
	changeEndpoint(t, s, e, "updated", testNow)
	event := seed(t, s, e.ID)
	old := claimOne(t, s, testNow)
	// No completion: the old worker might have sent the request.
	if claims, err := s.ClaimAvailable(ctx, "replacement", testNow.Add(time.Minute), time.Minute, 1); err != nil || len(claims) != 0 {
		t.Fatal("expired crash recovery dispatched")
	}
	if ok, err := s.Complete(ctx, old, "worker", testNow.Add(time.Minute), Outcome{Status: 204}); err != nil || ok {
		t.Fatal("expired result committed")
	}
	h, err := s.ListAttempts(ctx, event)
	if err != nil || len(h) != 1 || h[0].State != "dead_letter" || h[0].ClaimCount != 1 || *h[0].ErrorCode != "event_expired" {
		t.Fatalf("expired recovery %+v %v", h, err)
	}
}

func TestPauseSerializesWithConcurrentClaims(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	e, err := s.GetEndpoint(ctx, insertTestEndpoint(t, s))
	if err != nil {
		t.Fatal(err)
	}
	e.ConcurrencyLimit = 2
	e = changeEndpoint(t, s, e, "updated", testNow)
	for i := 0; i < 4; i++ {
		seed(t, s, e.ID)
	}
	// Hold the same endpoint lock as an in-progress configuration transaction.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT id FROM webhook_endpoints WHERE id=$1 FOR NO KEY UPDATE`, e.ID); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			claims, err := s.ClaimAvailable(ctx, "racer", testNow, time.Minute, 10)
			if err != nil || len(claims) != 0 {
				t.Errorf("claim crossed configuration lock: %d %v", len(claims), err)
			}
		}()
	}
	wg.Wait()
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	e = changeEndpoint(t, s, e, "paused", testNow)
	if got, err := s.ClaimAvailable(ctx, "after-pause", testNow, time.Minute, 10); err != nil || len(got) != 0 {
		t.Fatal("claimed after pause committed")
	}
	e = changeEndpoint(t, s, e, "resumed", testNow)
	claims, err := s.ClaimAvailable(ctx, "worker", testNow, time.Minute, 10)
	if err != nil || len(claims) != 2 {
		t.Fatalf("resume claims=%d want=2 err=%v", len(claims), err)
	}
	e.ConcurrencyLimit = 1
	changeEndpoint(t, s, e, "updated", testNow)
	if got, err := s.ClaimAvailable(ctx, "after-reduction", testNow, time.Minute, 10); err != nil || len(got) != 0 {
		t.Fatal("limit reduction allowed excess claims")
	}
	for _, claim := range claims {
		finish(t, s, claim, testNow, Outcome{Status: 204})
	}
	if got, err := s.ClaimAvailable(ctx, "after-drain", testNow, time.Minute, 10); err != nil || len(got) != 1 {
		t.Fatal("reduced limit not applied")
	}
}

func TestExpirationSweepIsBoundedAndResumable(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	endpoint := insertTestEndpoint(t, s)
	// Bulk seed one more row than the sweep bound, in an isolated test schema.
	_, err := s.pool.Exec(ctx, `INSERT INTO events(id,endpoint_id,event_type,payload,created_at)
        SELECT md5('event-' || n)::uuid,$1,'synthetic',decode('7b7d','hex'),$2 FROM generate_series(1,1001) n`, endpoint, testNow)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO delivery_attempts(id,event_id,endpoint_id,state,available_at,created_at,updated_at,expires_at)
        SELECT md5('attempt-' || n)::uuid,md5('event-' || n)::uuid,$1,'pending',$2,$2,$2,$2 FROM generate_series(1,1001) n`, endpoint, testNow)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []int{1000, 1001} {
		if got, err := s.ClaimAvailable(ctx, "sweeper", testNow, time.Minute, 1); err != nil || len(got) != 0 {
			t.Fatalf("expired send %v %v", got, err)
		}
		var completed, claims int
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FILTER(WHERE state='dead_letter'),sum(claim_count) FROM delivery_attempts`).Scan(&completed, &claims); err != nil || completed != want || claims != 0 {
			t.Fatalf("sweep completed=%d claims=%d err=%v", completed, claims, err)
		}
	}
}
