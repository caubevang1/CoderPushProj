// Command catalog is the staff tool for supplier catalog updates.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"CoderPush/internal/importer"
	"CoderPush/internal/report"
	"CoderPush/internal/store"
)

const (
	defaultDSN         = "admin:admin@tcp(127.0.0.1:3307)/coderpush_db?parseTime=true&loc=UTC&charset=utf8mb4"
	defaultSupplierURL = "http://localhost:8081"
	timeLayout         = "2006-01-02 15:04:05 UTC"
)

const usage = `Usage: catalog <command> [flags]

Commands:
  migrate                         create the database tables
  update start [-supplier URL]    start a new catalog update
  update resume [-supplier URL]   continue the unfinished update
  update status [-run ID]         show whether an update is finished (default: latest)
  update list [-limit N]          list recent updates
  changes [-run ID] [-kind KIND]  what an update changed (created, updated, withdrawn, reactivated)
  problems [-all]                 what still needs attention
  problems resolve ID             mark a problem as handled
  products [-status S] [-sku SKU] show the catalog (status: active, withdrawn)

Environment:
  CATALOG_DSN    MySQL DSN (default ` + defaultDSN + `)
  SUPPLIER_URL   supplier base URL (default ` + defaultSupplierURL + `)
`

// errUsage marks a mistake in the command line; usage has been printed.
var errUsage = errors.New("invalid command line")

type app struct {
	out, errOut io.Writer
	store       *store.Store
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, out, errOut io.Writer) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(out, usage)
		return 0
	}

	a := &app{out: out, errOut: errOut}
	err := a.dispatch(args[0], args[1:])
	if err == nil {
		return 0
	}
	if !errors.Is(err, errUsage) && !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(errOut, "error:", explain(err))
	}
	return 1
}

func (a *app) dispatch(cmd string, args []string) error {
	switch cmd {
	case "migrate":
		return a.withStore(func(ctx context.Context) error { return a.migrate(ctx) })
	case "update":
		return a.update(args)
	case "changes":
		return a.changes(args)
	case "problems":
		return a.problems(args)
	case "products":
		return a.products(args)
	default:
		fmt.Fprintf(a.errOut, "unknown command %q\n\n%s", cmd, usage)
		return errUsage
	}
}

// withStore connects to the database, runs fn and disconnects.
func (a *app) withStore(fn func(ctx context.Context) error) error {
	ctx := context.Background()
	s, err := store.Open(ctx, env("CATALOG_DSN", defaultDSN))
	if err != nil {
		return fmt.Errorf("%w (is MySQL running? try `docker compose up -d`)", err)
	}
	defer s.Close()
	a.store = s
	return fn(ctx)
}

func (a *app) migrate(ctx context.Context) error {
	if err := a.store.Migrate(ctx); err != nil {
		return err
	}
	fmt.Fprintln(a.out, "Database tables are ready.")
	return nil
}

// newFlags returns a flag set that reports mistakes on errOut.
func (a *app) newFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(a.errOut)
	return fs
}

// parse parses args and rejects anything left over.
func parse(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		return errUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(fs.Output(), "unexpected argument %q\n", fs.Arg(0))
		fs.Usage()
		return errUsage
	}
	return nil
}

// explain adds what staff should do next to well-known errors.
func explain(err error) string {
	switch {
	case errors.Is(err, store.ErrRunAlreadyOpen):
		return err.Error() + "\nRun `catalog update resume` to finish it first."
	case errors.Is(err, store.ErrImportLocked):
		return err.Error() + "\nWait for it to finish, or check `catalog update status`."
	case errors.Is(err, importer.ErrNothingToResume):
		return err.Error() + "\nRun `catalog update start` to begin a new update."
	}
	return err.Error()
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func plural(n int, one, many string) string {
	return report.Plural(n, one, many)
}
