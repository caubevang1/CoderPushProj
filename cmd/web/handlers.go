package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"

	"CoderPush/internal/catalog"
	"CoderPush/internal/importer"
	"CoderPush/internal/store"
)

// runView is a run as staff should read it.
type runView struct {
	store.Summary
	Status  store.RunStatus // "interrupted" for a running run whose process died
	Live    bool            // a process is importing it right now
	Verdict string          // FINISHED, IN PROGRESS or NOT FINISHED
}

func (s *server) runView(ctx context.Context, id int64) (runView, error) {
	sum, err := s.store.RunSummary(ctx, id)
	if err != nil {
		return runView{}, err
	}
	status, live, err := s.store.EffectiveStatus(ctx, sum.Run)
	if err != nil {
		return runView{}, err
	}
	v := runView{Summary: sum, Status: status, Live: live, Verdict: "NOT FINISHED"}
	switch {
	case sum.Run.Status.Finished():
		v.Verdict = "FINISHED"
	case live:
		v.Verdict = "IN PROGRESS"
	}
	return v, nil
}

type dashboardData struct {
	Importing    bool
	Result       string
	Latest       *runView
	CanResume    bool
	Scenario     scenario
	Feeds        []string
	Active       int
	Withdrawn    int
	OpenProblems int
	Log          []string
}

func (s *server) dashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	importing, result := s.state()
	d := dashboardData{
		Importing: importing,
		Result:    result,
		Scenario:  s.supplier.Load().scenario,
		Feeds:     s.feeds,
		Log:       s.log.Lines(),
	}

	latest, err := s.store.LatestRun(ctx)
	switch {
	case err == nil:
		v, err := s.runView(ctx, latest.ID)
		if err != nil {
			s.fail(w, err)
			return
		}
		d.Latest = &v
	case !errors.Is(err, store.ErrRunNotFound):
		s.fail(w, err)
		return
	}

	if _, err := s.store.OpenRun(ctx); err == nil {
		d.CanResume = !importing
	} else if !errors.Is(err, store.ErrNoOpenRun) {
		s.fail(w, err)
		return
	}

	products, err := s.store.ListProducts(ctx, "", "")
	if err != nil {
		s.fail(w, err)
		return
	}
	for _, p := range products {
		if p.Status == catalog.StatusActive {
			d.Active++
		} else {
			d.Withdrawn++
		}
	}
	problems, err := s.store.ListProblems(ctx, false)
	if err != nil {
		s.fail(w, err)
		return
	}
	d.OpenProblems = len(problems)

	s.render(w, "dashboard", view{Title: "Catalog updates", Refresh: importing, Msg: r.URL.Query().Get("msg"), D: d})
}

func (s *server) postScenario(w http.ResponseWriter, r *http.Request) {
	sc := scenario{Feed: r.FormValue("feed")}
	var err error
	if sc.FailAt, err = formInt(r, "fail_at", 0, 100); err == nil {
		if sc.FailTimes, err = formInt(r, "fail_times", 0, 100); err == nil {
			var secs int
			secs, err = formInt(r, "delay", 0, 10)
			sc.Delay = time.Duration(secs) * time.Second
		}
	}
	if err == nil {
		err = s.setScenario(sc)
	}
	if err != nil {
		redirect(w, r, "/", err.Error())
		return
	}
	redirect(w, r, "/", "Supplier settings saved.")
}

func (s *server) postUpdate(mode string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		crashAfter, err := formInt(r, "crash_after", 0, 100)
		if err != nil {
			redirect(w, r, "/", err.Error())
			return
		}
		// Refuse at once what the importer would refuse anyway, instead of
		// answering "started" when the database is slow to reply. The
		// importer still checks again under the import lock.
		open, err := s.store.OpenRun(r.Context())
		switch {
		case err != nil && !errors.Is(err, store.ErrNoOpenRun):
			s.fail(w, err)
			return
		case mode == "start" && err == nil:
			redirect(w, r, "/", explain(fmt.Errorf("%w: run #%d is %s", store.ErrRunAlreadyOpen, open.ID, open.Status)))
			return
		case mode == "resume" && err != nil:
			redirect(w, r, "/", explain(importer.ErrNothingToResume))
			return
		}
		if err := s.startImport(mode, crashAfter); err != nil {
			redirect(w, r, "/", err.Error())
			return
		}
		// Refusals (an update already open, nothing to resume) come back at
		// once: show them instead of an empty "started".
		msg := "Update started. This page refreshes until it is done."
		if s.waitFor(700 * time.Millisecond) {
			_, msg = s.state()
		}
		redirect(w, r, "/", msg)
	}
}

