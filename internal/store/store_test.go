package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"CoderPush/internal/catalog"
	"CoderPush/internal/store"
	"CoderPush/internal/store/storetest"
)

var ctx = context.Background()

func sale(sku string, version, price int64) catalog.Item {
	return catalog.Item{SKU: sku, Name: "Product " + sku, Price: price, Currency: "VND", Version: version}
}

func withdraw(sku string, version int64) catalog.Item {
	return catalog.Item{SKU: sku, Version: version, Withdrawn: true}
}

func batch(id, next string, items ...catalog.Item) store.BatchInput {
	return store.BatchInput{ID: id, Items: items, NextCursor: next}
}

func lastBatch(id string, items ...catalog.Item) store.BatchInput {
	return store.BatchInput{ID: id, Items: items, Last: true}
}

func newRun(t *testing.T, s *store.Store) store.Run {
	t.Helper()
	run, err := s.CreateRun(ctx)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	return run
}

func apply(t *testing.T, s *store.Store, runID int64, b store.BatchInput) store.BatchResult {
	t.Helper()
	res, err := s.ApplyBatch(ctx, runID, b)
	if err != nil {
		t.Fatalf("ApplyBatch %s: %v", b.ID, err)
	}
	return res
}

func product(t *testing.T, s *store.Store, sku string) store.StoredProduct {
	t.Helper()
	ps, err := s.ListProducts(ctx, "", sku)
	if err != nil {
		t.Fatalf("ListProducts: %v", err)
	}
	if len(ps) != 1 {
		t.Fatalf("want 1 product %s, got %d", sku, len(ps))
	}
	return ps[0]
}

func getRun(t *testing.T, s *store.Store, id int64) store.Run {
	t.Helper()
	run, err := s.GetRun(ctx, id)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	return run
}

func changes(t *testing.T, s *store.Store, runID int64) []store.Change {
	t.Helper()
	cs, err := s.ListChanges(ctx, runID, "")
	if err != nil {
		t.Fatalf("ListChanges: %v", err)
	}
	return cs
}

func TestMigrateIsRepeatable(t *testing.T) {
	s := storetest.New(t)
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
}

func TestApplyBatchCreatesProductsAndMovesCheckpoint(t *testing.T) {
	s := storetest.New(t)
	run := newRun(t, s)

	res := apply(t, s, run.ID, batch("b1", "2", sale("A", 1, 100), sale("B", 1, 200)))
	if res.Created != 2 || res.RunStatus != store.RunRunning {
		t.Fatalf("result = %+v, want 2 created and still running", res)
	}

	got := getRun(t, s, run.ID)
	if got.NextCursor != "2" || got.BatchesApplied != 1 || !got.Open {
		t.Fatalf("run = %+v, want cursor 2, 1 batch, open", got)
	}
	if p := product(t, s, "A"); p.Price != 100 || p.Status != catalog.StatusActive || p.LastRunID != run.ID {
		t.Fatalf("product A = %+v", p)
	}
	cs := changes(t, s, run.ID)
	if len(cs) != 2 || cs[0].Kind != store.ChangeCreated || cs[0].Old != nil || cs[0].New.Price != 100 {
		t.Fatalf("changes = %+v", cs)
	}
}

// T4
func TestStaleRecordAfterNewerIsSkipped(t *testing.T) {
	s := storetest.New(t)
	run := newRun(t, s)

	apply(t, s, run.ID, batch("b1", "2", sale("A", 3, 300)))
	res := apply(t, s, run.ID, batch("b2", "3", sale("A", 2, 200)))
	if res.SkippedStale != 1 {
		t.Fatalf("result = %+v, want 1 stale", res)
	}
	if p := product(t, s, "A"); p.Version != 3 || p.Price != 300 {
		t.Fatalf("product = %+v, want version 3 price 300", p)
	}
	if got := getRun(t, s, run.ID); got.SkippedStale != 1 {
		t.Fatalf("run.SkippedStale = %d, want 1", got.SkippedStale)
	}
}

