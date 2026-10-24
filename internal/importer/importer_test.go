package importer

import (
	"context"
	"errors"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"CoderPush/internal/catalog"
	"CoderPush/internal/store"
	"CoderPush/internal/store/storetest"
	"CoderPush/internal/supplier"
)

var ctx = context.Background()

func loadFeed(t *testing.T, name string) []supplier.FeedBatch {
	t.Helper()
	feed, err := supplier.LoadFeed("../../testdata/feeds/" + name)
	if err != nil {
		t.Fatalf("LoadFeed: %v", err)
	}
	return feed
}

// serve starts a fake supplier for feed and returns a fast-retrying client.
func serve(t *testing.T, feed []supplier.FeedBatch, opts supplier.FakeOptions) *supplier.Client {
	t.Helper()
	srv := httptest.NewServer(supplier.NewFake(feed, opts))
	t.Cleanup(srv.Close)
	c := supplier.NewClient(srv.URL)
	c.BaseDelay = time.Millisecond
	return c
}

func newImporter(s *store.Store, f Fetcher) *Importer {
	return &Importer{Store: s, Supplier: f}
}

// snapshot is the whole catalog without bookkeeping columns.
func snapshot(t *testing.T, s *store.Store) []catalog.Product {
	t.Helper()
	rows, err := s.ListProducts(ctx, "", "")
	if err != nil {
		t.Fatalf("ListProducts: %v", err)
	}
	out := make([]catalog.Product, len(rows))
	for i, r := range rows {
		out[i] = r.Product
	}
	return out
}

func mustStart(t *testing.T, im *Importer) store.Run {
	t.Helper()
	run, err := im.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return run
}

// expectedAfterChanges is the catalog after a clean import of "initial"
// followed by "changes", computed on a fresh database.
func expectedAfterChanges(t *testing.T) []catalog.Product {
	t.Helper()
	s := storetest.New(t)
	mustStart(t, newImporter(s, serve(t, loadFeed(t, "initial"), supplier.FakeOptions{})))
	mustStart(t, newImporter(s, serve(t, loadFeed(t, "changes"), supplier.FakeOptions{})))
	return snapshot(t, s)
}

// T7
func TestSupplierFailureThenResume(t *testing.T) {
	want := expectedAfterChanges(t)

	s := storetest.New(t)
	mustStart(t, newImporter(s, serve(t, loadFeed(t, "initial"), supplier.FakeOptions{})))

	changes := loadFeed(t, "changes")
	run, err := newImporter(s, serve(t, changes, supplier.FakeOptions{FailAt: 3})).Start(ctx)
	if err == nil {
		t.Fatal("Start succeeded although batch 3 always fails")
	}
	if run.Status != store.RunFailed || !run.Open || run.BatchesApplied != 2 || run.LastError == "" {
		t.Fatalf("run = %+v, want failed, open, 2 batches, with last error", run)
	}
	if run.Status.Finished() {
		t.Fatal("a failed run must not count as finished")
	}

	run, err = newImporter(s, serve(t, changes, supplier.FakeOptions{})).Resume(ctx)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if !run.Status.Finished() || run.Open || run.BatchesApplied != 3 {
		t.Fatalf("run = %+v, want finished with 3 batches", run)
	}
	if got := snapshot(t, s); !reflect.DeepEqual(got, want) {
		t.Fatalf("catalog after resume differs from a clean import\n got: %+v\nwant: %+v", got, want)
	}
}

