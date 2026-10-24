package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"CoderPush/internal/catalog"
)

// ChangeKind is the kind of a recorded catalog change.
type ChangeKind string

const (
	ChangeCreated     ChangeKind = "created"
	ChangeUpdated     ChangeKind = "updated"
	ChangeWithdrawn   ChangeKind = "withdrawn"
	ChangeReactivated ChangeKind = "reactivated"
)

// ErrBatchAlreadyApplied means this run already applied a batch with the
// same id. Nothing in the catalog changed; only the checkpoint moved on.
var ErrBatchAlreadyApplied = errors.New("batch already applied in this run")

// BatchInput is one supplier batch, as the store needs it.
type BatchInput struct {
	ID         string
	Items      []catalog.Item
	NextCursor string
	Last       bool // supplier answered has_more = false
}

// BatchResult counts what applying a batch did.
type BatchResult struct {
	Created          int
	Updated          int
	Withdrawn        int
	Reactivated      int
	Unchanged        int // newer version with identical content, or tombstone bump
	SkippedStale     int
	SkippedDuplicate int
	Problems         int
	RunStatus        RunStatus // status of the run after this batch
}

type changeRow struct {
	sku        string
	kind       ChangeKind
	oldVersion sql.NullInt64
	newVersion int64
	oldValue   []byte
	newValue   []byte
}

type problemRow struct {
	sku     string
	reason  string
	rawItem []byte
}

// ApplyBatch applies one batch in a single transaction: the products, the
// change and problem log, and the run's checkpoint (next_cursor) are
// committed together or not at all. A crash at any point before COMMIT
// leaves the catalog and the checkpoint exactly as they were, so resuming
// fetches this batch again.
//
// When b.Last is set the run is finished in the same transaction, so a
// run can never be completed without its last batch, nor the other way round.
//
// If the run already applied this batch id, ApplyBatch only moves the
// checkpoint and returns ErrBatchAlreadyApplied along with the run status.
func (s *Store) ApplyBatch(ctx context.Context, runID int64, b BatchInput) (res BatchResult, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return res, wrap("apply batch", err)
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	if err = lockOpenRun(ctx, tx, runID); err != nil {
		return res, err
	}

	t := now()
	_, err = tx.ExecContext(ctx, `
		INSERT INTO applied_batches (run_id, batch_id, item_count, applied_at)
		VALUES (?, ?, ?, ?)`, runID, b.ID, len(b.Items), t)
	if isMySQLError(err, errDuplicateEntry) {
		// The failed INSERT only rolls back itself; the transaction goes on.
		if res.RunStatus, err = advanceRun(ctx, tx, runID, b, res, false, t); err != nil {
			return res, err
		}
		if err = tx.Commit(); err != nil {
			return res, wrap("apply batch: commit", err)
		}
		return res, ErrBatchAlreadyApplied
	}
	if err != nil {
		return res, wrap("apply batch: record batch", err)
	}

	products, err := lockProducts(ctx, tx, b.Items)
	if err != nil {
		return res, err
	}

	var (
		changes  []changeRow
		problems []problemRow
		dirty    []string // SKUs to write, in first-touched order
		isDirty  = make(map[string]bool)
	)
	for _, it := range b.Items {
		if verr := catalog.Validate(it); verr != nil {
			problems = append(problems, newProblem(it, verr))
			continue
		}

		cur := products[it.SKU]
		action := catalog.Decide(cur, it)
		switch action {
		case catalog.ActionSkipStale:
			res.SkippedStale++
			continue
		case catalog.ActionSkipDuplicate:
			res.SkippedDuplicate++
			continue
		}

		next := catalog.Apply(cur, it, action)
		if kind, ok := changeKindOf(action, cur, next); ok {
			changes = append(changes, newChange(kind, cur, next))
			res.count(kind)
		} else {
			res.Unchanged++
		}

		if !isDirty[it.SKU] {
			isDirty[it.SKU] = true
			dirty = append(dirty, it.SKU)
		}
		// Later items of the same SKU in this batch compare against this state.
		products[it.SKU] = &next
	}
	res.Problems = len(problems)

	if err = upsertProducts(ctx, tx, runID, products, dirty, t); err != nil {
		return res, err
	}
	if err = insertChanges(ctx, tx, runID, b.ID, changes, t); err != nil {
		return res, err
	}
	if err = insertProblems(ctx, tx, runID, b.ID, problems, t); err != nil {
		return res, err
	}
	if res.RunStatus, err = advanceRun(ctx, tx, runID, b, res, true, t); err != nil {
		return res, err
	}
	if err = tx.Commit(); err != nil {
		return res, wrap("apply batch: commit", err)
	}
	return res, nil
}

