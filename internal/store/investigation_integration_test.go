package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestInvestigationSearchAndReferenceIdentity(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	p := accessPrincipal(t, s)
	endpoint := insertTestEndpoint(t, s)
	event := Event{ID: mustID(t), EndpointID: endpoint, EventType: "order.created", ProducerReference: "synthetic-order-42", CreatedAt: testNow}
	first, err := s.IngestEvent(ctx, event, []byte(`{"private":"payload-marker"}`), mustID(t), p.ID, "reference-key")
	if err != nil {
		t.Fatal(err)
	}
	event.ID = mustID(t)
	repeat, err := s.IngestEvent(ctx, event, []byte(`{"private":"payload-marker"}`), mustID(t), p.ID, "reference-key")
	if err != nil || repeat.Event != first.Event || !repeat.Repeated {
		t.Fatalf("receipt changed: %+v %v", repeat, err)
	}
	event.ProducerReference = "changed"
	if _, err := s.IngestEvent(ctx, event, []byte(`{"private":"payload-marker"}`), mustID(t), p.ID, "reference-key"); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatal("changed reference accepted")
	}
	event.ProducerReference = ""
	if _, err := s.IngestEvent(ctx, event, []byte(`{"private":"payload-marker"}`), mustID(t), p.ID, "reference-key"); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatal("removed reference accepted")
	}
	c := claimOne(t, s, testNow)
	due := testNow.Add(time.Minute)
	finish(t, s, c, testNow, Outcome{Code: "http_status", Status: 503, Message: "private-error-marker", RetryAt: &due})
	until := testNow.Add(time.Second)
	page, err := s.SearchEvents(ctx, EventFilter{EndpointID: endpoint, From: &testNow, Until: &until, ProducerReference: "synthetic-order-42", State: "pending"}, "", 100)
	if err != nil || len(page.Events) != 1 {
		t.Fatalf("search: %+v %v", page, err)
	}
	found := page.Events[0]
	if found.ID != first.Event.ID || found.NextAttemptAt == nil || !found.NextAttemptAt.Equal(due) || found.FailureCode != "http_status" || *found.ResponseStatus != 503 {
		t.Fatalf("missing investigation details: %+v", found)
	}
	raw, _ := json.Marshal(page)
	for _, secret := range []string{"payload-marker", "private-error-marker", "secret_ciphertext", "reference-key"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("search leaked private data")
		}
	}
	if _, err := s.pool.Exec(ctx, `UPDATE delivery_attempts SET error_code='private-code-marker' WHERE id=$1`, c.AttemptID); err != nil {
		t.Fatal(err)
	}
	unknown, err := s.SearchEvents(ctx, EventFilter{ProducerReference: "synthetic-order-42"}, "", 100)
	if err != nil || len(unknown.Events) != 1 || unknown.Events[0].FailureCode != "delivery_error" {
		t.Fatal("unknown failure was not redacted")
	}
	excluded, err := s.SearchEvents(ctx, EventFilter{Until: &testNow}, "", 100)
	if err != nil || len(excluded.Events) != 0 {
		t.Fatal("until is not exclusive")
	}
	// Identical timestamps require the UUID tie breaker; fetch one lookahead row.
	for i := 0; i < 101; i++ {
		seed(t, s, endpoint)
	}
	all, err := s.SearchEvents(ctx, EventFilter{}, "", 100)
	if err != nil || len(all.Events) != 100 || all.NextAfter == "" {
		t.Fatal("page bound failed", err)
	}
	next, err := s.SearchEvents(ctx, EventFilter{}, all.NextAfter, 100)
	if err != nil || len(next.Events) != 2 || next.NextAfter != "" {
		t.Fatalf("second page: %+v %v", next, err)
	}
	seen := map[string]bool{}
	for _, e := range append(all.Events, next.Events...) {
		if seen[e.ID] {
			t.Fatal("duplicate cursor row")
		}
		seen[e.ID] = true
	}
	if _, err := s.SearchEvents(ctx, EventFilter{State: "failed"}, all.NextAfter, 100); !errors.Is(err, ErrInvalidSearch) {
		t.Fatal("cursor reused with different filters")
	}
	for _, f := range []EventFilter{{EndpointID: "bad"}, {State: "bogus"}, {ProducerReference: "bad value"}, {From: &until, Until: &testNow}} {
		if _, err := s.SearchEvents(ctx, f, "", 100); !errors.Is(err, ErrInvalidSearch) {
			t.Fatal("invalid filter accepted")
		}
	}
	for _, cursor := range []string{"%", strings.Repeat("a", 1025)} {
		if _, err := s.SearchEvents(ctx, EventFilter{}, cursor, 100); !errors.Is(err, ErrInvalidSearch) {
			t.Fatal("invalid cursor accepted")
		}
	}
	if _, err := s.SearchEvents(ctx, EventFilter{}, "", 101); !errors.Is(err, ErrInvalidSearch) {
		t.Fatal("unbounded search")
	}
}