// T8
func TestCancelledContextThenResume(t *testing.T) {
	want := expectedAfterChanges(t)

	s := storetest.New(t)
	mustStart(t, newImporter(s, serve(t, loadFeed(t, "initial"), supplier.FakeOptions{})))

	changes := loadFeed(t, "changes")
	cctx, cancel := context.WithCancel(ctx)
	im := newImporter(s, serve(t, changes, supplier.FakeOptions{}))
	im.AfterBatch = func(n int) {
		if n == 1 {
			cancel() // like Ctrl+C right after the first batch
		}
	}
	run, err := im.Start(cctx)
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("Start error = %v, want ErrInterrupted", err)
	}
	if run.Status != store.RunInterrupted || run.BatchesApplied != 1 || !run.Open {
		t.Fatalf("run = %+v, want interrupted, open, 1 batch", run)
	}

	run, err = newImporter(s, serve(t, changes, supplier.FakeOptions{})).Resume(ctx)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if !run.Status.Finished() || run.BatchesApplied != 3 {
		t.Fatalf("run = %+v, want finished with 3 batches", run)
	}
	if got := snapshot(t, s); !reflect.DeepEqual(got, want) {
		t.Fatalf("catalog after resume differs from a clean import\n got: %+v\nwant: %+v", got, want)
	}
}

// A process that dies leaves its run "running". The next Start or Resume
// takes the lock, sees the run is abandoned and marks it interrupted.
func TestAbandonedRunIsDetectedAndResumed(t *testing.T) {
	s := storetest.New(t)
	feed := loadFeed(t, "initial")

	// Simulate a crash after batch 1: apply it directly and never finish.
	run, err := s.CreateRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyBatch(ctx, run.ID, store.BatchInput{ID: feed[0].BatchID, Items: feed[0].Items, NextCursor: "2"}); err != nil {
		t.Fatal(err)
	}
	if free, _ := s.IsImportLockFree(ctx); !free {
		t.Fatal("lock should be free: no process holds it")
	}

	im := newImporter(s, serve(t, feed, supplier.FakeOptions{}))
	if _, err := im.Start(ctx); !errors.Is(err, store.ErrRunAlreadyOpen) {
		t.Fatalf("Start error = %v, want ErrRunAlreadyOpen", err)
	}
	got, _ := s.GetRun(ctx, run.ID)
	if got.Status != store.RunInterrupted {
		t.Fatalf("status = %s, want interrupted", got.Status)
	}

	got, err = im.Resume(ctx)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if got.Status != store.RunCompleted || got.BatchesApplied != 3 {
		t.Fatalf("run = %+v, want completed with 3 batches", got)
	}
	if n := len(snapshot(t, s)); n != 10 {
		t.Fatalf("catalog has %d products, want 10", n)
	}
}

// T9
func TestSecondRunRefusedWhileOneIsOpen(t *testing.T) {
	s := storetest.New(t)
	feed := loadFeed(t, "initial")

	if _, err := newImporter(s, serve(t, feed, supplier.FakeOptions{FailAt: 2})).Start(ctx); err == nil {
		t.Fatal("first Start should fail at batch 2")
	}
	_, err := newImporter(s, serve(t, feed, supplier.FakeOptions{})).Start(ctx)
	if !errors.Is(err, store.ErrRunAlreadyOpen) {
		t.Fatalf("second Start error = %v, want ErrRunAlreadyOpen", err)
	}
}

func TestSecondProcessRefusedWhileImporting(t *testing.T) {
	s := storetest.New(t)
	release, err := s.AcquireImportLock(ctx) // another process is importing
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	im := newImporter(s, serve(t, loadFeed(t, "initial"), supplier.FakeOptions{}))
	if _, err := im.Start(ctx); !errors.Is(err, store.ErrImportLocked) {
		t.Fatalf("Start error = %v, want ErrImportLocked", err)
	}
	if _, err := im.Resume(ctx); !errors.Is(err, store.ErrImportLocked) {
		t.Fatalf("Resume error = %v, want ErrImportLocked", err)
	}
}

func TestResumeWithoutOpenRun(t *testing.T) {
	s := storetest.New(t)
	im := newImporter(s, serve(t, loadFeed(t, "initial"), supplier.FakeOptions{}))
	if _, err := im.Resume(ctx); !errors.Is(err, ErrNothingToResume) {
		t.Fatalf("Resume error = %v, want ErrNothingToResume", err)
	}
}