func TestStaleRecordInSameBatchIsSkipped(t *testing.T) {
	s := storetest.New(t)
	run := newRun(t, s)

	res := apply(t, s, run.ID, batch("b1", "2", sale("A", 3, 300), sale("A", 2, 200)))
	if res.Created != 1 || res.SkippedStale != 1 {
		t.Fatalf("result = %+v, want 1 created, 1 stale", res)
	}
	if p := product(t, s, "A"); p.Version != 3 {
		t.Fatalf("product = %+v, want version 3", p)
	}
}

// T5
func TestWithdrawnProductIgnoresOlderAndReactivatesOnNewer(t *testing.T) {
	s := storetest.New(t)
	run := newRun(t, s)

	apply(t, s, run.ID, batch("b1", "2", sale("A", 1, 100)))
	apply(t, s, run.ID, batch("b2", "3", withdraw("A", 2)))

	res := apply(t, s, run.ID, batch("b3", "4", sale("A", 1, 100)))
	if res.SkippedStale != 1 {
		t.Fatalf("older record: result = %+v, want stale", res)
	}
	if p := product(t, s, "A"); p.Status != catalog.StatusWithdrawn || p.Version != 2 {
		t.Fatalf("after older record: product = %+v, want withdrawn v2", p)
	}

	res = apply(t, s, run.ID, batch("b4", "5", sale("A", 3, 150)))
	if res.Reactivated != 1 {
		t.Fatalf("newer record: result = %+v, want reactivated", res)
	}
	if p := product(t, s, "A"); p.Status != catalog.StatusActive || p.Version != 3 || p.Price != 150 {
		t.Fatalf("after newer record: product = %+v, want active v3 price 150", p)
	}

	cs := changes(t, s, run.ID)
	kinds := []store.ChangeKind{store.ChangeCreated, store.ChangeWithdrawn, store.ChangeReactivated}
	if len(cs) != len(kinds) {
		t.Fatalf("got %d changes, want %d", len(cs), len(kinds))
	}
	for i, k := range kinds {
		if cs[i].Kind != k {
			t.Fatalf("change %d kind = %s, want %s", i, cs[i].Kind, k)
		}
	}
}

// T6
func TestWithdrawBeforeCreateLeavesTombstone(t *testing.T) {
	s := storetest.New(t)
	run := newRun(t, s)

	res := apply(t, s, run.ID, batch("b1", "2", withdraw("A", 2)))
	if res.Withdrawn != 1 {
		t.Fatalf("result = %+v, want 1 withdrawn", res)
	}
	if p := product(t, s, "A"); p.Status != catalog.StatusWithdrawn || p.Version != 2 {
		t.Fatalf("product = %+v, want withdrawn tombstone v2", p)
	}

	res = apply(t, s, run.ID, batch("b2", "3", sale("A", 1, 100)))
	if res.SkippedStale != 1 {
		t.Fatalf("late create: result = %+v, want stale", res)
	}
	if p := product(t, s, "A"); p.Status != catalog.StatusWithdrawn {
		t.Fatalf("late create revived product: %+v", p)
	}
}

// T10
func TestSameBatchTwiceInOneRunAppliesOnce(t *testing.T) {
	s := storetest.New(t)
	run := newRun(t, s)

	b := batch("b1", "2", sale("A", 1, 100))
	apply(t, s, run.ID, b)

	// Pretend the run was rewound, as if the supplier repeated the batch.
	b.NextCursor = "2"
	res, err := s.ApplyBatch(ctx, run.ID, b)
	if !errors.Is(err, store.ErrBatchAlreadyApplied) {
		t.Fatalf("second ApplyBatch error = %v, want ErrBatchAlreadyApplied", err)
	}
	if res.RunStatus != store.RunRunning {
		t.Fatalf("status = %s, want running", res.RunStatus)
	}
	if cs := changes(t, s, run.ID); len(cs) != 1 {
		t.Fatalf("got %d changes, want 1", len(cs))
	}
	if got := getRun(t, s, run.ID); got.BatchesApplied != 1 || got.NextCursor != "2" {
		t.Fatalf("run = %+v, want 1 batch applied, cursor 2", got)
	}
}

