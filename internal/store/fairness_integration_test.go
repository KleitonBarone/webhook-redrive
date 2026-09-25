package store

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"
)

func fairnessBacklogs(t *testing.T, s *Store, count, events int) []string {
	t.Helper()
	endpoints := make([]string, count)
	for i := range endpoints {
		endpoints[i] = insertTestEndpoint(t, s)
		for range events {
			seed(t, s, endpoints[i])
		}
	}
	sort.Strings(endpoints)
	return endpoints
}

func TestFairClaimsRotateAcrossBatchesAndRestarts(t *testing.T) {
	s := integrationStore(t)
	endpoints := fairnessBacklogs(t, s, 3, 10)
	ctx := context.Background()
	claims, err := s.ClaimAvailable(ctx, "worker", testNow, time.Minute, 8)
	if err != nil || len(claims) != 8 {
		t.Fatalf("claims=%d err=%v", len(claims), err)
	}
	counts := map[string]int{}
	for _, c := range claims {
		counts[c.EndpointID]++
	}
	for i, want := range []int{3, 3, 2} {
		if counts[endpoints[i]] != want {
			t.Fatalf("batch distribution=%v", counts)
		}
	}
	// Reopen the pool after each single-slot claim. A frozen clock must not tie
	// service order and send every new slot back to the same endpoint.
	connection := s.pool.Config().ConnString()
	for _, i := range []int{2, 0, 1, 2, 0, 1} {
		s.Close()
		s, err = Open(ctx, connection)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(s.Close)
		c := claimOne(t, s, testNow)
		if c.EndpointID != endpoints[i] {
			t.Fatalf("claimed %s, want %s", c.EndpointID, endpoints[i])
		}
	}
	var version, audits int
	if err := s.pool.QueryRow(ctx, `SELECT version,(SELECT count(*) FROM endpoint_audit WHERE endpoint_id=$1) FROM webhook_endpoints WHERE id=$1`, endpoints[0]).Scan(&version, &audits); err != nil {
		t.Fatal(err)
	}
	if version != 1 || audits != 1 {
		t.Fatalf("scheduling changed configuration: version=%d audits=%d", version, audits)
	}
}

func TestFairClaimsUseSpareCapacityAndSkipIneligible(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	e := fairnessBacklogs(t, s, 6, 1)
	for range 19 {
		seed(t, s, e[1])
	}
	if _, err := s.pool.Exec(ctx, `UPDATE webhook_endpoints SET paused=true WHERE id=$1`, e[2]); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE webhook_endpoints SET rate_window=$2,rate_used=rate_limit WHERE id=$1`, e[3], testNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE delivery_attempts SET available_at=$2 WHERE endpoint_id=$1`, e[4], testNow.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE delivery_attempts SET expires_at=$2 WHERE endpoint_id=$1`, e[5], testNow); err != nil {
		t.Fatal(err)
	}
	claims, err := s.ClaimAvailable(ctx, "worker", testNow, time.Minute, 10)
	if err != nil || len(claims) != 10 {
		t.Fatalf("claims=%d err=%v", len(claims), err)
	}
	counts := map[string]int{}
	for _, c := range claims {
		counts[c.EndpointID]++
	}
	if counts[e[0]] != 1 || counts[e[1]] != 9 || len(counts) != 2 {
		t.Fatalf("distribution=%v", counts)
	}
	// Resume and a renewed rate window restore eligibility, ahead of recently
	// serviced backlogs. Expired and future attempts remain unclaimable.
	if _, err := s.pool.Exec(ctx, `UPDATE webhook_endpoints SET paused=false WHERE id=$1`, e[2]); err != nil {
		t.Fatal(err)
	}
	claims, err = s.ClaimAvailable(ctx, "worker", testNow.Add(time.Second), time.Minute, 2)
	if err != nil || len(claims) != 2 {
		t.Fatalf("claims=%d err=%v", len(claims), err)
	}
	counts = map[string]int{}
	for _, c := range claims {
		counts[c.EndpointID]++
	}
	if counts[e[2]] != 1 || counts[e[3]] != 1 {
		t.Fatalf("resumed distribution=%v", counts)
	}
}

func TestFairClaimsRollbackServiceOrderWithLeases(t *testing.T) {
	s := integrationStore(t)
	e := fairnessBacklogs(t, s, 2, 3)
	ctx := context.Background()
	// Fail after every claim and endpoint update, just before commit.
	if _, err := s.pool.Exec(ctx, `ALTER TABLE worker_progress ADD CONSTRAINT injected_failure CHECK (worker_id <> 'fail')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimAvailable(ctx, "fail", testNow, time.Minute, 3); err == nil {
		t.Fatal("expected injected failure")
	}
	var changed int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM webhook_endpoints WHERE last_service_seq<>0 OR rate_used<>0`).Scan(&changed); err != nil || changed != 0 {
		t.Fatalf("partial service order: %d %v", changed, err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM delivery_attempts WHERE state<>'pending' OR claim_count<>0`).Scan(&changed); err != nil || changed != 0 {
		t.Fatalf("partial claims: %d %v", changed, err)
	}
	first := claimOne(t, s, testNow)
	if first.EndpointID != e[0] {
		t.Fatal("rollback changed next endpoint")
	}
	// A committed claim followed by a crash retains service order. Recovery
	// reuses the logical attempt and fences the abandoned worker generation.
	second := claimOne(t, s, testNow.Add(time.Minute))
	if second.EndpointID != e[1] {
		t.Fatal("crash reset endpoint rotation")
	}
	recovered := claimOne(t, s, testNow.Add(time.Minute))
	if recovered.EndpointID != e[0] {
		t.Fatal("recovery did not rotate")
	}
	// Within an endpoint UUID order breaks equal available-time ties, so the
	// oldest claimed row is selected again at lease expiry.
	if recovered.AttemptID != first.AttemptID || recovered.ClaimCount != 2 {
		t.Fatalf("recovery=%+v", recovered)
	}
	if ok, err := s.Complete(ctx, first, "worker", testNow.Add(time.Minute), Outcome{Status: 204}); err != nil || ok {
		t.Fatalf("stale completion=%v %v", ok, err)
	}
}