func (r *BatchResult) count(kind ChangeKind) {
	switch kind {
	case ChangeCreated:
		r.Created++
	case ChangeUpdated:
		r.Updated++
	case ChangeWithdrawn:
		r.Withdrawn++
	case ChangeReactivated:
		r.Reactivated++
	}
}

// changeKindOf says which change, if any, an action records. A newer
// version with identical content and a tombstone bump change nothing a
// person would care about, so they are not logged.
func changeKindOf(a catalog.Action, cur *catalog.Product, next catalog.Product) (ChangeKind, bool) {
	switch a {
	case catalog.ActionCreate:
		return ChangeCreated, true
	case catalog.ActionCreateTombstone, catalog.ActionWithdraw:
		return ChangeWithdrawn, true
	case catalog.ActionReactivate:
		return ChangeReactivated, true
	case catalog.ActionUpdate:
		if cur.SameContent(next) {
			return "", false
		}
		return ChangeUpdated, true
	}
	return "", false
}

// lockOpenRun locks the run row and checks the run may still take batches.
func lockOpenRun(ctx context.Context, tx *sql.Tx, runID int64) error {
	var status RunStatus
	err := tx.QueryRowContext(ctx,
		`SELECT status FROM import_runs WHERE id = ? FOR UPDATE`, runID).
		Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("run %d: %w", runID, ErrRunNotFound)
	}
	if err != nil {
		return wrap("lock run", err)
	}
	if status != RunRunning {
		return fmt.Errorf("run %d is %s, not running", runID, status)
	}
	return nil
}

// lockProducts reads the current state of every SKU in the batch and locks
// those rows until the transaction ends.
func lockProducts(ctx context.Context, tx *sql.Tx, items []catalog.Item) (map[string]*catalog.Product, error) {
	products := make(map[string]*catalog.Product)

	var skus []any
	seen := make(map[string]bool)
	for _, it := range items {
		if it.SKU != "" && !seen[it.SKU] {
			seen[it.SKU] = true
			skus = append(skus, it.SKU)
		}
	}
	if len(skus) == 0 {
		return products, nil
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT sku, name, price, currency, status, version
		FROM products WHERE sku IN (`+placeholders(len(skus))+`) FOR UPDATE`, skus...)
	if err != nil {
		return nil, wrap("lock products", err)
	}
	defer rows.Close()

	for rows.Next() {
		var p catalog.Product
		if err := rows.Scan(&p.SKU, &p.Name, &p.Price, &p.Currency, &p.Status, &p.Version); err != nil {
			return nil, fmt.Errorf("lock products: %w", err)
		}
		products[p.SKU] = &p
	}
	return products, wrap("lock products", rows.Err())
}

func upsertProducts(ctx context.Context, tx *sql.Tx, runID int64, products map[string]*catalog.Product, skus []string, t time.Time) error {
	if len(skus) == 0 {
		return nil
	}
	// "AS new" replaces VALUES(), deprecated since MySQL 8.0.20.
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO products (sku, name, price, currency, status, version, last_run_id, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?) AS new
		ON DUPLICATE KEY UPDATE
			name = new.name, price = new.price, currency = new.currency,
			status = new.status, version = new.version,
			last_run_id = new.last_run_id, updated_at = new.updated_at`)
	if err != nil {
		return wrap("upsert products", err)
	}
	defer stmt.Close()

	for _, sku := range skus {
		p := products[sku]
		if _, err := stmt.ExecContext(ctx, p.SKU, p.Name, p.Price, p.Currency, p.Status, p.Version, runID, t); err != nil {
			return wrap("upsert product "+sku, err)
		}
	}
	return nil
}