func TestRepeatedLastBatchStillFinishesRun(t *testing.T) {
	s := storetest.New(t)
	run := newRun(t, s)

	apply(t, s, run.ID, batch("b1", "2", sale("A", 1, 100)))
	res, err := s.ApplyBatch(ctx, run.ID, lastBatch("b1", sale("A", 1, 100)))
	if !errors.Is(err, store.ErrBatchAlreadyApplied) || res.RunStatus != store.RunCompleted {
		t.Fatalf("got (%+v, %v), want completed with ErrBatchAlreadyApplied", res, err)
	}
}

func TestInvalidItemsBecomeProblemsAndRestIsApplied(t *testing.T) {
	s := storetest.New(t)
	run := newRun(t, s)

	bad := sale("B", 1, -5)
	res := apply(t, s, run.ID, lastBatch("b1", sale("A", 1, 100), bad, catalog.Item{SKU: "C"}))
	if res.Created != 1 || res.Problems != 2 {
		t.Fatalf("result = %+v, want 1 created, 2 problems", res)
	}
	if res.RunStatus != store.RunCompletedWithProblems {
		t.Fatalf("status = %s, want completed_with_problems", res.RunStatus)
	}

	ps, err := s.ListProblems(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 2 || ps[0].SKU != "B" || ps[0].BatchID != "b1" || ps[0].RawItem == "" {
		t.Fatalf("problems = %+v", ps)
	}

	if err := s.ResolveProblem(ctx, ps[0].ID); err != nil {
		t.Fatalf("ResolveProblem: %v", err)
	}
	if err := s.ResolveProblem(ctx, ps[0].ID); err != nil {
		t.Fatalf("ResolveProblem twice: %v", err)
	}
	if err := s.ResolveProblem(ctx, 999999); !errors.Is(err, store.ErrProblemNotFound) {
		t.Fatalf("ResolveProblem unknown id: %v", err)
	}
	if open, _ := s.ListProblems(ctx, false); len(open) != 1 {
		t.Fatalf("open problems = %d, want 1", len(open))
	}
	sum, err := s.RunSummary(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Problems != 2 || sum.OpenProblems != 1 || sum.Changes[store.ChangeCreated] != 1 {
		t.Fatalf("summary = %+v", sum)
	}
}

func TestOverlongSKUIsRecordedAsProblem(t *testing.T) {
	s := storetest.New(t)
	run := newRun(t, s)

	long := sale(strings.Repeat("X", 80), 1, 100)
	res := apply(t, s, run.ID, lastBatch("b1", long))
	if res.Problems != 1 {
		t.Fatalf("result = %+v, want 1 problem", res)
	}
}

func TestSameContentWithNewerVersionIsNotAChange(t *testing.T) {
	s := storetest.New(t)
	run := newRun(t, s)

	apply(t, s, run.ID, batch("b1", "2", sale("A", 1, 100)))
	res := apply(t, s, run.ID, batch("b2", "3", sale("A", 2, 100)))
	if res.Updated != 0 || res.Unchanged != 1 {
		t.Fatalf("result = %+v, want 0 updated, 1 unchanged", res)
	}
	if p := product(t, s, "A"); p.Version != 2 {
		t.Fatalf("version = %d, want 2", p.Version)
	}
	if cs := changes(t, s, run.ID); len(cs) != 1 {
		t.Fatalf("got %d changes, want only the create", len(cs))
	}
}

func TestUpdateRecordsOldAndNewValues(t *testing.T) {
	s := storetest.New(t)
	run := newRun(t, s)

	apply(t, s, run.ID, batch("b1", "2", sale("A", 1, 100)))
	apply(t, s, run.ID, batch("b2", "3", sale("A", 2, 90)))

	cs, err := s.ListChanges(ctx, run.ID, store.ChangeUpdated)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 {
		t.Fatalf("got %d updates, want 1", len(cs))
	}
	c := cs[0]
	if c.Old.Price != 100 || c.New.Price != 90 || *c.OldVersion != 1 || c.NewVersion != 2 {
		t.Fatalf("change = %+v old=%+v new=%+v", c, c.Old, c.New)
	}
}

func TestSKUsAreCaseSensitive(t *testing.T) {
	s := storetest.New(t)
	run := newRun(t, s)

	res := apply(t, s, run.ID, batch("b1", "2", sale("tea-1", 1, 100), sale("TEA-1", 1, 200)))
	if res.Created != 2 {
		t.Fatalf("result = %+v, want 2 created", res)
	}
	if p := product(t, s, "tea-1"); p.Price != 100 {
		t.Fatalf("tea-1 price = %d, want 100", p.Price)
	}
}

func TestLastBatchFinishesRun(t *testing.T) {
	s := storetest.New(t)
	run := newRun(t, s)

	res := apply(t, s, run.ID, lastBatch("b1", sale("A", 1, 100)))
	if res.RunStatus != store.RunCompleted {
		t.Fatalf("status = %s, want completed", res.RunStatus)
	}
	got := getRun(t, s, run.ID)
	if got.Open || got.FinishedAt == nil || got.Status != store.RunCompleted {
		t.Fatalf("run = %+v, want closed and completed", got)
	}

	if _, err := s.ApplyBatch(ctx, run.ID, batch("b2", "3", sale("B", 1, 1))); err == nil {
		t.Fatal("ApplyBatch on a finished run succeeded")
	}
	if ps, _ := s.ListProducts(ctx, "", "B"); len(ps) != 0 {
		t.Fatal("batch for a finished run was written")
	}
	newRun(t, s) // a new run can be opened again
}

func TestSetStatusAndStaleRuns(t *testing.T) {
	s := storetest.New(t)
	run := newRun(t, s)

	if err := s.SetStatus(ctx, run.ID, store.RunFailed, "supplier down"); err != nil {
		t.Fatal(err)
	}
	if got := getRun(t, s, run.ID); got.Status != store.RunFailed || got.LastError != "supplier down" || !got.Open {
		t.Fatalf("run = %+v, want failed, still open", got)
	}

	if err := s.SetStatus(ctx, run.ID, store.RunRunning, ""); err != nil {
		t.Fatal(err)
	}
	n, err := s.MarkStaleRunsInterrupted(ctx)
	if err != nil || n != 1 {
		t.Fatalf("MarkStaleRunsInterrupted = %d, %v; want 1", n, err)
	}
	if got := getRun(t, s, run.ID); got.Status != store.RunInterrupted {
		t.Fatalf("status = %s, want interrupted", got.Status)
	}

	if err := s.SetStatus(ctx, run.ID, store.RunCompleted, ""); err == nil {
		t.Fatal("SetStatus accepted a finished status")
	}
}

func TestImportLockAllowsOneHolder(t *testing.T) {
	s := storetest.New(t)

	release, err := s.AcquireImportLock(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcquireImportLock(ctx); !errors.Is(err, store.ErrImportLocked) {
		t.Fatalf("second AcquireImportLock error = %v, want ErrImportLocked", err)
	}
	if free, err := s.IsImportLockFree(ctx); err != nil || free {
		t.Fatalf("IsImportLockFree = %v, %v; want false", free, err)
	}

	release()
	if free, err := s.IsImportLockFree(ctx); err != nil || !free {
		t.Fatalf("after release IsImportLockFree = %v, %v; want true", free, err)
	}
	release2, err := s.AcquireImportLock(ctx)
	if err != nil {
		t.Fatalf("reacquire: %v", err)
	}
	release2()
}
