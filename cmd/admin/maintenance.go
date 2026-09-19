package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"time"

	"github.com/KleitonBarone/webhook-redrive/internal/config"
	"github.com/KleitonBarone/webhook-redrive/internal/secret"
	"github.com/KleitonBarone/webhook-redrive/internal/store"
)

func maintenance(args []string, output io.Writer) error {
	f := flag.NewFlagSet(args[0], flag.ContinueOnError)
	f.SetOutput(io.Discard)
	apply := f.Bool("apply", false, "commit changes (default preview)")
	days := f.Int("days", 90, "shared retention horizon in days")
	limit := f.Int("limit", 100, "maximum events, batches, and rows per audit table")
	offline := f.Bool("offline", false, "confirm API and workers are stopped for key rotation")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if f.NArg() != 0 || *days < 1 || *days > 3650 || *limit < 1 || *limit > 100 {
		return errors.New("invalid maintenance options")
	}
	var old, next *secret.Box
	if args[0] == "rotate-master-key" {
		if !*offline {
			return errors.New("key rotation requires offline confirmation")
		}
		var err error
		old, err = secret.NewBoxFromBase64(os.Getenv("MASTER_KEY"))
		if err != nil {
			return err
		}
		next, err = secret.NewBoxFromBase64(os.Getenv("NEW_MASTER_KEY"))
		if err != nil {
			return err
		}
	}
	databaseURL, err := config.Required("DATABASE_URL")
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := store.Open(ctx, databaseURL)
	if err != nil {
		return err
	}
	defer s.Close()
	// Upgrade explicitly through the service before maintenance. Preview must not migrate.
	now := time.Now().UTC()
	if args[0] == "retain" {
		r, err := s.Retain(ctx, now, *days, *limit, *apply)
		if err != nil {
			return err
		}
		return json.NewEncoder(output).Encode(r)
	}
	count, err := s.RotateMasterKey(ctx, old, next, now, *apply)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(struct {
		Endpoints int  `json:"endpoints"`
		Applied   bool `json:"applied"`
	}{count, *apply})
}
