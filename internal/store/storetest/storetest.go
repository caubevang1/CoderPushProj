// Package storetest gives integration tests a clean MySQL-backed store.
package storetest

import (
	"context"
	"os"
	"testing"

	"CoderPush/internal/store"
)

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
	if err := s.DeleteAll(ctx); err != nil {
		t.Fatalf("empty test database: %v", err)
	}
	return s
}