func insertChanges(ctx context.Context, tx *sql.Tx, runID int64, batchID string, changes []changeRow, t time.Time) error {
	if len(changes) == 0 {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO changes (run_id, batch_id, sku, kind, old_version, new_version, old_value, new_value, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return wrap("insert changes", err)
	}
	defer stmt.Close()

	for _, c := range changes {
		if _, err := stmt.ExecContext(ctx, runID, batchID, c.sku, c.kind, c.oldVersion, c.newVersion,
			nullJSON(c.oldValue), nullJSON(c.newValue), t); err != nil {
			return wrap("insert change", err)
		}
	}
	return nil
}

func insertProblems(ctx context.Context, tx *sql.Tx, runID int64, batchID string, problems []problemRow, t time.Time) error {
	if len(problems) == 0 {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO problems (run_id, batch_id, sku, reason, raw_item, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return wrap("insert problems", err)
	}
	defer stmt.Close()

	for _, p := range problems {
		if _, err := stmt.ExecContext(ctx, runID, batchID, p.sku, p.reason, nullJSON(p.rawItem), t); err != nil {
			return wrap("insert problem", err)
		}
	}
	return nil
}

// advanceRun moves the checkpoint and adds the batch's counters. On the
// last batch it also finishes the run and frees the active slot.
func advanceRun(ctx context.Context, tx *sql.Tx, runID int64, b BatchInput, res BatchResult, applied bool, t time.Time) (RunStatus, error) {
	batches := 0
	if applied {
		batches = 1
	}
	_, err := tx.ExecContext(ctx, `
		UPDATE import_runs
		SET next_cursor = ?, batches_applied = batches_applied + ?,
			skipped_stale = skipped_stale + ?, skipped_duplicate = skipped_duplicate + ?,
			updated_at = ?
		WHERE id = ?`,
		b.NextCursor, batches, res.SkippedStale, res.SkippedDuplicate, t, runID)
	if err != nil {
		return "", wrap("advance run", err)
	}
	if !b.Last {
		return RunRunning, nil
	}

	status := RunCompleted
	var hasProblems bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM problems WHERE run_id = ?)`, runID).Scan(&hasProblems); err != nil {
		return "", wrap("finish run", err)
	}
	if hasProblems {
		status = RunCompletedWithProblems
	}
	_, err = tx.ExecContext(ctx, `
		UPDATE import_runs
		SET status = ?, last_error = NULL, finished_at = ?, updated_at = ?
		WHERE id = ?`, status, t, t, runID)
	if err != nil {
		return "", wrap("finish run", err)
	}
	return status, nil
}

func newChange(kind ChangeKind, cur *catalog.Product, next catalog.Product) changeRow {
	c := changeRow{sku: next.SKU, kind: kind, newVersion: next.Version}
	c.newValue, _ = json.Marshal(next)
	if cur != nil {
		c.oldVersion = sql.NullInt64{Int64: cur.Version, Valid: true}
		c.oldValue, _ = json.Marshal(cur)
	}
	return c
}

func newProblem(it catalog.Item, err error) problemRow {
	raw, _ := json.Marshal(it)
	return problemRow{
		// An invalid item may carry values that do not fit the columns.
		sku:     truncate(it.SKU, 64),
		reason:  truncate(err.Error(), 500),
		rawItem: raw,
	}
}

func nullJSON(b []byte) any {
	if b == nil {
		return nil
	}
	return string(b)
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}

func truncate(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max])
}
