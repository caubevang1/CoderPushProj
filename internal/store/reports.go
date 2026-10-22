package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"CoderPush/internal/catalog"
)

// Change is one recorded change to the catalog.
type Change struct {
	ID         int64
	RunID      int64
	BatchID    string
	SKU        string
	Kind       ChangeKind
	OldVersion *int64
	NewVersion int64
	Old        *catalog.Product // nil for a product seen for the first time
	New        *catalog.Product
	CreatedAt  time.Time
}

// ListChanges returns the changes of a run in the order they happened.
// An empty kind returns every kind.
func (s *Store) ListChanges(ctx context.Context, runID int64, kind ChangeKind) ([]Change, error) {
	query := `SELECT id, run_id, batch_id, sku, kind, old_version, new_version, old_value, new_value, created_at
		FROM changes WHERE run_id = ?`
	args := []any{runID}
	if kind != "" {
		query += ` AND kind = ?`
		args = append(args, kind)
	}
	query += ` ORDER BY id`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, wrap("list changes", err)
	}
	defer rows.Close()

	var changes []Change
	for rows.Next() {
		var (
			c                  Change
			oldVersion         sql.NullInt64
			oldValue, newValue []byte
		)
		if err := rows.Scan(&c.ID, &c.RunID, &c.BatchID, &c.SKU, &c.Kind, &oldVersion, &c.NewVersion,
			&oldValue, &newValue, &c.CreatedAt); err != nil {
			return nil, fmt.Errorf("list changes: %w", err)
		}
		if oldVersion.Valid {
			c.OldVersion = &oldVersion.Int64
		}
		if c.Old, err = decodeProduct(oldValue); err != nil {
			return nil, fmt.Errorf("list changes: %w", err)
		}
		if c.New, err = decodeProduct(newValue); err != nil {
			return nil, fmt.Errorf("list changes: %w", err)
		}
		changes = append(changes, c)
	}
	return changes, wrap("list changes", rows.Err())
}

func decodeProduct(b []byte) (*catalog.Product, error) {
	if b == nil {
		return nil, nil
	}
	var p catalog.Product
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// Problem is a supplier record that could not be applied.
type Problem struct {
	ID         int64
	RunID      int64
	BatchID    string
	SKU        string
	Reason     string
	RawItem    string
	CreatedAt  time.Time
	ResolvedAt *time.Time
}

var ErrProblemNotFound = errors.New("problem not found")

// ListProblems returns unresolved problems, or all of them when
// includeResolved is set, oldest first.
func (s *Store) ListProblems(ctx context.Context, includeResolved bool) ([]Problem, error) {
	query := `SELECT id, run_id, batch_id, sku, reason, raw_item, created_at, resolved_at FROM problems`
	if !includeResolved {
		query += ` WHERE resolved_at IS NULL`
	}
	query += ` ORDER BY id`

	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, wrap("list problems", err)
	}
	defer rows.Close()

	var problems []Problem
	for rows.Next() {
		var (
			p          Problem
			raw        sql.NullString
			resolvedAt sql.NullTime
		)
		if err := rows.Scan(&p.ID, &p.RunID, &p.BatchID, &p.SKU, &p.Reason, &raw, &p.CreatedAt, &resolvedAt); err != nil {
			return nil, fmt.Errorf("list problems: %w", err)
		}
		p.RawItem = raw.String
		if resolvedAt.Valid {
			p.ResolvedAt = &resolvedAt.Time
		}
		problems = append(problems, p)
	}
	return problems, wrap("list problems", rows.Err())
}

// ResolveProblem marks a problem as handled. Resolving it twice is a no-op.
func (s *Store) ResolveProblem(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE problems SET resolved_at = ? WHERE id = ? AND resolved_at IS NULL`, now(), id)
	if err != nil {
		return wrap("resolve problem", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	var exists bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM problems WHERE id = ?)`, id).Scan(&exists); err != nil {
		return wrap("resolve problem", err)
	}
	if !exists {
		return fmt.Errorf("problem %d: %w", id, ErrProblemNotFound)
	}
	return nil
}

// StoredProduct is a catalog product with its bookkeeping columns.
type StoredProduct struct {
	catalog.Product
	LastRunID int64
	UpdatedAt time.Time
}

// ListProducts returns the catalog ordered by SKU. Empty filters match all.
func (s *Store) ListProducts(ctx context.Context, status catalog.Status, sku string) ([]StoredProduct, error) {
	var (
		where []string
		args  []any
	)
	if status != "" {
		where = append(where, "status = ?")
		args = append(args, status)
	}
	if sku != "" {
		where = append(where, "sku = ?")
		args = append(args, sku)
	}
	query := `SELECT sku, name, price, currency, status, version, last_run_id, updated_at FROM products`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY sku"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, wrap("list products", err)
	}
	defer rows.Close()

	var products []StoredProduct
	for rows.Next() {
		var (
			p         StoredProduct
			lastRunID sql.NullInt64
		)
		if err := rows.Scan(&p.SKU, &p.Name, &p.Price, &p.Currency, &p.Status, &p.Version, &lastRunID, &p.UpdatedAt); err != nil {
			return nil, fmt.Errorf("list products: %w", err)
		}
		p.LastRunID = lastRunID.Int64
		products = append(products, p)
	}
	return products, wrap("list products", rows.Err())
}

// Summary is what staff need to judge a run at a glance.
type Summary struct {
	Run          Run
	Changes      map[ChangeKind]int
	Problems     int
	OpenProblems int
}

// RunSummary counts the changes and problems of a run.
func (s *Store) RunSummary(ctx context.Context, runID int64) (Summary, error) {
	run, err := s.GetRun(ctx, runID)
	if err != nil {
		return Summary{}, err
	}
	sum := Summary{Run: run, Changes: make(map[ChangeKind]int)}

	rows, err := s.db.QueryContext(ctx, `SELECT kind, COUNT(*) FROM changes WHERE run_id = ? GROUP BY kind`, runID)
	if err != nil {
		return Summary{}, wrap("run summary", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			kind ChangeKind
			n    int
		)
		if err := rows.Scan(&kind, &n); err != nil {
			return Summary{}, fmt.Errorf("run summary: %w", err)
		}
		sum.Changes[kind] = n
	}
	if err := rows.Err(); err != nil {
		return Summary{}, wrap("run summary", err)
	}

	err = s.db.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(resolved_at IS NULL), 0) FROM problems WHERE run_id = ?`, runID).
		Scan(&sum.Problems, &sum.OpenProblems)
	return sum, wrap("run summary", err)
}
