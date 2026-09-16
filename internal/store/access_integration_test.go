package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/KleitonBarone/webhook-redrive/internal/auth"
)

func accessPrincipal(t *testing.T, s *Store) auth.Principal {
	t.Helper()
	p := auth.Principal{ID: mustID(t), Name: "synthetic-operator", Kind: "operator", Permissions: []string{auth.Replay}}
	if err := s.CreatePrincipal(context.Background(), p, testNow); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCredentialExpiryRevocationAndAuditAreDurable(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	p := accessPrincipal(t, s)
	token := "wr_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	hash, _ := auth.Hash(token)
	credentialID := mustID(t)
	if err := s.IssueCredential(ctx, p.ID, credentialID, hash, testNow, testNow.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, err := s.Authenticate(ctx, hash, testNow)
	if err != nil || got.ID != p.ID || !got.Allows(auth.Replay) || got.Allows(auth.Endpoints) {
		t.Fatalf("principal: %+v %v", got, err)
	}
	if _, err := s.Authenticate(ctx, hash, testNow.Add(time.Hour)); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatal("expired credential accepted")
	}
	if _, err := s.Authenticate(ctx, make([]byte, 32), testNow); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatal("unknown credential accepted")
	}
	for i := 0; i < 2; i++ {
		if err := s.RevokeCredential(ctx, credentialID, testNow.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	// A fresh pool proves there is no process-local revocation state.
	other, err := Open(ctx, s.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := other.Authenticate(ctx, hash, testNow.Add(2*time.Second)); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatal("revoked credential accepted")
	}
	var audits int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM credential_audit WHERE credential_id=$1 AND db_actor<>''`, credentialID).Scan(&audits); err != nil || audits != 2 {
		t.Fatalf("audits %d %v", audits, err)
	}
	var stored []byte
	if err := s.pool.QueryRow(ctx, `SELECT token_hash FROM credentials WHERE id=$1`, credentialID).Scan(&stored); err != nil || string(stored) == token || len(stored) != 32 {
		t.Fatal("plaintext credential stored")
	}
}

func TestCredentialMutationsRollBackWhenAuditFails(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	p := accessPrincipal(t, s)
	hash, _ := auth.Hash("wr_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	credentialID := mustID(t)
	if _, err := s.pool.Exec(ctx, `ALTER TABLE credential_audit ADD CONSTRAINT reject_audit CHECK(false)`); err != nil {
		t.Fatal(err)
	}
	if err := s.IssueCredential(ctx, p.ID, credentialID, hash, testNow, testNow.Add(time.Hour)); err == nil {
		t.Fatal("expected audit failure")
	}
	if _, err := s.Authenticate(ctx, hash, testNow); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatal("partial issuance committed")
	}
	if _, err := s.pool.Exec(ctx, `ALTER TABLE credential_audit DROP CONSTRAINT reject_audit`); err != nil {
		t.Fatal(err)
	}
	if err := s.IssueCredential(ctx, p.ID, credentialID, hash, testNow, testNow.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `ALTER TABLE credential_audit ADD CONSTRAINT reject_revoke CHECK(action='issued')`); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeCredential(ctx, credentialID, testNow); err == nil {
		t.Fatal("expected audit failure")
	}
	if _, err := s.Authenticate(ctx, hash, testNow); err != nil {
		t.Fatal("partial revocation committed")
	}
}

func TestReplayUsesStablePrincipalAndPreservesLegacyAttribution(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	p := accessPrincipal(t, s)
	endpointID := insertTestEndpoint(t, s)
	eventID := seed(t, s, endpointID)
	first := claimOne(t, s, testNow)
	finish(t, s, first, testNow, Outcome{Status: 400, Code: "http_status"})
	legacy := ReplayRequest{AttemptID: first.AttemptID, RequestID: mustID(t), Actor: "legacy-label", Reason: "repair"}
	if _, err := s.Replay(ctx, eventID, legacy, testNow); err != nil {
		t.Fatal(err)
	}
	forged := legacy
	forged.PrincipalID = p.ID
	if _, err := s.Replay(ctx, eventID, forged, testNow); !errors.Is(err, ErrConflict) {
		t.Fatal("legacy attribution upgraded by caller")
	}
	second := claimOne(t, s, testNow)
	finish(t, s, second, testNow, Outcome{Status: 400, Code: "http_status"})
	verified := ReplayRequest{AttemptID: second.AttemptID, RequestID: mustID(t), Actor: p.Name, PrincipalID: p.ID, Reason: "repair"}
	result, err := s.Replay(ctx, eventID, verified, testNow)
	if err != nil {
		t.Fatal(err)
	}
	verified.Actor = "changed display label"
	duplicate, err := s.Replay(ctx, eventID, verified, testNow)
	if err != nil || duplicate != result {
		t.Fatal("principal deduplication depends on display label")
	}
	other := accessPrincipal(t, s)
	verified.PrincipalID = other.ID
	if _, err := s.Replay(ctx, eventID, verified, testNow); !errors.Is(err, ErrConflict) {
		t.Fatal("another principal reused replay")
	}
	history, err := s.ListAttempts(ctx, eventID)
	if err != nil || len(history) != 3 || history[1].ReplayPrincipalID != nil || history[2].ReplayPrincipalID == nil || *history[2].ReplayPrincipalID != p.ID || *history[1].ReplayActor != "legacy-label" {
		t.Fatalf("history %+v %v", history, err)
	}
}
