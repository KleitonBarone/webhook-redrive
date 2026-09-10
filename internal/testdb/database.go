// Package testdb gives integration tests isolated PostgreSQL schemas.
package testdb

import (
	"context"
	"net/url"
	"os"
	"testing"

	"github.com/KleitonBarone/webhook-redrive/internal/id"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func URL(t *testing.T) string {
	t.Helper()
	raw := os.Getenv("TEST_DATABASE_URL")
	if raw == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("CI requires TEST_DATABASE_URL")
		}
		t.Skip("TEST_DATABASE_URL is not set")
	}
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") {
		t.Fatal("TEST_DATABASE_URL must be a PostgreSQL URL")
	}
	pool, err := pgxpool.New(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	unique, err := id.New()
	if err != nil {
		pool.Close()
		t.Fatal(err)
	}
	schema := "test_" + unique
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := pool.Exec(context.Background(), "CREATE SCHEMA "+quoted); err != nil {
		pool.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		defer pool.Close()
		if _, err := pool.Exec(context.Background(), "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Errorf("clean test schema: %v", err)
		}
	})
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	return parsed.String()
}
