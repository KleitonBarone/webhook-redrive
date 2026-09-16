package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/KleitonBarone/webhook-redrive/internal/auth"
	"github.com/KleitonBarone/webhook-redrive/internal/store"
	"github.com/KleitonBarone/webhook-redrive/internal/testdb"
)

func TestCredentialAdministration(t *testing.T) {
	databaseURL := testdb.URL(t)
	t.Setenv("DATABASE_URL", databaseURL)
	var output bytes.Buffer
	if err := run([]string{"create", "--name", "synthetic-operator", "--kind", "operator", "--permissions", "inspect,replay"}, &output); err != nil {
		t.Fatal(err)
	}
	var p auth.Principal
	if err := json.Unmarshal(output.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := run([]string{"issue", "--principal", p.ID, "--ttl", "1h"}, &output); err != nil {
		t.Fatal(err)
	}
	var issued struct {
		Token        string
		CredentialID string `json:"credential_id"`
	}
	if err := json.Unmarshal(output.Bytes(), &issued); err != nil {
		t.Fatal(err)
	}
	hash, err := auth.Hash(issued.Token)
	if err != nil {
		t.Fatal("invalid credential issued")
	}
	s, err := store.Open(context.Background(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got, err := s.Authenticate(context.Background(), hash, time.Now()); err != nil || got.ID != p.ID {
		t.Fatal("issued credential not usable")
	}
	output.Reset()
	if err := run([]string{"revoke", "--credential", issued.CredentialID}, &output); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(context.Background(), hash, time.Now()); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatal("revocation failed")
	}
	if bytes.Contains(output.Bytes(), []byte(issued.Token)) {
		t.Fatal("revocation exposed token")
	}
}

func TestInvalidCommandOptionsDoNotConnect(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	for _, args := range [][]string{nil, {"unknown"}, {"create", "--permissions", "root"}, {"issue", "--principal", "bad"}, {"revoke", "--credential", "bad"}, {"create", "--name", "test", "--permissions", "inspect", "extra"}} {
		var output bytes.Buffer
		if err := run(args, &output); err == nil || output.Len() != 0 {
			t.Fatal("invalid command accepted")
		}
	}
}