func TestFairClaimsConcurrentWorkersRespectEndpointBudgets(t *testing.T) {
	s := integrationStore(t)
	e := fairnessBacklogs(t, s, 4, 20)
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx, `UPDATE webhook_endpoints SET concurrency_limit=4,rate_limit=4`); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan []ClaimedDelivery, 16)
	var wg sync.WaitGroup
	for n := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			claims, err := s.ClaimAvailable(ctx, mustWorkerID(n), testNow, time.Minute, 3)
			if err != nil {
				t.Error(err)
				return
			}
			if len(claims) > 3 {
				t.Error("exceeded local slots")
			}
			results <- claims
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	seen, counts := map[string]bool{}, map[string]int{}
	for batch := range results {
		for _, c := range batch {
			recordClaim(t, seen, c)
			counts[c.EndpointID]++
		}
	}
	// Locked candidates may be skipped. Drain once after contenders finish.
	remaining, err := s.ClaimAvailable(ctx, "drain", testNow, time.Minute, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range remaining {
		recordClaim(t, seen, c)
		counts[c.EndpointID]++
	}
	for _, endpoint := range e {
		if counts[endpoint] != 4 {
			t.Fatalf("endpoint budgets=%v", counts)
		}
	}
	if len(seen) != 16 {
		t.Fatalf("claimed=%d", len(seen))
	}
}

func TestFairClaimsSkipLockedEndpoint(t *testing.T) {
	s := integrationStore(t)
	e := fairnessBacklogs(t, s, 2, 12)
	ctx := context.Background()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT id FROM webhook_endpoints WHERE id=$1 FOR NO KEY UPDATE`, e[0]); err != nil {
		t.Fatal(err)
	}
	claims, err := s.ClaimAvailable(ctx, "worker", testNow, time.Minute, 10)
	if err != nil || len(claims) != 10 {
		t.Fatalf("claims=%d err=%v", len(claims), err)
	}
	for _, c := range claims {
		if c.EndpointID != e[1] {
			t.Fatal("crossed endpoint lock")
		}
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if c := claimOne(t, s, testNow); c.EndpointID != e[0] {
		t.Fatal("skipped endpoint lost its turn")
	}
}

func TestFairClaimsPreferFewerLiveClaims(t *testing.T) {
	s := integrationStore(t)
	slow := fairnessBacklogs(t, s, 1, 20)[0]
	ctx := context.Background()
	occupied, err := s.ClaimAvailable(ctx, "worker", testNow, time.Minute, 8)
	if err != nil || len(occupied) != 8 {
		t.Fatalf("occupied=%d err=%v", len(occupied), err)
	}
	healthy := fairnessBacklogs(t, s, 1, 12)[0]
	for range 2 {
		if c := claimOne(t, s, testNow); c.EndpointID != healthy {
			t.Fatal("ignored existing slot occupancy")
		}
	}
	// Another worker has eight slots free. Fill toward equal global occupancy,
	// not equal new-claim counts, while preserving every existing lease.
	claims, err := s.ClaimAvailable(ctx, "second-worker", testNow, time.Minute, 8)
	if err != nil || len(claims) != 8 {
		t.Fatalf("claims=%d err=%v", len(claims), err)
	}
	counts := map[string]int{}
	for _, c := range claims {
		counts[c.EndpointID]++
	}
	if counts[slow] != 1 || counts[healthy] != 7 {
		t.Fatalf("distribution=%v", counts)
	}
	finish(t, s, occupied[0], testNow, Outcome{Status: 204})
}
