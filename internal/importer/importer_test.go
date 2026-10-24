package importer

import (
	"context"
	"errors"
	"net/http/httptest"
	"reflect"
	"slices"
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

// recorder remembers which cursors were fetched.
type recorder struct {
	Fetcher
	cursors []string
}

func (r *recorder) FetchBatch(ctx context.Context, cursor string) (supplier.Batch, error) {
	r.cursors = append(r.cursors, cursor)
	return r.Fetcher.FetchBatch(ctx, cursor)
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
	if run.NextCursor != "3" {
		t.Fatalf("checkpoint = %q, want 3 (the batch that failed)", run.NextCursor)
	}
	if run.Status.Finished() {
		t.Fatal("a failed run must not count as finished")
	}

	rec := &recorder{Fetcher: serve(t, changes, supplier.FakeOptions{})}
	run, err = newImporter(s, rec).Resume(ctx)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if !run.Status.Finished() || run.Open || run.BatchesApplied != 3 {
		t.Fatalf("run = %+v, want finished with 3 batches", run)
	}
	if !slices.Equal(rec.cursors, []string{"3"}) {
		t.Fatalf("resume fetched cursors %q, want only [3]", rec.cursors)
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
	if run.Status != store.RunInterrupted || run.BatchesApplied != 1 || !run.Open || run.NextCursor != "2" {
		t.Fatalf("run = %+v, want interrupted, open, 1 batch, checkpoint 2", run)
	}

	rec := &recorder{Fetcher: serve(t, changes, supplier.FakeOptions{})}
	run, err = newImporter(s, rec).Resume(ctx)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if !slices.Equal(rec.cursors, []string{"2", "3"}) {
		t.Fatalf("resume fetched cursors %q, want [2 3]", rec.cursors)
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

// T1
func TestFirstImportCreatesEverything(t *testing.T) {
	s := storetest.New(t)
	run := mustStart(t, newImporter(s, serve(t, loadFeed(t, "initial"), supplier.FakeOptions{})))

	if run.Status != store.RunCompleted || run.Open || run.FinishedAt == nil || run.BatchesApplied != 3 {
		t.Fatalf("run = %+v, want completed and closed after 3 batches", run)
	}
	if n := len(snapshot(t, s)); n != 10 {
		t.Fatalf("catalog has %d products, want 10", n)
	}
	changes, err := s.ListChanges(ctx, run.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 10 {
		t.Fatalf("got %d changes, want 10", len(changes))
	}
	for _, c := range changes {
		if c.Kind != store.ChangeCreated {
			t.Fatalf("change %s is %s, want created", c.SKU, c.Kind)
		}
	}
}

// T2
func TestImportingSameFeedTwiceChangesNothing(t *testing.T) {
	s := storetest.New(t)
	feed := loadFeed(t, "initial")
	mustStart(t, newImporter(s, serve(t, feed, supplier.FakeOptions{})))
	before := snapshot(t, s)

	run := mustStart(t, newImporter(s, serve(t, feed, supplier.FakeOptions{})))
	if run.Status != store.RunCompleted || run.SkippedDuplicate != 10 {
		t.Fatalf("run = %+v, want completed with 10 duplicates", run)
	}
	if changes, _ := s.ListChanges(ctx, run.ID, ""); len(changes) != 0 {
		t.Fatalf("second import recorded %d changes, want 0", len(changes))
	}
	if after := snapshot(t, s); !reflect.DeepEqual(after, before) {
		t.Fatalf("catalog changed on re-import\nbefore: %+v\n after: %+v", before, after)
	}
}

// T3
func TestChangesInReverseBatchOrderGiveSameCatalog(t *testing.T) {
	want := expectedAfterChanges(t)

	s := storetest.New(t)
	mustStart(t, newImporter(s, serve(t, loadFeed(t, "initial"), supplier.FakeOptions{})))
	feed := loadFeed(t, "changes")
	slices.Reverse(feed)
	mustStart(t, newImporter(s, serve(t, feed, supplier.FakeOptions{})))

	if got := snapshot(t, s); !reflect.DeepEqual(got, want) {
		t.Fatalf("reverse order gave a different catalog\n got: %+v\nwant: %+v", got, want)
	}
}

// The changes feed holds every tricky case of PLAN section 5.3; check
// each one landed where the decision table says.
func TestChangesFeedOutcome(t *testing.T) {
	got := expectedAfterChanges(t)
	bySKU := make(map[string]catalog.Product)
	for _, p := range got {
		bySKU[p.SKU] = p
	}

	tests := []struct {
		sku     string
		status  catalog.Status
		version int64
		price   int64
		why     string
	}{
		{"TEA-001", catalog.StatusActive, 2, 42000, "price change; repeated v2 ignored"},
		{"TEA-004", catalog.StatusActive, 3, 65000, "older v2 in the same batch ignored"},
		{"CF-001", catalog.StatusActive, 3, 99000, "older v2 in a later batch ignored"},
		{"CF-002", catalog.StatusActive, 2, 150000, "newer version, same content"},
		{"CF-003", catalog.StatusWithdrawn, 2, 110000, "withdrawn; older record did not revive it"},
		{"CF-005", catalog.StatusWithdrawn, 3, 0, "withdraw before create; late create ignored; tombstone bumped"},
		{"ACC-001", catalog.StatusActive, 3, 28000, "withdrawn then reactivated by a newer version"},
		{"TEA-005", catalog.StatusActive, 1, 95000, "new product"},
		{"ACC-004", catalog.StatusActive, 1, 35000, "new product in the last batch"},
	}
	for _, tc := range tests {
		p, ok := bySKU[tc.sku]
		if !ok {
			t.Errorf("%s missing (%s)", tc.sku, tc.why)
			continue
		}
		if p.Status != tc.status || p.Version != tc.version || p.Price != tc.price {
			t.Errorf("%s = %s v%d price %d, want %s v%d price %d (%s)",
				tc.sku, p.Status, p.Version, p.Price, tc.status, tc.version, tc.price, tc.why)
		}
	}
	for _, sku := range []string{"TEA-006", "CF-006", "ACC-003"} {
		if _, ok := bySKU[sku]; ok {
			t.Errorf("invalid record %s reached the catalog", sku)
		}
	}
	if len(got) != 13 {
		t.Errorf("catalog has %d products, want 13", len(got))
	}
}

// T11
func TestInvalidRecordsDoNotBlockTheBatch(t *testing.T) {
	s := storetest.New(t)
	feed := []supplier.FeedBatch{{
		BatchID: "mixed-001",
		Items: []catalog.Item{
			{SKU: "OK-1", Name: "Fine", Price: 100, Currency: "VND", Version: 1},
			{SKU: "BAD-1", Name: "", Price: 100, Currency: "VND", Version: 1},
			{SKU: "BAD-2", Name: "Negative", Price: -1, Currency: "VND", Version: 1},
			{SKU: "BAD-3", Name: "No version", Price: 1, Currency: "VND"},
			{SKU: "OK-2", Name: "Also fine", Price: 200, Currency: "VND", Version: 1},
		},
	}}

	run := mustStart(t, newImporter(s, serve(t, feed, supplier.FakeOptions{})))
	if run.Status != store.RunCompletedWithProblems {
		t.Fatalf("status = %s, want completed_with_problems", run.Status)
	}
	products := snapshot(t, s)
	if len(products) != 2 || products[0].SKU != "OK-1" || products[1].SKU != "OK-2" {
		t.Fatalf("catalog = %+v, want only OK-1 and OK-2", products)
	}
	problems, err := s.ListProblems(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 3 {
		t.Fatalf("got %d problems, want 3", len(problems))
	}
	for i, sku := range []string{"BAD-1", "BAD-2", "BAD-3"} {
		if problems[i].SKU != sku || problems[i].RunID != run.ID || problems[i].BatchID != "mixed-001" {
			t.Fatalf("problem %d = %+v, want %s from run %d", i, problems[i], sku, run.ID)
		}
	}
}

// T12
func TestSupplierDownAtFirstBatchKeepsCatalog(t *testing.T) {
	s := storetest.New(t)
	mustStart(t, newImporter(s, serve(t, loadFeed(t, "initial"), supplier.FakeOptions{})))
	before := snapshot(t, s)

	run, err := newImporter(s, serve(t, loadFeed(t, "changes"), supplier.FakeOptions{FailAt: 1})).Start(ctx)
	if err == nil {
		t.Fatal("Start succeeded although the supplier is down")
	}
	if run.Status != store.RunFailed || run.BatchesApplied != 0 || run.NextCursor != "" {
		t.Fatalf("run = %+v, want failed with nothing applied", run)
	}
	if after := snapshot(t, s); !reflect.DeepEqual(after, before) {
		t.Fatalf("catalog changed although nothing was applied\nbefore: %+v\n after: %+v", before, after)
	}
}

// A supplier that times out and recovers on its own within the retries
// must not even fail the run.
func TestTransientSupplierErrorsAreRetried(t *testing.T) {
	s := storetest.New(t)
	run := mustStart(t, newImporter(s, serve(t, loadFeed(t, "initial"), supplier.FakeOptions{FailAt: 2, FailTimes: 3})))
	if run.Status != store.RunCompleted || run.BatchesApplied != 3 {
		t.Fatalf("run = %+v, want completed after retries", run)
	}
}
