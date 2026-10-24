// Package importer runs catalog updates: it pages through the supplier
// feed and applies each batch, so that an update can stop at any point and
// be resumed later without losing or repeating work.
package importer

import (
	"context"
	"errors"
	"fmt"
	"io"

	"CoderPush/internal/store"
	"CoderPush/internal/supplier"
)

// Fetcher gets one supplier batch. *supplier.Client implements it.
type Fetcher interface {
	FetchBatch(ctx context.Context, cursor string) (supplier.Batch, error)
}

var (
	// ErrInterrupted means the context was cancelled (Ctrl+C) mid-run.
	ErrInterrupted = errors.New("import interrupted")
	// ErrNothingToResume means there is no open run.
	ErrNothingToResume = errors.New("there is no unfinished update to resume")
)

// Importer runs one update at a time.
type Importer struct {
	Store    *store.Store
	Supplier Fetcher
	Out      io.Writer // progress messages; nil discards them

	// AfterBatch, when set, is called right after each batch is committed,
	// with the number of batches committed by this call so far. The CLI
	// uses it for -crash-after-batch.
	AfterBatch func(committed int)
}

// Start opens a new run and imports until the supplier has no more
// batches. It refuses while another run is still open.
//
// The returned run reflects the final state, also when err is not nil:
// a failed or interrupted run stays open and can be resumed.
func (im *Importer) Start(ctx context.Context) (store.Run, error) {
	release, err := im.begin(ctx)
	if err != nil {
		return store.Run{}, err
	}
	defer release()

	if open, err := im.Store.OpenRun(ctx); err == nil {
		return open, fmt.Errorf("%w: run #%d is %s, resume it instead", store.ErrRunAlreadyOpen, open.ID, open.Status)
	} else if !errors.Is(err, store.ErrNoOpenRun) {
		return store.Run{}, err
	}

	run, err := im.Store.CreateRun(ctx)
	if err != nil {
		return store.Run{}, err
	}
	im.printf("Started run #%d\n", run.ID)
	return im.run(ctx, run)
}

// Resume continues the open run from its last checkpoint.
func (im *Importer) Resume(ctx context.Context) (store.Run, error) {
	release, err := im.begin(ctx)
	if err != nil {
		return store.Run{}, err
	}
	defer release()

	run, err := im.Store.OpenRun(ctx)
	if errors.Is(err, store.ErrNoOpenRun) {
		return store.Run{}, ErrNothingToResume
	}
	if err != nil {
		return store.Run{}, err
	}
	if err := im.Store.SetStatus(ctx, run.ID, store.RunRunning, ""); err != nil {
		return run, err
	}
	im.printf("Resuming run #%d (was %s) after %d applied batches\n", run.ID, run.Status, run.BatchesApplied)
	return im.run(ctx, run)
}

// begin takes the import lock. While we hold it no other process can be
// importing, so any run still marked "running" belongs to a process that
// died: it is marked interrupted.
func (im *Importer) begin(ctx context.Context) (release func(), err error) {
	release, err = im.Store.AcquireImportLock(ctx)
	if err != nil {
		return nil, err
	}
	n, err := im.Store.MarkStaleRunsInterrupted(ctx)
	if err != nil {
		release()
		return nil, err
	}
	if n > 0 {
		im.printf("Found %d run(s) left running by a stopped process, marked interrupted\n", n)
	}
	return release, nil
}

// run fetches and applies batches from the run's checkpoint until the
// supplier says has_more = false, the supplier gives up, or ctx ends.
func (im *Importer) run(ctx context.Context, run store.Run) (store.Run, error) {
	// Database writes must not be cut short by Ctrl+C: a batch in hand is
	// committed or rolled back as a whole, and the final status is saved.
	dbCtx := context.WithoutCancel(ctx)

	cursor := run.NextCursor
	committed := 0
	for {
		if ctx.Err() != nil {
			return im.stop(dbCtx, run.ID, store.RunInterrupted, "stopped by user", ErrInterrupted)
		}

		b, err := im.Supplier.FetchBatch(ctx, cursor)
		if err != nil {
			if ctx.Err() != nil {
				return im.stop(dbCtx, run.ID, store.RunInterrupted, "stopped by user", ErrInterrupted)
			}
			return im.stop(dbCtx, run.ID, store.RunFailed, err.Error(), err)
		}

		res, err := im.Store.ApplyBatch(dbCtx, run.ID, store.BatchInput{
			ID:         b.BatchID,
			Items:      b.Items,
			NextCursor: b.NextCursor,
			Last:       !b.HasMore,
		})
		switch {
		case errors.Is(err, store.ErrBatchAlreadyApplied):
			im.printf("  batch %s was already applied in this run, skipped\n", b.BatchID)
		case err != nil:
			// The transaction was rolled back: catalog and checkpoint are unchanged.
			return im.stop(dbCtx, run.ID, store.RunFailed, err.Error(), err)
		default:
			committed++
			im.printf("  batch %s: %s\n", b.BatchID, describe(res))
			if im.AfterBatch != nil {
				im.AfterBatch(committed)
			}
		}

		if res.RunStatus.Finished() {
			return im.Store.GetRun(dbCtx, run.ID)
		}
		cursor = b.NextCursor
	}
}

// stop records why the run stopped and returns its final state with cause.
// The run stays open so it can be resumed.
func (im *Importer) stop(ctx context.Context, runID int64, status store.RunStatus, reason string, cause error) (store.Run, error) {
	if err := im.Store.SetStatus(ctx, runID, status, reason); err != nil {
		return store.Run{ID: runID, Status: store.RunRunning, Open: true}, errors.Join(cause, err)
	}
	run, err := im.Store.GetRun(ctx, runID)
	if err != nil {
		return store.Run{ID: runID, Status: status, Open: true}, errors.Join(cause, err)
	}
	return run, cause
}

func describe(r store.BatchResult) string {
	return fmt.Sprintf("%d created, %d updated, %d withdrawn, %d reactivated, %d unchanged, %d stale, %d duplicate, %d problems",
		r.Created, r.Updated, r.Withdrawn, r.Reactivated, r.Unchanged, r.SkippedStale, r.SkippedDuplicate, r.Problems)
}

func (im *Importer) printf(format string, args ...any) {
	if im.Out != nil {
		fmt.Fprintf(im.Out, format, args...)
	}
}
