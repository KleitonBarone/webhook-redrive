// admin manages credentials and explicit maintenance through database access.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/KleitonBarone/webhook-redrive/internal/auth"
	"github.com/KleitonBarone/webhook-redrive/internal/config"
	"github.com/KleitonBarone/webhook-redrive/internal/id"
	"github.com/KleitonBarone/webhook-redrive/internal/store"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		// Driver/flag errors may contain input or connection credentials.
		fmt.Fprintln(os.Stderr, "admin command failed; check command options and database access")
		os.Exit(1)
	}
}

func run(args []string, output io.Writer) error {
	if len(args) > 0 && (args[0] == "retain" || args[0] == "rotate-master-key") {
		return maintenance(args, output)
	}
	if len(args) == 0 {
		return errors.New("expected create, issue, or revoke")
	}
	f := flag.NewFlagSet("admin", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	name := f.String("name", "", "identity name")
	kind := f.String("kind", "service", "service or operator")
	permissions := f.String("permissions", "", "comma-separated permissions")
	principal := f.String("principal", "", "principal UUID")
	credential := f.String("credential", "", "credential UUID")
	ttl := f.Duration("ttl", 30*24*time.Hour, "credential lifetime")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("unexpected arguments")
	}
	switch args[0] {
	case "create":
		if *name == "" || len(*name) > 100 || (*kind != "service" && *kind != "operator") || !auth.ValidPermissions(strings.Split(*permissions, ",")) {
			return errors.New("invalid principal")
		}
	case "issue":
		if !id.Valid(*principal) || *ttl <= 0 || *ttl > 365*24*time.Hour {
			return errors.New("invalid credential options")
		}
	case "revoke":
		if !id.Valid(*credential) {
			return errors.New("invalid credential ID")
		}
	default:
		return errors.New("unknown command")
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
	now := time.Now().UTC()
	if err := s.Migrate(ctx, now); err != nil {
		return err
	}
	switch args[0] {
	case "create":
		principalID, err := id.New()
		if err != nil {
			return err
		}
		p := auth.Principal{ID: principalID, Name: *name, Kind: *kind, Permissions: strings.Split(*permissions, ",")}
		if err := s.CreatePrincipal(ctx, p, now); err != nil {
			return err
		}
		return json.NewEncoder(output).Encode(p)
	case "issue":
		credentialID, err := id.New()
		if err != nil {
			return err
		}
		token, err := auth.Generate()
		if err != nil {
			return err
		}
		hash, err := auth.Hash(token)
		if err != nil {
			return err
		}
		expires := now.Add(*ttl)
		if err := s.IssueCredential(ctx, *principal, credentialID, hash, now, expires); err != nil {
			return err
		}
		return json.NewEncoder(output).Encode(struct {
			CredentialID string    `json:"credential_id"`
			Token        string    `json:"token"`
			Expires      time.Time `json:"expires_at"`
		}{credentialID, token, expires})
	default:
		if err := s.RevokeCredential(ctx, *credential, now); err != nil {
			return err
		}
		return json.NewEncoder(output).Encode(map[string]string{"status": "revoked"})
	}
}
