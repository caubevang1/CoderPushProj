package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"CoderPush/internal/importer"
	"CoderPush/internal/store"
	"CoderPush/internal/supplier"
)

// errBusy means this process is already running an import.
var errBusy = errors.New("an update is already running, wait for it or press Stop")

// scenario is how the embedded fake supplier behaves.
type scenario struct {
	Feed      string        // a sub-directory of the feeds directory
	FailAt    int           // batch that answers 503; 0 disables
	FailTimes int           // how many times FailAt fails; 0 means forever
	Delay     time.Duration // wait before each answer, so progress is visible
}

var defaultScenario = scenario{Feed: "initial", Delay: time.Second}

// liveSupplier is the fake supplier currently served under /supplier.
type liveSupplier struct {
	scenario scenario
	handler  http.Handler
}

type server struct {
	store    *store.Store
	feedsDir string
	feeds    []string // feed names, from the feeds directory
	selfURL  string   // this server's base URL, used by the importer

	// newClient builds the supplier client; tests make it retry faster.
	newClient func(baseURL string) importer.Fetcher
	// exit ends the process for the crash scenario; tests replace it.
	exit func(code int)

	pages pages
	log   *logBuffer

	supplier atomic.Pointer[liveSupplier]

	mu     sync.Mutex
	cancel context.CancelFunc // set while an import runs in this process
	done   chan struct{}      // closed when that import returns
	result string             // outcome of the last import, for the dashboard
}

func newServer(s *store.Store, feedsDir, selfURL string) (*server, error) {
	entries, err := os.ReadDir(feedsDir)
	if err != nil {
		return nil, fmt.Errorf("read feeds: %w", err)
	}
	var feeds []string
	for _, e := range entries {
		if e.IsDir() {
			feeds = append(feeds, e.Name())
		}
	}

	srv := &server{
		store:     s,
		feedsDir:  feedsDir,
		feeds:     feeds,
		selfURL:   selfURL,
		newClient: func(baseURL string) importer.Fetcher { return supplier.NewClient(baseURL) },
		exit: func(code int) {
			// Let the browser get its redirect before the server disappears.
			// The batch is already committed and the lock still held, so
			// waiting changes nothing about the crash itself.
			time.Sleep(time.Second)
			os.Exit(code)
		},
		log: newLogBuffer(100),
	}
	if srv.pages, err = parsePages(); err != nil {
		return nil, err
	}
	if err := srv.setScenario(defaultScenario); err != nil {
		return nil, err
	}
	return srv, nil
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/supplier/", http.StripPrefix("/supplier", http.HandlerFunc(s.serveSupplier)))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })

	mux.HandleFunc("GET /{$}", s.dashboard)
	mux.HandleFunc("POST /supplier", s.postScenario)
	mux.HandleFunc("POST /update/start", s.postUpdate("start"))
	mux.HandleFunc("POST /update/resume", s.postUpdate("resume"))
	mux.HandleFunc("POST /update/stop", s.postStop)
	mux.HandleFunc("POST /reset", s.postReset)

	mux.HandleFunc("GET /runs", s.runs)
	mux.HandleFunc("GET /runs/{id}", s.runDetail)
	mux.HandleFunc("GET /problems", s.problems)
	mux.HandleFunc("POST /problems/{id}/resolve", s.postResolve)
	mux.HandleFunc("GET /products", s.products)
	return mux
}

func (s *server) serveSupplier(w http.ResponseWriter, r *http.Request) {
	s.supplier.Load().handler.ServeHTTP(w, r)
}

// setScenario replaces the fake supplier. A new Fake also starts counting
// FailTimes from zero.
func (s *server) setScenario(sc scenario) error {
	if !slices.Contains(s.feeds, sc.Feed) {
		return fmt.Errorf("unknown feed %q", sc.Feed)
	}
	feed, err := supplier.LoadFeed(filepath.Join(s.feedsDir, sc.Feed))
	if err != nil {
		return err
	}
	fake := supplier.NewFake(feed, supplier.FakeOptions{FailAt: sc.FailAt, FailTimes: sc.FailTimes, Delay: sc.Delay})
	s.supplier.Store(&liveSupplier{scenario: sc, handler: fake})
	return nil
}

// startImport runs `update start` or `update resume` in the background, so
// the update outlives the request that started it. crashAfter > 0 ends the
// whole process right after that many batches are committed.
func (s *server) startImport(mode string, crashAfter int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		return errBusy
	}

	im := &importer.Importer{Store: s.store, Supplier: s.newClient(s.selfURL + "/supplier"), Out: s.log}
	if crashAfter > 0 {
		im.AfterBatch = func(n int) {
			if n == crashAfter {
				// No cleanup on purpose: this is what a killed process looks like.
				fmt.Fprintf(s.log, "simulated crash right after batch %d was committed\n", n)
				s.exit(1)
			}
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s.cancel, s.done, s.result = cancel, done, ""
	fmt.Fprintf(s.log, "--- update %s ---\n", mode)

	go func() {
		defer close(done)
		var result string
		defer func() {
			cancel()
			s.mu.Lock()
			s.cancel, s.done, s.result = nil, nil, result
			s.mu.Unlock()
		}()

		var (
			run store.Run
			err error
		)
		if mode == "start" {
			run, err = im.Start(ctx)
		} else {
			run, err = im.Resume(ctx)
		}
		switch {
		case err != nil:
			result = explain(err)
		case run.ID != 0:
			result = fmt.Sprintf("Run #%d finished: %s.", run.ID, run.Status)
		}
		if result != "" {
			fmt.Fprintln(s.log, result)
		}
	}()
	return nil
}

// stopImport cancels the import running in this process, if any. The
// importer stops after the batch in hand and records the run as interrupted.
func (s *server) stopImport() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel == nil {
		return false
	}
	s.cancel()
	return true
}

// waitFor blocks until no import runs in this process, or d has passed.
// It reports whether the process is idle.
func (s *server) waitFor(d time.Duration) bool {
	s.mu.Lock()
	done := s.done
	s.mu.Unlock()
	if done == nil {
		return true
	}
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

func (s *server) waitIdle() { s.waitFor(time.Minute) }

// state returns whether an import runs in this process and the outcome of
// the last one.
func (s *server) state() (importing bool, result string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cancel != nil, s.result
}

// explain adds what to do next to well-known errors, like the CLI does.
func explain(err error) string {
	switch {
	case errors.Is(err, store.ErrRunAlreadyOpen):
		return err.Error() + ". Press Resume to finish it first."
	case errors.Is(err, store.ErrImportLocked):
		return err.Error() + ". Wait for it to finish."
	case errors.Is(err, importer.ErrNothingToResume):
		return err.Error() + ". Press Start update to begin a new one."
	case errors.Is(err, importer.ErrInterrupted):
		return "Update stopped. The catalog keeps every batch applied so far; press Resume to continue."
	}
	return "Update stopped: " + err.Error()
}
