package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/KleitonBarone/webhook-redrive/internal/testdb"
	"github.com/KleitonBarone/webhook-redrive/migrations"
)

var testNow = time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)

func seed(t *testing.T, s *Store, endpointID string) string {
	t.Helper()
	eventID := mustID(t)
	if err := s.CreateEvent(context.Background(), Event{ID: eventID, EndpointID: endpointID, EventType: "test", CreatedAt: testNow}, []byte("{\n \"private\":true\n}"), mustID(t)); err != nil {
		t.Fatal(err)
	}
	return eventID
}

func claimOne(t *testing.T, s *Store, now time.Time) ClaimedDelivery {
	t.Helper()
	claims, err := s.ClaimAvailable(context.Background(), "worker", now, time.Minute, 1)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claim: %v %v", claims, err)
	}
	return claims[0]
}

func finish(t *testing.T, s *Store, c ClaimedDelivery, now time.Time, outcome Outcome) {
	t.Helper()
	ok, err := s.Complete(context.Background(), c, "worker", now, outcome)
	if err != nil || !ok {
		t.Fatalf("complete: updated=%v err=%v", ok, err)
	}
}

func TestRetrySchedulingExhaustionAndReplay(t *testing.T) {
	s := integrationStore(t)
	endpointID := insertTestEndpoint(t, s)
	if _, err := s.pool.Exec(context.Background(), "UPDATE webhook_endpoints SET max_attempts=3 WHERE id=$1", endpointID); err != nil {
		t.Fatal(err)
	}
	eventID := seed(t, s, endpointID)
	now := testNow
	var last ClaimedDelivery
	for n := 1; n <= 3; n++ {
		last = claimOne(t, s, now)
		due := now.Add(2 * time.Second)
		finish(t, s, last, now, Outcome{Status: 503, Code: "http_status", RetryAt: &due})
		attempts, err := s.ListAttempts(context.Background(), eventID)
		if err != nil {
			t.Fatal(err)
		}
		want := n + 1
		if n == 3 {
			want = 3
		}
		if len(attempts) != want {
			t.Fatalf("attempt count=%d want=%d", len(attempts), want)
		}
		claims, err := s.ClaimAvailable(context.Background(), "early", due.Add(-time.Microsecond), time.Minute, 10)
		if err != nil || len(claims) != 0 {
			t.Fatalf("claimed before due: %v %v", claims, err)
		}
		now = due
	}
	event, err := s.GetEvent(context.Background(), eventID)
	if err != nil || event.State != "dead_letter" {
		t.Fatalf("event=%+v err=%v", event, err)
	}
	dead, err := s.ListDeadLetters(context.Background(), "")
	if err != nil || len(dead) != 1 || dead[0].ID != eventID {
		t.Fatalf("dead letters=%v err=%v", dead, err)
	}
	input := ReplayRequest{AttemptID: last.AttemptID, RequestID: mustID(t), Actor: "synthetic-operator", Reason: "receiver repaired"}
	result, err := s.Replay(context.Background(), eventID, input, now)
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := s.Replay(context.Background(), eventID, input, now)
	if err != nil || duplicate != result {
		t.Fatalf("duplicate replay=%v err=%v", duplicate, err)
	}
	conflicting := input
	conflicting.Reason = "different reason"
	if _, err := s.Replay(context.Background(), eventID, conflicting, now); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected conflict: %v", err)
	}
	replay := claimOne(t, s, now)
	if replay.EventID != eventID || replay.CycleAttempt != 1 || string(replay.Payload) != "{\n \"private\":true\n}" {
		t.Fatalf("replay changed event: %+v", replay)
	}
	finish(t, s, replay, now, Outcome{Status: 204})
	attempts, err := s.ListAttempts(context.Background(), eventID)
	if err != nil {
		t.Fatal(err)
	}
	a := attempts[3]
	if a.ReplayOf == nil || *a.ReplayOf != last.AttemptID || a.ReplayActor == nil || *a.ReplayActor != input.Actor || a.ReplayReason == nil || *a.ReplayReason != input.Reason {
		t.Fatalf("missing audit: %+v", a)
	}
	if a.State != "succeeded" || a.AttemptNumber != 4 {
		t.Fatalf("replay history: %+v", a)
	}
	dead, err = s.ListDeadLetters(context.Background(), "")
	if err != nil || len(dead) != 0 {
		t.Fatalf("replayed event remains dead: %v %v", dead, err)
	}
	duplicate, err = s.Replay(context.Background(), eventID, input, now)
	if err != nil || duplicate != result {
		t.Fatalf("replay dedupe after success: %v %v", duplicate, err)
	}
}