func (s *server) postStop(w http.ResponseWriter, r *http.Request) {
	if !s.stopImport() {
		redirect(w, r, "/", "No update is running.")
		return
	}
	redirect(w, r, "/", "Stopping after the current batch...")
}

func (s *server) postReset(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if importing, _ := s.state(); importing {
		redirect(w, r, "/", errBusy.Error())
		return
	}
	free, err := s.store.IsImportLockFree(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	if !free {
		redirect(w, r, "/", store.ErrImportLocked.Error()+" in another process.")
		return
	}
	if err := s.store.DeleteAll(ctx); err != nil {
		s.fail(w, err)
		return
	}
	if err := s.setScenario(defaultScenario); err != nil {
		s.fail(w, err)
		return
	}
	s.log.Reset()
	s.mu.Lock()
	s.result = ""
	s.mu.Unlock()
	redirect(w, r, "/", "Everything was deleted: empty catalog, no updates, default supplier settings.")
}

func (s *server) runs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	runs, err := s.store.ListRuns(ctx, 50)
	if err != nil {
		s.fail(w, err)
		return
	}
	// One query for the list: change counts are on each run's own page.
	rows := make([]runRow, 0, len(runs))
	for _, run := range runs {
		status, _, err := s.store.EffectiveStatus(ctx, run) // queries only for a "running" run
		if err != nil {
			s.fail(w, err)
			return
		}
		rows = append(rows, runRow{Run: run, Status: status})
	}
	s.render(w, "runs", view{Title: "Updates", D: rows})
}

type runRow struct {
	Run    store.Run
	Status store.RunStatus
}

type runDetailData struct {
	Run     runView
	Kind    string
	Kinds   []store.ChangeKind
	Changes []store.Change
}

func (s *server) runDetail(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	kind := store.ChangeKind(r.URL.Query().Get("kind"))
	kinds := []store.ChangeKind{store.ChangeCreated, store.ChangeUpdated, store.ChangeWithdrawn, store.ChangeReactivated}
	if kind != "" && !slices.Contains(kinds, kind) {
		http.Error(w, fmt.Sprintf("invalid kind %q", kind), http.StatusBadRequest)
		return
	}

	v, err := s.runView(ctx, id)
	if errors.Is(err, store.ErrRunNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	changes, err := s.store.ListChanges(ctx, id, kind)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, "run", view{
		Title:   fmt.Sprintf("Update #%d", id),
		Refresh: v.Live,
		D:       runDetailData{Run: v, Kind: string(kind), Kinds: kinds, Changes: changes},
	})
}

type problemsData struct {
	Open     *runView // the unfinished update, if any
	All      bool
	Problems []store.Problem
}

func (s *server) problems(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	d := problemsData{All: r.URL.Query().Get("all") == "1"}

	run, err := s.store.OpenRun(ctx)
	switch {
	case err == nil:
		v, err := s.runView(ctx, run.ID)
		if err != nil {
			s.fail(w, err)
			return
		}
		d.Open = &v
	case !errors.Is(err, store.ErrNoOpenRun):
		s.fail(w, err)
		return
	}

	if d.Problems, err = s.store.ListProblems(ctx, d.All); err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, "problems", view{Title: "Problems", Msg: r.URL.Query().Get("msg"), D: d})
}

func (s *server) postResolve(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := s.store.ResolveProblem(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrProblemNotFound) {
			http.NotFound(w, r)
			return
		}
		s.fail(w, err)
		return
	}
	redirect(w, r, "/problems", fmt.Sprintf("Problem %d marked as resolved.", id))
}

type productsData struct {
	Status   string
	Products []store.StoredProduct
}

func (s *server) products(w http.ResponseWriter, r *http.Request) {
	status := catalog.Status(r.URL.Query().Get("status"))
	switch status {
	case "", catalog.StatusActive, catalog.StatusWithdrawn:
	default:
		http.Error(w, fmt.Sprintf("invalid status %q", status), http.StatusBadRequest)
		return
	}
	products, err := s.store.ListProducts(r.Context(), status, "")
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, "products", view{Title: "Catalog", D: productsData{Status: string(status), Products: products}})
}

func (s *server) fail(w http.ResponseWriter, err error) {
	log.Print(err)
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

// redirect answers a form post with "see other", carrying msg to show.
func redirect(w http.ResponseWriter, r *http.Request, path, msg string) {
	if msg != "" {
		path += "?msg=" + url.QueryEscape(msg)
	}
	http.Redirect(w, r, path, http.StatusSeeOther)
}

// formInt reads an optional integer form field in [min, max].
func formInt(r *http.Request, name string, min, max int) (int, error) {
	v := r.FormValue(name)
	if v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < min || n > max {
		return 0, fmt.Errorf("%s must be a number from %d to %d", name, min, max)
	}
	return n, nil
}
