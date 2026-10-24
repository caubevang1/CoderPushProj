package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// RunStatus is the state of an import run.
type RunStatus string

const (
	RunRunning               RunStatus = "running"
	RunCompleted             RunStatus = "completed"
	RunCompletedWithProblems RunStatus = "completed_with_problems"
	RunFailed                RunStatus = "failed"
	RunInterrupted           RunStatus = "interrupted"
)

// Finished reports whether the run reached the end of the supplier feed.
// Every other status means the run is still open and can be resumed.
func (s RunStatus) Finished() bool {
	return s == RunCompleted || s == RunCompletedWithProblems
}

var (
	ErrRunAlreadyOpen = errors.New("an import run is already open")
	ErrNoOpenRun      = errors.New("no open import run")
	ErrRunNotFound    = errors.New("import run not found")
)

// Run is one import run, from start until the supplier says has_more = false.
type Run struct {
	ID               int64
	Status           RunStatus
	NextCursor       string // checkpoint: where the next fetch starts
	Open             bool   // !Status.Finished()
	BatchesApplied   int
	SkippedStale     int
	SkippedDuplicate int
	LastError        string
	StartedAt        time.Time
	UpdatedAt        time.Time
	FinishedAt       *time.Time
}

const runColumns = `id, status, next_cursor, batches_applied, skipped_stale,
	skipped_duplicate, last_error, started_at, updated_at, finished_at`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanRun(row rowScanner) (Run, error) {
	var (
		r          Run
		lastError  sql.NullString
		finishedAt sql.NullTime
	)
	err := row.Scan(&r.ID, &r.Status, &r.NextCursor, &r.BatchesApplied,
		&r.SkippedStale, &r.SkippedDuplicate, &lastError, &r.StartedAt, &r.UpdatedAt, &finishedAt)
	if err != nil {
		return Run{}, err
	}
	r.Open = !r.Status.Finished()
	r.LastError = lastError.String
	if finishedAt.Valid {
		r.FinishedAt = &finishedAt.Time
	}
	return r, nil
}

// CreateRun opens a new run. Callers must hold the import lock and check
// OpenRun first, so that at most one run is open.
func (s *Store) CreateRun(ctx context.Context) (Run, error) {
	t := now()
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO import_runs (status, next_cursor, started_at, updated_at)
		VALUES (?, '', ?, ?)`, RunRunning, t, t)
	if err != nil {
		return Run{}, wrap("create run", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Run{}, fmt.Errorf("create run: %w", err)
	}
	return s.GetRun(ctx, id)
}

// OpenRun returns the run that is still open, or ErrNoOpenRun.
func (s *Store) OpenRun(ctx context.Context) (Run, error) {
	r, err := scanRun(s.db.QueryRowContext(ctx,
		`SELECT `+runColumns+` FROM import_runs WHERE status NOT IN (?, ?) ORDER BY id DESC LIMIT 1`,
		RunCompleted, RunCompletedWithProblems))
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, ErrNoOpenRun
	}
	return r, wrap("get open run", err)
}

// GetRun returns one run by id.
func (s *Store) GetRun(ctx context.Context, id int64) (Run, error) {
	r, err := scanRun(s.db.QueryRowContext(ctx,
		`SELECT `+runColumns+` FROM import_runs WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, ErrRunNotFound
	}
	return r, wrap("get run", err)
}

// LatestRun returns the most recently started run.
func (s *Store) LatestRun(ctx context.Context) (Run, error) {
	r, err := scanRun(s.db.QueryRowContext(ctx,
		`SELECT `+runColumns+` FROM import_runs ORDER BY id DESC LIMIT 1`))
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, ErrRunNotFound
	}
	return r, wrap("get latest run", err)
}

// ListRuns returns the most recent runs first.
func (s *Store) ListRuns(ctx context.Context, limit int) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+runColumns+` FROM import_runs ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, wrap("list runs", err)
	}
	defer rows.Close()

	var runs []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("list runs: %w", err)
		}
		runs = append(runs, r)
	}
	return runs, wrap("list runs", rows.Err())
}

// SetStatus moves an open run to running, failed or interrupted.
// lastError is stored as NULL when empty. Finishing a run is done by
// ApplyBatch, in the same transaction as the last batch.
func (s *Store) SetStatus(ctx context.Context, id int64, status RunStatus, lastError string) (err error) {
	if status.Finished() {
		return fmt.Errorf("set status: %s must be set by ApplyBatch", status)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return wrap("set status", err)
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	// Checked with a locking read: RowsAffected would count only rows whose
	// values changed, not rows that matched.
	var current RunStatus
	err = tx.QueryRowContext(ctx, `SELECT status FROM import_runs WHERE id = ? FOR UPDATE`, id).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("set status of run %d: %w", id, ErrRunNotFound)
	}
	if err != nil {
		return wrap("set status", err)
	}
	if current.Finished() {
		return fmt.Errorf("set status of run %d: run is already finished", id)
	}

	if _, err = tx.ExecContext(ctx, `
		UPDATE import_runs SET status = ?, last_error = NULLIF(?, ''), updated_at = ?
		WHERE id = ?`, status, lastError, now(), id); err != nil {
		return wrap("set status", err)
	}
	return wrap("set status: commit", tx.Commit())
}

// MarkStaleRunsInterrupted turns every "running" run into "interrupted".
// Call it only while holding the import lock: then no live process owns
// those runs, so they belong to a process that died.
func (s *Store) MarkStaleRunsInterrupted(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE import_runs
		SET status = ?, last_error = 'the import process stopped unexpectedly', updated_at = ?
		WHERE status = ?`, RunInterrupted, now(), RunRunning)
	if err != nil {
		return 0, wrap("mark stale runs", err)
	}
	return res.RowsAffected()
}
