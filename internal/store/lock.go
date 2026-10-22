package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

const importLockName = "catalog-import"

// ErrImportLocked means another process is importing right now.
var ErrImportLocked = errors.New("another import is in progress")

// AcquireImportLock takes the MySQL named lock that allows a single
// importer at a time. GET_LOCK belongs to a connection, so a dedicated
// connection is held until release is called. If the process dies, MySQL
// closes that connection and frees the lock on its own.
func (s *Store) AcquireImportLock(ctx context.Context) (release func(), err error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, wrap("acquire import lock", err)
	}

	var got sql.NullInt64
	if err := conn.QueryRowContext(ctx, `SELECT GET_LOCK(?, 0)`, importLockName).Scan(&got); err != nil {
		conn.Close()
		return nil, wrap("acquire import lock", err)
	}
	if !got.Valid || got.Int64 != 1 {
		conn.Close()
		return nil, ErrImportLocked
	}

	release = func() {
		// Use a fresh context: the caller's may already be cancelled (Ctrl+C).
		conn.ExecContext(context.Background(), `SELECT RELEASE_LOCK(?)`, importLockName)
		conn.Close()
	}
	return release, nil
}

// IsImportLockFree reports whether no process is importing. A run left in
// "running" while the lock is free belongs to a process that died.
func (s *Store) IsImportLockFree(ctx context.Context) (bool, error) {
	var free sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT IS_FREE_LOCK(?)`, importLockName).Scan(&free); err != nil {
		return false, wrap("check import lock", err)
	}
	if !free.Valid {
		return false, fmt.Errorf("check import lock: MySQL returned NULL")
	}
	return free.Int64 == 1, nil
}
