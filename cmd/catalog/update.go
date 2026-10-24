package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"text/tabwriter"

	"CoderPush/internal/importer"
	"CoderPush/internal/store"
	"CoderPush/internal/supplier"
)

func (a *app) update(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(a.errOut, usage)
		return errUsage
	}
	switch args[0] {
	case "start", "resume":
		return a.updateRun(args[0], args[1:])
	case "status":
		return a.updateStatus(args[1:])
	case "list":
		return a.updateList(args[1:])
	default:
		fmt.Fprintf(a.errOut, "unknown update command %q\n\n%s", args[0], usage)
		return errUsage
	}
}

// updateRun runs `update start` or `update resume`.
func (a *app) updateRun(mode string, args []string) error {
	fs := a.newFlags("update " + mode)
	supplierURL := fs.String("supplier", env("SUPPLIER_URL", defaultSupplierURL), "supplier base URL")
	crashAfter := fs.Int("crash-after-batch", 0, "exit abruptly right after committing batch `N` (demo of a crash)")
	if err := parse(fs, args); err != nil {
		return err
	}

	return a.withStore(func(ctx context.Context) error {
		ctx, stop := a.stopOnInterrupt(ctx)
		defer stop()

		im := &importer.Importer{Store: a.store, Supplier: supplier.NewClient(*supplierURL), Out: a.out}
		if *crashAfter > 0 {
			im.AfterBatch = func(n int) {
				if n == *crashAfter {
					// No cleanup on purpose: this is what a killed process looks like.
					fmt.Fprintf(a.errOut, "simulated crash right after batch %d was committed\n", n)
					os.Exit(1)
				}
			}
		}

		var (
			run store.Run
			err error
		)
		if mode == "start" {
			run, err = im.Start(ctx)
		} else {
			run, err = im.Resume(ctx)
		}
		if run.ID != 0 {
			fmt.Fprintln(a.out)
			if perr := a.printStatus(context.WithoutCancel(ctx), run.ID); perr != nil {
				return errors.Join(err, perr)
			}
		}
		return err
	})
}

// stopOnInterrupt cancels the returned context on the first Ctrl+C, so the
// importer stops after the batch in hand. A second Ctrl+C kills the process.
func (a *app) stopOnInterrupt(ctx context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt)
	done := make(chan struct{})

	go func() {
		select {
		case <-sigs:
			signal.Stop(sigs)
			fmt.Fprintln(a.errOut, "\nInterrupt received: stopping after the current batch (Ctrl+C again to force quit)...")
			cancel()
		case <-done:
		}
	}()

	return ctx, func() {
		close(done)
		signal.Stop(sigs)
		cancel()
	}
}

func (a *app) updateStatus(args []string) error {
	fs := a.newFlags("update status")
	runID := fs.Int64("run", 0, "run `ID` (default: the latest run)")
	if err := parse(fs, args); err != nil {
		return err
	}
	return a.withStore(func(ctx context.Context) error {
		id, err := a.runOrLatest(ctx, *runID)
		if err != nil || id == 0 {
			return err
		}
		return a.printStatus(ctx, id)
	})
}

// runOrLatest returns id, or the latest run's id when id is 0. It returns
// 0 and prints a hint when there is no run at all.
func (a *app) runOrLatest(ctx context.Context, id int64) (int64, error) {
	if id != 0 {
		return id, nil
	}
	run, err := a.store.LatestRun(ctx)
	if errors.Is(err, store.ErrRunNotFound) {
		fmt.Fprintln(a.out, "No updates yet. Run `catalog update start` to begin one.")
		return 0, nil
	}
	return run.ID, err
}

// effectiveStatus tells a live "running" run from one whose process died:
// the latter has no process holding the import lock.
func (a *app) effectiveStatus(ctx context.Context, run store.Run) (status string, live bool, err error) {
	if run.Status != store.RunRunning {
		return string(run.Status), false, nil
	}
	free, err := a.store.IsImportLockFree(ctx)
	if err != nil {
		return "", false, err
	}
	if free {
		return string(store.RunInterrupted), false, nil
	}
	return string(store.RunRunning), true, nil
}

func (a *app) printStatus(ctx context.Context, id int64) error {
	sum, err := a.store.RunSummary(ctx, id)
	if err != nil {
		return err
	}
	run := sum.Run
	status, live, err := a.effectiveStatus(ctx, run)
	if err != nil {
		return err
	}

	var verdict string
	switch {
	case run.Status.Finished():
		verdict = "FINISHED"
	case live:
		verdict = "IN PROGRESS"
	default:
		verdict = "NOT FINISHED"
	}

	finished := "-"
	if run.FinishedAt != nil {
		finished = run.FinishedAt.Format(timeLayout)
	}

	w := a.out
	fmt.Fprintf(w, "Run #%d   Status: %s (%s)\n", run.ID, status, verdict)
	fmt.Fprintf(w, "Started:      %s\n", run.StartedAt.Format(timeLayout))
	fmt.Fprintf(w, "Last update:  %s\n", run.UpdatedAt.Format(timeLayout))
	fmt.Fprintf(w, "Finished:     %s\n", finished)
	fmt.Fprintf(w, "Applied:      %s\n", plural(run.BatchesApplied, "batch", "batches"))
	fmt.Fprintf(w, "Changes:      %d created, %d updated, %d withdrawn, %d reactivated\n",
		sum.Changes[store.ChangeCreated], sum.Changes[store.ChangeUpdated],
		sum.Changes[store.ChangeWithdrawn], sum.Changes[store.ChangeReactivated])
	fmt.Fprintf(w, "Skipped:      %d stale, %d duplicate\n", run.SkippedStale, run.SkippedDuplicate)
	fmt.Fprintf(w, "Problems:     %s (%d unresolved)\n", plural(sum.Problems, "invalid record", "invalid records"), sum.OpenProblems)
	if run.LastError != "" {
		fmt.Fprintf(w, "Last error:   %s\n", run.LastError)
	}

	if !run.Status.Finished() && !live {
		fmt.Fprintln(w, "→ The catalog keeps every batch applied so far. Run `catalog update resume` to continue.")
	}
	if len(sum.Changes) > 0 {
		fmt.Fprintf(w, "→ Run `catalog changes -run %d` to see what changed.\n", run.ID)
	}
	if sum.OpenProblems > 0 {
		fmt.Fprintln(w, "→ Run `catalog problems` to see the records that could not be applied.")
	}
	return nil
}

func (a *app) updateList(args []string) error {
	fs := a.newFlags("update list")
	limit := fs.Int("limit", 20, "how many runs to show")
	if err := parse(fs, args); err != nil {
		return err
	}
	return a.withStore(func(ctx context.Context) error {
		runs, err := a.store.ListRuns(ctx, *limit)
		if err != nil {
			return err
		}
		if len(runs) == 0 {
			fmt.Fprintln(a.out, "No updates yet. Run `catalog update start` to begin one.")
			return nil
		}

		tw := tabwriter.NewWriter(a.out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "RUN\tSTATUS\tSTARTED\tFINISHED\tBATCHES\tSTALE\tDUPLICATE")
		for _, r := range runs {
			status, _, err := a.effectiveStatus(ctx, r)
			if err != nil {
				return err
			}
			finished := "-"
			if r.FinishedAt != nil {
				finished = r.FinishedAt.Format(timeLayout)
			}
			fmt.Fprintf(tw, "#%d\t%s\t%s\t%s\t%d\t%d\t%d\n", r.ID, status, r.StartedAt.Format(timeLayout),
				finished, r.BatchesApplied, r.SkippedStale, r.SkippedDuplicate)
		}
		return tw.Flush()
	})
}
