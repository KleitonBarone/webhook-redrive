package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/KleitonBarone/webhook-redrive/internal/secret"
	"github.com/KleitonBarone/webhook-redrive/internal/testdb"
	"github.com/KleitonBarone/webhook-redrive/migrations"
)

func TestMasterKeyValidationRotationAndRollback(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	old, _ := secret.NewBox(bytes.Repeat([]byte{1}, 32))
	next, _ := secret.NewBox(bytes.Repeat([]byte{2}, 32))
	ids := []string{insertTestEndpoint(t, s), insertTestEndpoint(t, s)}
	for _, id := range ids {
		cipher, _ := old.Encrypt([]byte("synthetic-signing-secret"))
		if _, err := s.pool.Exec(ctx, `UPDATE webhook_endpoints SET secret_ciphertext=$2 WHERE id=$1`, id, cipher); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CheckMasterKey(ctx, next); !errors.Is(err, ErrMasterKey) {
		t.Fatalf("wrong upgrade key: %v", err)
	}
	if err := s.CheckMasterKey(ctx, old); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RotateMasterKey(ctx, old, next, testNow, false); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckMasterKey(ctx, next); !errors.Is(err, ErrMasterKey) {
		t.Fatal("preview mutated key")
	}
	// A failure after re-encryption must roll back all endpoints and the marker.
	if _, err := s.pool.Exec(ctx, `ALTER TABLE maintenance_audit ADD CONSTRAINT fail_rotation CHECK(action<>'master_key_rotation')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RotateMasterKey(ctx, old, next, testNow, true); err == nil {
		t.Fatal("expected audit failure")
	}
	if err := s.CheckMasterKey(ctx, old); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `ALTER TABLE maintenance_audit DROP CONSTRAINT fail_rotation`); err != nil {
		t.Fatal(err)
	}
	if count, err := s.RotateMasterKey(ctx, old, next, testNow, true); err != nil || count != 2 {
		t.Fatalf("rotation %d %v", count, err)
	}
	if err := s.CheckMasterKey(ctx, old); !errors.Is(err, ErrMasterKey) {
		t.Fatal("old key accepted")
	}
	if err := s.CheckMasterKey(ctx, next); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		var cipher []byte
		if err := s.pool.QueryRow(ctx, `SELECT secret_ciphertext FROM webhook_endpoints WHERE id=$1`, id).Scan(&cipher); err != nil {
			t.Fatal(err)
		}
		plain, err := next.Decrypt(cipher)
		if err != nil || string(plain) != "synthetic-signing-secret" {
			t.Fatal("signing secret changed")
		}
	}
}

func TestEmptyDatabaseBindsKey(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	old, _ := secret.NewBox(bytes.Repeat([]byte{1}, 32))
	wrong, _ := secret.NewBox(bytes.Repeat([]byte{2}, 32))
	if err := s.CheckMasterKey(ctx, old); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckMasterKey(ctx, wrong); !errors.Is(err, ErrMasterKey) {
		t.Fatal("empty database accepted another key")
	}
}

func TestAccountingFailureRollsBackDeliveryIntent(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	endpoint := insertTestEndpoint(t, s)
	if _, err := s.pool.Exec(ctx, `ALTER TABLE cumulative_metrics ADD CONSTRAINT synthetic_counter_failure CHECK(events=0)`); err != nil {
		t.Fatal(err)
	}
	event := Event{ID: mustID(t), EndpointID: endpoint, EventType: "synthetic", CreatedAt: testNow}
	if err := s.CreateEvent(ctx, event, []byte(`{}`), mustID(t)); err == nil {
		t.Fatal("expected deferred counter failure")
	}
	if _, err := s.GetEvent(ctx, event.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("event committed without accounting")
	}
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM delivery_attempts`).Scan(&count); err != nil || count != 0 {
		t.Fatal("attempt committed without accounting")
	}
}

func TestRetentionCutoffAndLimit(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	endpoint := insertTestEndpoint(t, s)
	for i := 0; i < 3; i++ {
		seed(t, s, endpoint)
		c := claimOne(t, s, testNow)
		finish(t, s, c, testNow, Outcome{Status: 204})
	}
	boundary := testNow.Add(90 * 24 * time.Hour)
	r, err := s.Retain(ctx, boundary, 90, 1, true)
	if err != nil || r.Events != 0 {
		t.Fatal("cutoff must be exclusive")
	}
	r, err = s.Retain(ctx, boundary.Add(time.Microsecond), 90, 1, true)
	if err != nil || r.Events != 1 {
		t.Fatalf("bound %+v %v", r, err)
	}
	var count int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM events`).Scan(&count); err != nil || count != 2 {
		t.Fatal("deleted beyond limit")
	}
	r, err = s.Retain(ctx, boundary.Add(time.Microsecond), 90, 1, true)
	if err != nil || r.Events != 1 {
		t.Fatal("restart did not continue")
	}
}

func TestOperationsMigrationBackfillsMetrics(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, testdb.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = s.pool.Exec(ctx, `CREATE TABLE schema_migrations(version text PRIMARY KEY,applied_at timestamptz NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	files, _ := migrations.Files.ReadDir(".")
	for _, file := range files {
		if file.Name() >= "008" {
			continue
		}
		sql, err := migrations.Files.ReadFile(file.Name())
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.pool.Exec(ctx, string(sql)); err != nil {
			t.Fatal(err)
		}
		if _, err = s.pool.Exec(ctx, `INSERT INTO schema_migrations VALUES($1,$2)`, file.Name(), testNow); err != nil {
			t.Fatal(err)
		}
	}
	endpoint := insertTestEndpoint(t, s)
	event := seed(t, s, endpoint)
	if _, err = s.pool.Exec(ctx, `UPDATE delivery_attempts SET state='succeeded',claim_count=2,last_started_at=$1::timestamptz,completed_at=$1::timestamptz+interval '1 second' WHERE event_id=$2`, testNow, event); err != nil {
		t.Fatal(err)
	}
	if err = s.Migrate(ctx, testNow); err != nil {
		t.Fatal(err)
	}
	m, err := s.Metrics(ctx, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if m.Events != 1 || m.Claims != 2 || m.Recoveries != 1 || m.Completed["succeeded"] != 1 || m.Latency["attempt"].Sum != 1 || m.Latency["attempt"].Buckets[1] != 1 {
		t.Fatalf("backfill %+v", m)
	}
	if err = s.Migrate(ctx, testNow); err != nil {
		t.Fatal(err)
	}
	after, err := s.Metrics(ctx, testNow)
	// Other isolated tests can grow the same physical database concurrently.
	m.DatabaseBytes, after.DatabaseBytes = 0, 0
	if err != nil || !reflect.DeepEqual(m, after) {
		t.Fatal("second migration changed totals")
	}
}

// Opt-in measurement, not a CI latency assertion. Rows are SQL-seeded terminal
// history, not wire deliveries. Every test still uses an isolated test schema.
func TestOperationsLargeHistory(t *testing.T) {
	if os.Getenv("MEASURE_HISTORY") != "true" {
		t.Skip("set MEASURE_HISTORY=true for 10000-event evidence")
	}
	s := integrationStore(t)
	ctx := context.Background()
	endpoint := insertTestEndpoint(t, s)
	began := time.Now()
	if _, err := s.pool.Exec(ctx, `WITH inserted AS (
 INSERT INTO events(id,endpoint_id,event_type,payload,created_at) SELECT gen_random_uuid(),$1,'synthetic',convert_to('{}','UTF8'),$2 FROM generate_series(1,10000) RETURNING id)
 INSERT INTO delivery_attempts(id,event_id,endpoint_id,state,available_at,created_at,updated_at,last_started_at,completed_at,claim_count)
 SELECT gen_random_uuid(),id,$1,'succeeded',$2,$2,$2,$2,$2+interval '1 second',1 FROM inserted`, endpoint, testNow); err != nil {
		t.Fatal(err)
	}
	seedSeconds := time.Since(began).Seconds()
	if _, err := s.pool.Exec(ctx, `ANALYZE events; ANALYZE delivery_attempts`); err != nil {
		t.Fatal(err)
	}
	now := testNow.Add(91 * 24 * time.Hour)
	began = time.Now()
	before, err := s.Metrics(ctx, now)
	if err != nil || before.Events != 10000 {
		t.Fatalf("metrics %+v %v", before, err)
	}
	metricsSeconds := time.Since(began).Seconds()
	began = time.Now()
	page, err := s.SearchEvents(ctx, EventFilter{EndpointID: endpoint, State: "succeeded"}, "", 100)
	if err != nil || len(page.Events) != 100 || page.NextAfter == "" {
		t.Fatalf("search %v", err)
	}
	searchSeconds := time.Since(began).Seconds()
	began = time.Now()
	r, err := s.Retain(ctx, now, 90, 100, true)
	if err != nil || r.Events != 100 {
		t.Fatalf("retention %+v %v", r, err)
	}
	retentionSeconds := time.Since(began).Seconds()
	after, err := s.Metrics(ctx, now)
	if err != nil || after.Events != 10000 || after.States["succeeded"] != 9900 || !reflect.DeepEqual(before.Latency, after.Latency) {
		t.Fatal("history deletion changed totals")
	}
	encoded, _ := json.Marshal(map[string]any{"seeded_events": 10000, "remaining_events": 9900, "seed_seconds": seedSeconds, "metrics_seconds": metricsSeconds, "search_page_seconds": searchSeconds, "retention_100_seconds": retentionSeconds, "database_bytes": after.DatabaseBytes})
	t.Log(string(encoded))
}

func TestRetentionPreservesIntentAndCountersExpiresKeysAtomically(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	endpoint := insertTestEndpoint(t, s)
	p := accessPrincipal(t, s)
	e := Event{ID: mustID(t), EndpointID: endpoint, EventType: "test", CreatedAt: testNow}
	if _, err := s.IngestEvent(ctx, e, []byte(`{}`), mustID(t), p.ID, "synthetic-key"); err != nil {
		t.Fatal(err)
	}
	claim := claimOne(t, s, testNow)
	finish(t, s, claim, testNow.Add(time.Second), Outcome{Status: 204})
	active := seed(t, s, endpoint)
	_ = claimOne(t, s, testNow.Add(time.Second)) // Expired leases are still durable intent.
	paused := seed(t, s, endpoint)
	if _, err := s.pool.Exec(ctx, `UPDATE webhook_endpoints SET paused=true WHERE id=$1`, endpoint); err != nil {
		t.Fatal(err)
	}
	now := testNow.Add(91 * 24 * time.Hour)
	before, err := s.Metrics(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := s.Retain(ctx, now, 90, 1, false)
	if err != nil || preview.Events != 1 {
		t.Fatalf("preview %+v %v", preview, err)
	}
	if _, err = s.GetEvent(ctx, e.ID); err != nil {
		t.Fatal("preview deleted event")
	}
	if _, err = s.pool.Exec(ctx, `CREATE FUNCTION fail_retention() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic fault'; END $$; CREATE TRIGGER fail_retention BEFORE DELETE ON events FOR EACH ROW EXECUTE FUNCTION fail_retention()`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Retain(ctx, now, 90, 1, true); err == nil {
		t.Fatal("expected deletion failure")
	}
	repeated, err := s.IngestEvent(ctx, e, []byte(`{}`), mustID(t), p.ID, "synthetic-key")
	if err != nil || !repeated.Repeated {
		t.Fatal("failed cleanup lost key")
	}
	if _, err = s.pool.Exec(ctx, `DROP TRIGGER fail_retention ON events`); err != nil {
		t.Fatal(err)
	}
	result, err := s.Retain(ctx, now, 90, 1, true)
	if err != nil || result.Events != 1 {
		t.Fatalf("retention %+v %v", result, err)
	}
	for _, id := range []string{active, paused} {
		if _, err = s.GetEvent(ctx, id); err != nil {
			t.Fatal("active intent removed")
		}
	}
	if _, err = s.GetEvent(ctx, e.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("old event retained")
	}
	after, err := s.Metrics(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if after.Events != before.Events || after.Claims != before.Claims || !reflect.DeepEqual(after.Latency, before.Latency) || !reflect.DeepEqual(after.Completed, before.Completed) {
		t.Fatalf("counters changed: %+v %+v", before, after)
	}
	e.ID = mustID(t)
	e.CreatedAt = now
	receipt, err := s.IngestEvent(ctx, e, []byte(`{}`), mustID(t), p.ID, "synthetic-key")
	if err != nil || receipt.Repeated {
		t.Fatal("expired key did not admit a new event")
	}
	result, err = s.Retain(ctx, now, 90, 1, true)
	if err != nil || result.Events != 0 {
		t.Fatalf("restart %+v %v", result, err)
	}
}

func TestRetentionBatchPinAndReplayRace(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	endpoint := insertTestEndpoint(t, s)
	p := accessPrincipal(t, s)
	e := seed(t, s, endpoint)
	c := claimOne(t, s, testNow)
	finish(t, s, c, testNow, Outcome{Status: 400, Code: "http_status"})
	now := testNow.Add(91 * 24 * time.Hour)
	batch, err := s.PreviewBatch(ctx, BatchRequest{RequestID: mustID(t), PrincipalID: p.ID, Actor: p.Name, Reason: "synthetic", Events: []ReplaySelection{{EventID: e, AttemptID: c.AttemptID}}}, now)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Retain(ctx, now, 90, 100, true)
	if err != nil || r.Events != 0 {
		t.Fatal("recent preview must pin history")
	}
	r, err = s.Retain(ctx, now.Add(91*24*time.Hour), 90, 100, true)
	if err != nil || r.Events != 1 || r.Batches != 1 {
		t.Fatalf("batch expiry %+v %v", r, err)
	}
	if _, err = s.GetBatch(ctx, batch.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("expired preview exists")
	}
	// Both outcomes are legal: deletion wins, or replay pins a new active cycle.
	for i := 0; i < 10; i++ {
		e = seed(t, s, endpoint)
		c = claimOne(t, s, testNow)
		finish(t, s, c, testNow, Outcome{Status: 400, Code: "http_status"})
		var wg sync.WaitGroup
		wg.Add(2)
		var replayErr, retainErr error
		var result ReplayResult
		go func() {
			defer wg.Done()
			result, replayErr = s.Replay(ctx, e, ReplayRequest{AttemptID: c.AttemptID, RequestID: mustID(t), Actor: "synthetic", Reason: "race"}, now)
		}()
		go func() { defer wg.Done(); _, retainErr = s.Retain(ctx, now, 90, 100, true) }()
		wg.Wait()
		if retainErr != nil {
			t.Fatal(retainErr)
		}
		if replayErr != nil && !errors.Is(replayErr, ErrNotFound) {
			t.Fatal(replayErr)
		}
		if replayErr == nil {
			event, err := s.GetEvent(ctx, result.EventID)
			if err != nil || event.State != "pending" {
				t.Fatal("replay intent lost")
			}
			// Finish this cycle so the next iteration's claim is unambiguous.
			claim := claimOne(t, s, now)
			finish(t, s, claim, now, Outcome{Status: 204})
		}
	}
}
