package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"

	"CoderPush/internal/catalog"
	"CoderPush/internal/store"
)

func (a *app) changes(args []string) error {
	fs := a.newFlags("changes")
	runID := fs.Int64("run", 0, "run `ID` (default: the latest run)")
	kind := fs.String("kind", "", "only this kind: created, updated, withdrawn or reactivated")
	if err := parse(fs, args); err != nil {
		return err
	}
	switch store.ChangeKind(*kind) {
	case "", store.ChangeCreated, store.ChangeUpdated, store.ChangeWithdrawn, store.ChangeReactivated:
	default:
		fmt.Fprintf(a.errOut, "invalid -kind %q\n", *kind)
		fs.Usage()
		return errUsage
	}

	return a.withStore(func(ctx context.Context) error {
		id, err := a.runOrLatest(ctx, *runID)
		if err != nil || id == 0 {
			return err
		}
		if _, err := a.store.GetRun(ctx, id); err != nil {
			return fmt.Errorf("run #%d: %w", id, err)
		}
		changes, err := a.store.ListChanges(ctx, id, store.ChangeKind(*kind))
		if err != nil {
			return err
		}
		if len(changes) == 0 {
			fmt.Fprintf(a.out, "Run #%d made no changes%s.\n", id, kindSuffix(*kind))
			return nil
		}

		fmt.Fprintf(a.out, "Run #%d: %d changes%s\n\n", id, len(changes), kindSuffix(*kind))
		tw := tabwriter.NewWriter(a.out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "BATCH\tSKU\tCHANGE\tVERSION\tDETAILS")
		for _, c := range changes {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", c.BatchID, c.SKU, c.Kind, versions(c), details(c))
		}
		return tw.Flush()
	})
}

func kindSuffix(kind string) string {
	if kind == "" {
		return ""
	}
	return " of kind " + kind
}

func versions(c store.Change) string {
	if c.OldVersion == nil {
		return "v" + strconv.FormatInt(c.NewVersion, 10)
	}
	return fmt.Sprintf("v%d → v%d", *c.OldVersion, c.NewVersion)
}

// details describes a change in words staff can check quickly.
func details(c store.Change) string {
	n, o := c.New, c.Old
	switch {
	case n == nil:
		return ""
	case c.Kind == store.ChangeWithdrawn && o == nil:
		return "withdrawn before it was ever listed"
	case c.Kind == store.ChangeWithdrawn:
		return "no longer for sale"
	case o == nil || c.Kind == store.ChangeReactivated:
		return fmt.Sprintf("%s, %s", n.Name, money(n.Price, n.Currency))
	}

	var diffs []string
	if o.Name != n.Name {
		diffs = append(diffs, fmt.Sprintf("name %q → %q", o.Name, n.Name))
	}
	if o.Price != n.Price || o.Currency != n.Currency {
		diffs = append(diffs, fmt.Sprintf("price %s → %s", money(o.Price, o.Currency), money(n.Price, n.Currency)))
	}
	return strings.Join(diffs, "; ")
}

// money formats an amount in the currency's smallest unit, e.g. 45,000 VND.
func money(amount int64, currency string) string {
	s := strconv.FormatInt(amount, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	if neg {
		s = "-" + s
	}
	return strings.TrimSpace(s + " " + currency)
}

func (a *app) problems(args []string) error {
	if len(args) > 0 && args[0] == "resolve" {
		return a.resolveProblem(args[1:])
	}

	fs := a.newFlags("problems")
	all := fs.Bool("all", false, "include problems already resolved")
	if err := parse(fs, args); err != nil {
		return err
	}

	return a.withStore(func(ctx context.Context) error {
		nothing := true

		run, err := a.store.OpenRun(ctx)
		switch {
		case err == nil:
			nothing = false
			status, live, err := a.effectiveStatus(ctx, run)
			if err != nil {
				return err
			}
			fmt.Fprintf(a.out, "Unfinished update: run #%d is %s after %s.\n", run.ID, status, plural(run.BatchesApplied, "batch", "batches"))
			if run.LastError != "" {
				fmt.Fprintf(a.out, "  Last error: %s\n", run.LastError)
			}
			if !live {
				fmt.Fprintln(a.out, "  → Run `catalog update resume` to finish it.")
			}
			fmt.Fprintln(a.out)
		case !errors.Is(err, store.ErrNoOpenRun):
			return err
		}

		problems, err := a.store.ListProblems(ctx, *all)
		if err != nil {
			return err
		}
		if len(problems) > 0 {
			nothing = false
			fmt.Fprintf(a.out, "Records that could not be applied (%d):\n\n", len(problems))
			tw := tabwriter.NewWriter(a.out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tRUN\tBATCH\tSKU\tREASON\tRESOLVED")
			for _, p := range problems {
				resolved := "no"
				if p.ResolvedAt != nil {
					resolved = p.ResolvedAt.Format(timeLayout)
				}
				fmt.Fprintf(tw, "%d\t#%d\t%s\t%s\t%s\t%s\n", p.ID, p.RunID, p.BatchID, orDash(p.SKU), p.Reason, resolved)
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			fmt.Fprintln(a.out, "\n→ Fix the record at the supplier, then `catalog problems resolve ID`.")
		}

		if nothing {
			fmt.Fprintln(a.out, "Nothing needs attention.")
		}
		return nil
	})
}

func (a *app) resolveProblem(args []string) error {
	fs := a.newFlags("problems resolve")
	if err := fs.Parse(args); err != nil {
		return errUsage
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(a.errOut, "usage: catalog problems resolve ID")
		return errUsage
	}
	id, err := strconv.ParseInt(fs.Arg(0), 10, 64)
	if err != nil || id <= 0 {
		fmt.Fprintf(a.errOut, "invalid problem ID %q\n", fs.Arg(0))
		return errUsage
	}

	return a.withStore(func(ctx context.Context) error {
		if err := a.store.ResolveProblem(ctx, id); err != nil {
			return err
		}
		fmt.Fprintf(a.out, "Problem %d marked as resolved.\n", id)
		return nil
	})
}

func (a *app) products(args []string) error {
	fs := a.newFlags("products")
	status := fs.String("status", "", "only this status: active or withdrawn")
	sku := fs.String("sku", "", "only this SKU")
	if err := parse(fs, args); err != nil {
		return err
	}
	switch catalog.Status(*status) {
	case "", catalog.StatusActive, catalog.StatusWithdrawn:
	default:
		fmt.Fprintf(a.errOut, "invalid -status %q\n", *status)
		fs.Usage()
		return errUsage
	}

	return a.withStore(func(ctx context.Context) error {
		products, err := a.store.ListProducts(ctx, catalog.Status(*status), *sku)
		if err != nil {
			return err
		}
		if len(products) == 0 {
			fmt.Fprintln(a.out, "No products found.")
			return nil
		}

		active := 0
		for _, p := range products {
			if p.Status == catalog.StatusActive {
				active++
			}
		}
		fmt.Fprintf(a.out, "%d products (%d active, %d withdrawn)\n\n", len(products), active, len(products)-active)

		tw := tabwriter.NewWriter(a.out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "SKU\tNAME\tPRICE\tSTATUS\tVERSION\tLAST RUN")
		for _, p := range products {
			price := money(p.Price, p.Currency)
			if p.Name == "" && p.Status == catalog.StatusWithdrawn {
				price = "-" // tombstone of a product never listed
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t#%d\n", p.SKU, orDash(p.Name), price, p.Status, p.Version, p.LastRunID)
		}
		return tw.Flush()
	})
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