func TestCompletionFencesOldGenerationAndIsAtomic(t *testing.T) {
	s := integrationStore(t)
	eventID := seed(t, s, insertTestEndpoint(t, s))
	old := claimOne(t, s, testNow)
	now := testNow.Add(time.Minute)
	due := now.Add(time.Second)
	outcome := Outcome{Code: "timeout", RetryAt: &due}
	if ok, err := s.Complete(context.Background(), old, "worker", now, outcome); err != nil || ok {
		t.Fatalf("expired completion accepted: %v %v", ok, err)
	}
	current := claimOne(t, s, now)
	if ok, err := s.Complete(context.Background(), old, "worker", now, outcome); err != nil || ok {
		t.Fatalf("old generation accepted: %v %v", ok, err)
	}
	// Fail only the successor INSERT to prove the preceding UPDATE rolls back.
	if _, err := s.pool.Exec(context.Background(), `ALTER TABLE delivery_attempts ADD CONSTRAINT fail_successor CHECK (attempt_number=1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Complete(context.Background(), current, "worker", now, outcome); err == nil {
		t.Fatal("expected successor insert failure")
	}
	attempts, err := s.ListAttempts(context.Background(), eventID)
	if err != nil || len(attempts) != 1 || attempts[0].State != "in_progress" {
		t.Fatalf("partial completion committed: %v %v", attempts, err)
	}
	if _, err := s.pool.Exec(context.Background(), `ALTER TABLE delivery_attempts DROP CONSTRAINT fail_successor`); err != nil {
		t.Fatal(err)
	}
	finish(t, s, current, now, outcome)
	if ok, err := s.Complete(context.Background(), current, "worker", now, outcome); err != nil || ok {
		t.Fatalf("completion duplicated: %v %v", ok, err)
	}
	attempts, err = s.ListAttempts(context.Background(), eventID)
	if err != nil || len(attempts) != 2 {
		t.Fatalf("duplicate successor: %v %v", attempts, err)
	}
}

func TestConcurrentReplaysCreateOneAttempt(t *testing.T) {
	s := integrationStore(t)
	eventID := seed(t, s, insertTestEndpoint(t, s))
	claim := claimOne(t, s, testNow)
	finish(t, s, claim, testNow, Outcome{Status: 400, Code: "http_status"})
	input := ReplayRequest{AttemptID: claim.AttemptID, RequestID: mustID(t), Actor: "test", Reason: "fixed"}
	var wg sync.WaitGroup
	results := make(chan ReplayResult, 12)
	for n := 0; n < 12; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := s.Replay(context.Background(), eventID, input, testNow)
			if err != nil {
				t.Error(err)
				return
			}
			results <- result
		}()
	}
	wg.Wait()
	close(results)
	var first string
	for result := range results {
		if first == "" {
			first = result.AttemptID
		}
		if first != result.AttemptID {
			t.Fatal("duplicate replay attempts")
		}
	}
	attempts, err := s.ListAttempts(context.Background(), eventID)
	if err != nil || len(attempts) != 2 {
		t.Fatalf("history=%v err=%v", attempts, err)
	}
	input.RequestID = mustID(t)
	if _, err := s.Replay(context.Background(), eventID, input, testNow); !errors.Is(err, ErrConflict) {
		t.Fatalf("active replay should conflict: %v", err)
	}
}

func TestEndpointLimitsAreSharedAndDoNotBlockOtherEndpoints(t *testing.T) {
	s := integrationStore(t)
	endpointID := insertTestEndpoint(t, s)
	if _, err := s.pool.Exec(context.Background(), "UPDATE webhook_endpoints SET concurrency_limit=2,rate_limit=3 WHERE id=$1", endpointID); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 10; n++ {
		seed(t, s, endpointID)
	}
	var wg sync.WaitGroup
	results := make(chan []ClaimedDelivery, 12)
	for n := 0; n < 12; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			claimed, err := s.ClaimAvailable(context.Background(), "worker", testNow, time.Minute, 10)
			if err != nil {
				t.Error(err)
				return
			}
			results <- claimed
		}()
	}
	wg.Wait()
	close(results)
	var claimed []ClaimedDelivery
	for batch := range results {
		claimed = append(claimed, batch...)
	}
	if len(claimed) != 2 {
		t.Fatalf("concurrency allowed %d requests", len(claimed))
	}
	for _, c := range claimed {
		finish(t, s, c, testNow, Outcome{Status: 204})
	}
	lastPermit := claimOne(t, s, testNow)
	finish(t, s, lastPermit, testNow, Outcome{Status: 204})
	blocked, err := s.ClaimAvailable(context.Background(), "worker", testNow.Add(999*time.Millisecond), time.Minute, 10)
	if err != nil || len(blocked) != 0 {
		t.Fatalf("rate exceeded: %v %v", blocked, err)
	}
	other := insertTestEndpoint(t, s)
	otherEvent := seed(t, s, other)
	healthy := claimOne(t, s, testNow)
	if healthy.EventID != otherEvent {
		t.Fatal("rate limited endpoint blocked healthy endpoint")
	}
	resumed, err := s.ClaimAvailable(context.Background(), "worker", testNow.Add(time.Second), time.Minute, 10)
	if err != nil || len(resumed) != 2 {
		t.Fatalf("new window: %v %v", resumed, err)
	}
	// Expired leases free local capacity, but reclaiming still consumes permits.
	recovered, err := s.ClaimAvailable(context.Background(), "worker", testNow.Add(61*time.Second), time.Minute, 10)
	if err != nil || len(recovered) != 3 {
		t.Fatalf("recovery capacity: %v %v", recovered, err)
	}
}

func TestMigrationPreservesMilestoneOneData(t *testing.T) {
	s, err := Open(context.Background(), testdb.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	sql, err := migrations.Files.ReadFile("001_initial.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(context.Background(), string(sql)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(context.Background(), `CREATE TABLE schema_migrations(version text PRIMARY KEY,applied_at timestamptz NOT NULL);
        INSERT INTO schema_migrations VALUES ('001_initial.sql',now())`); err != nil {
		t.Fatal(err)
	}
	endpoint, event, attempt := mustID(t), mustID(t), mustID(t)
	if _, err := s.pool.Exec(context.Background(), `INSERT INTO webhook_endpoints VALUES ($1,'http://synthetic.test',$2,$3)`, endpoint, []byte("encrypted"), testNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(context.Background(), `INSERT INTO events VALUES ($1,$2,'test',$3,$4)`, event, endpoint, []byte("{}"), testNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(context.Background(), `INSERT INTO delivery_attempts(id,event_id,state,available_at,created_at,updated_at)
        VALUES($1,$2,'failed',$3,$3,$3)`, attempt, event, testNow); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(context.Background(), testNow); err != nil {
		t.Fatal(err)
	}
	history, err := s.ListAttempts(context.Background(), event)
	if err != nil || len(history) != 1 || history[0].State != "failed" || history[0].AttemptNumber != 1 {
		t.Fatalf("migration history=%v err=%v", history, err)
	}
	input := ReplayRequest{AttemptID: attempt, RequestID: mustID(t), Actor: "test", Reason: "upgrade"}
	if _, err := s.Replay(context.Background(), event, input, testNow); err != nil {
		t.Fatal(err)
	}
}
