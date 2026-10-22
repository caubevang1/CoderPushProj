// Package store keeps the catalog, its import runs and their history in MySQL.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

//go:embed schema.sql
var schemaSQL string

// MySQL error numbers used by the store.
const (
	errDuplicateEntry = 1062
	errNoSuchTable    = 1146
)

// ErrNotMigrated is returned when the tables do not exist yet.
var ErrNotMigrated = errors.New("database tables are missing, run `catalog migrate` first")

// Store is the MySQL-backed catalog.
type Store struct {
	db *sql.DB
}

// Open connects to MySQL, retrying the first ping for up to 30 seconds so
// the app can start while the database container is still booting.
func Open(ctx context.Context, dsn string) (*Store, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		err = db.PingContext(ctx)
		if err == nil {
			return &Store{db: db}, nil
		}
		select {
		case <-ctx.Done():
			db.Close()
			return nil, fmt.Errorf("connect to database: %w", err)
		case <-time.After(time.Second):
		}
	}
}

// Close closes the connection pool.
func (s *Store) Close() error {
	return s.db.Close()
}

// DB exposes the pool for tests and tooling.
func (s *Store) DB() *sql.DB {
	return s.db
}

// Migrate creates any missing table. It is safe to run repeatedly.
func (s *Store) Migrate(ctx context.Context) error {
	for _, stmt := range strings.Split(schemaSQL, ";") {
		if isBlankSQL(stmt) {
			continue
		}
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	return nil
}

// isBlankSQL reports whether stmt holds only whitespace and -- comments.
func isBlankSQL(stmt string) bool {
	for _, line := range strings.Split(stmt, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "--") {
			return false
		}
	}
	return true
}

func now() time.Time {
	return time.Now().UTC()
}

func isMySQLError(err error, number uint16) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == number
}

// wrap turns a missing-table error into ErrNotMigrated so callers can give
// staff a useful hint.
func wrap(op string, err error) error {
	if err == nil {
		return nil
	}
	if isMySQLError(err, errNoSuchTable) {
		return fmt.Errorf("%s: %w", op, ErrNotMigrated)
	}
	return fmt.Errorf("%s: %w", op, err)
}
