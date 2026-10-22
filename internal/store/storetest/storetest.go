// Package storetest gives integration tests a clean MySQL-backed store.
package storetest

import (
	"context"
	"os"
	"testing"

	"CoderPush/internal/store"
)

// tables lists every table, children before parents.
var tables = []string{"changes", "problems", "applied_batches", "products", "import_runs"}

// New returns a migrated store on TEST_MYSQL_DSN with every table emptied.
// It skips the test when TEST_MYSQL_DSN is not set. Tests sharing the
// database must not run in parallel (go test -p 1).
func New(t testing.TB) *store.Store {
	t.Helper()
	dsn := os.Getenv("TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("TEST_MYSQL_DSN not set, skipping MySQL integration test")
	}

	ctx := context.Background()
	s, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}

	// DELETE in child-to-parent order: much faster than TRUNCATE (DDL) on
	// tiny tables, and foreign keys stay enforced.
	for _, table := range tables {
		if _, err := s.DB().ExecContext(ctx, "DELETE FROM "+table); err != nil {
			t.Fatalf("empty %s: %v", table, err)
		}
	}
	return s
}