func TestConcurrentRecoveryPreview(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	p := accessPrincipal(t, s)
	input := BatchRequest{RequestID: mustID(t), PrincipalID: p.ID, Actor: p.Name, Reason: "reviewed", Events: recoverySelection(t, s, insertTestEndpoint(t, s), 2)}
	var wg sync.WaitGroup
	results := make(chan ReplayBatch, 8)
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			batch, err := s.PreviewBatch(ctx, input, testNow)
			results <- batch
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for batch := range results {
		if batch.ID != input.RequestID || batch.State != "preview" || len(batch.Items) != 2 {
			t.Fatal("concurrent preview changed selection")
		}
	}
	var batches, items, attempts int
	if err := s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM replay_batches),(SELECT count(*) FROM replay_batch_items),(SELECT count(*) FROM delivery_attempts)`).Scan(&batches, &items, &attempts); err != nil || batches != 1 || items != 2 || attempts != 2 {
		t.Fatal("preview duplicated or dispatched work", err)
	}
}

func recoverySelection(t *testing.T, s *Store, endpoint string, n int) []ReplaySelection {
	t.Helper()
	items := make([]ReplaySelection, 0, n)
	for i := 0; i < n; i++ {
		eventID := seed(t, s, endpoint)
		c := claimOne(t, s, testNow)
		finish(t, s, c, testNow, Outcome{Code: "http_status", Status: 400})
		items = append(items, ReplaySelection{eventID, c.AttemptID})
	}
	return items
}

func TestBulkRecoveryAtomicityRestartAndBounds(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	p := accessPrincipal(t, s)
	endpoint := insertTestEndpoint(t, s)
	selection := recoverySelection(t, s, endpoint, 12)
	input := BatchRequest{RequestID: mustID(t), PrincipalID: p.ID, Actor: p.Name, Reason: "synthetic-recovery-reason", Events: selection}
	preview, err := s.PreviewBatch(ctx, input, testNow)
	if err != nil || preview.State != "preview" || len(preview.Items) != 12 {
		t.Fatal("preview failed", err)
	}
	for _, item := range preview.Items {
		if !item.PreviewEligible || item.Result != "pending" {
			t.Fatal("bad preview")
		}
	}
	// Fault at the result write must also roll back the replay attempt and start.
	if _, err := s.pool.Exec(ctx, `ALTER TABLE replay_batch_items ADD CONSTRAINT reject_result CHECK(result='pending')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RunBatch(ctx, input.RequestID, p.ID, testNow); err == nil {
		t.Fatal("expected result write failure")
	}
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM delivery_attempts`).Scan(&total); err != nil || total != 12 {
		t.Fatal("replay escaped failed result transaction")
	}
	after, err := s.GetBatch(ctx, input.RequestID)
	if err != nil || after.StartedAt != nil {
		t.Fatal("failed item started batch")
	}
	if _, err := s.pool.Exec(ctx, `ALTER TABLE replay_batch_items DROP CONSTRAINT reject_result`); err != nil {
		t.Fatal(err)
	}
	// One committed item followed by lost response/process restart.
	if _, err := s.runBatchItem(ctx, input.RequestID, p.ID, testNow); err != nil {
		t.Fatal(err)
	}
	fresh, err := Open(ctx, s.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	progress, err := fresh.RunBatch(ctx, input.RequestID, p.ID, testNow)
	if err != nil || progress.State != "running" {
		t.Fatal("resume failed", err)
	}
	pending := 0
	for _, item := range progress.Items {
		if item.Result == "pending" {
			pending++
		}
	}
	if pending != 1 {
		t.Fatalf("call exceeded ten-item bound: pending %d", pending)
	}
	complete, err := fresh.RunBatch(ctx, input.RequestID, p.ID, testNow)
	if err != nil || complete.State != "completed" {
		t.Fatal("completion failed", err)
	}
	again, err := fresh.RunBatch(ctx, input.RequestID, p.ID, testNow.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for i, item := range again.Items {
		if item.Result != "replayed" || *item.ReplayAttemptID != *complete.Items[i].ReplayAttemptID {
			t.Fatal("repeated recovery changed results")
		}
	}
	for _, item := range selection {
		history, err := s.ListAttempts(ctx, item.EventID)
		if err != nil || len(history) != 2 || *history[1].ReplayPrincipalID != p.ID || *history[1].ReplayReason != input.Reason {
			t.Fatal("duplicate or unattributed replay")
		}
	}
	// A reordered identical set is the same request; changed reason/owner is not.
	input.Events = append([]ReplaySelection(nil), selection...)
	input.Events[0], input.Events[1] = input.Events[1], input.Events[0]
	repeated, err := s.PreviewBatch(ctx, input, testNow)
	if err != nil || repeated.State != "completed" {
		t.Fatal("preview was not idempotent", err)
	}
	input.Reason = "different"
	if _, err := s.PreviewBatch(ctx, input, testNow); !errors.Is(err, ErrConflict) {
		t.Fatal("conflicting request accepted")
	}
	if _, err := s.RunBatch(ctx, input.RequestID, mustID(t), testNow); !errors.Is(err, ErrConflict) {
		t.Fatal("another actor resumed batch")
	}
	for _, items := range [][]ReplaySelection{nil, {selection[0], selection[0]}, make([]ReplaySelection, 101), {{selection[0].EventID, "bad"}}} {
		input.RequestID = mustID(t)
		input.Events = items
		if _, err := s.PreviewBatch(ctx, input, testNow); !errors.Is(err, ErrInvalidBatch) {
			t.Fatal("invalid selection accepted", err)
		}
	}
	input.RequestID = mustID(t)
	input.Events = []ReplaySelection{{selection[0].EventID, selection[1].AttemptID}}
	if _, err := s.PreviewBatch(ctx, input, testNow); !errors.Is(err, ErrNotFound) {
		t.Fatal("cross-event attempt accepted")
	}
	if _, err := s.GetBatch(ctx, input.RequestID); !errors.Is(err, ErrNotFound) {
		t.Fatal("bad preview left a batch")
	}
}

func TestConcurrentRecoveryRechecksEligibilityAndLimits(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	p := accessPrincipal(t, s)
	endpoint := insertTestEndpoint(t, s)
	selection := recoverySelection(t, s, endpoint, 8)
	// Include an active event. Even if it fails later it was not approved by preview.
	pendingID := seed(t, s, endpoint)
	history, _ := s.ListAttempts(ctx, pendingID)
	selection = append(selection, ReplaySelection{pendingID, history[0].ID})
	batches := []string{mustID(t), mustID(t)}
	for _, batchID := range batches {
		if _, err := s.PreviewBatch(ctx, BatchRequest{RequestID: batchID, PrincipalID: p.ID, Actor: p.Name, Reason: "recovery", Events: selection}, testNow); err != nil {
			t.Fatal(err)
		}
	}
	active := claimOne(t, s, testNow)
	finish(t, s, active, testNow, Outcome{Code: "http_status", Status: 400})
	// Single replay wins one selection before bulk confirmation, and succeeds.
	single, err := s.Replay(ctx, selection[0].EventID, ReplayRequest{AttemptID: selection[0].AttemptID, RequestID: mustID(t), PrincipalID: p.ID, Actor: p.Name, Reason: "single"}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	c := claimOne(t, s, testNow)
	if c.AttemptID != single.AttemptID {
		t.Fatal("wrong replay claim")
	}
	finish(t, s, c, testNow, Outcome{Status: 204})
	if _, err := s.pool.Exec(ctx, `UPDATE webhook_endpoints SET paused=true,concurrency_limit=2,rate_limit=2,rate_used=0 WHERE id=$1`, endpoint); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); _, err := s.RunBatch(ctx, batches[i%2], p.ID, testNow); errs <- err }(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	replays, skips := 0, 0
	for _, batchID := range batches {
		b, err := s.GetBatch(ctx, batchID)
		if err != nil || b.State != "completed" {
			t.Fatal("concurrent recovery incomplete")
		}
		for _, item := range b.Items {
			switch item.Result {
			case "replayed":
				replays++
			case "skipped":
				skips++
			default:
				t.Fatal("unresolved item")
			}
		}
	}
	if replays != 7 || skips != 11 {
		t.Fatalf("replayed %d skipped %d", replays, skips)
	}
	for i, item := range selection {
		h, _ := s.ListAttempts(ctx, item.EventID)
		want := 2
		if i == 8 {
			want = 1
		}
		if len(h) != want {
			t.Fatal("duplicate or unapproved replay")
		}
	}
	if claims, err := s.ClaimAvailable(ctx, "bulk-worker", testNow, time.Minute, 100); err != nil || len(claims) != 0 {
		t.Fatal("bulk bypassed pause")
	}
	if _, err := s.pool.Exec(ctx, `UPDATE webhook_endpoints SET paused=false WHERE id=$1`, endpoint); err != nil {
		t.Fatal(err)
	}
	if claims, err := s.ClaimAvailable(ctx, "bulk-worker", testNow, time.Minute, 100); err != nil || len(claims) != 2 {
		t.Fatal("bulk bypassed concurrency/rate", err)
	}
	if claims, err := s.ClaimAvailable(ctx, "other-worker", testNow, time.Minute, 100); err != nil || len(claims) != 0 {
		t.Fatal("concurrent worker bypassed bulk limits")
	}
}
