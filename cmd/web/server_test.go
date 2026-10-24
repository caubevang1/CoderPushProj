package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"testing"
	"time"

	"CoderPush/internal/importer"
	"CoderPush/internal/store"
	"CoderPush/internal/store/storetest"
	"CoderPush/internal/supplier"
)

type testApp struct {
	t      *testing.T
	srv    *server
	ts     *httptest.Server
	client *http.Client
}

// newTestApp serves the web app on an empty test database, with a fake
// supplier that answers at once and a client that retries in milliseconds.
func newTestApp(t *testing.T) *testApp {
	t.Helper()
	st := storetest.New(t)
	srv, err := newServer(st, "../../testdata/feeds", "")
	if err != nil {
		t.Fatal(err)
	}
	srv.newClient = func(baseURL string) importer.Fetcher {
		c := supplier.NewClient(baseURL)
		c.BaseDelay = time.Millisecond
		return c
	}
	// A crashed process runs no more code. runtime.Goexit stops the import
	// goroutine the same way, except that deferred calls still run, which
	// releases the import lock as MySQL does when a process dies.
	srv.exit = func(int) { runtime.Goexit() }
	if err := srv.setScenario(scenario{Feed: "initial"}); err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(srv.routes())
	srv.selfURL = ts.URL
	t.Cleanup(func() {
		srv.stopImport()
		srv.waitIdle()
		ts.Close()
	})

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &testApp{t: t, srv: srv, ts: ts, client: client}
}

// post submits a form and returns the message carried by the redirect.
func (a *testApp) post(path string, form url.Values) string {
	a.t.Helper()
	resp, err := a.client.PostForm(a.ts.URL+path, form)
	if err != nil {
		a.t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		a.t.Fatalf("POST %s: status %d, want 303", path, resp.StatusCode)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		a.t.Fatal(err)
	}
	return loc.Query().Get("msg")
}

func (a *testApp) get(path string) string {
	a.t.Helper()
	resp, err := a.client.Get(a.ts.URL + path)
	if err != nil {
		a.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		a.t.Fatalf("GET %s: status %d: %s", path, resp.StatusCode, body)
	}
	return string(body)
}

// update presses Start or Resume and waits for the import to stop.
func (a *testApp) update(mode string, crashAfter string) string {
	a.t.Helper()
	msg := a.post("/update/"+mode, url.Values{"crash_after": {crashAfter}})
	if !a.srv.waitFor(30 * time.Second) {
		a.t.Fatalf("update %s still running after 30 s", mode)
	}
	return msg
}

func (a *testApp) latest() store.Summary {
	a.t.Helper()
	ctx := context.Background()
	run, err := a.srv.store.LatestRun(ctx)
	if err != nil {
		a.t.Fatal(err)
	}
	sum, err := a.srv.store.RunSummary(ctx, run.ID)
	if err != nil {
		a.t.Fatal(err)
	}
	return sum
}

func mustContain(t *testing.T, what, s string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(s, want) {
			t.Errorf("%s does not contain %q", what, want)
		}
	}
}

func TestFirstUpdateThenSameDataAgain(t *testing.T) {
	a := newTestApp(t)

	a.update("start", "0")
	sum := a.latest()
	if sum.Run.Status != store.RunCompleted || sum.Changes[store.ChangeCreated] != 10 {
		t.Fatalf("first update: status %s, changes %v", sum.Run.Status, sum.Changes)
	}
	mustContain(t, "dashboard", a.get("/"), "completed", "FINISHED", "10 active")
	if rows := strings.Count(a.get("/products"), "<tr>") - 1; rows != 10 {
		t.Errorf("catalog page lists %d products, want 10", rows)
	}

	a.update("start", "0")
	sum = a.latest()
	if sum.Run.Status != store.RunCompleted || len(sum.Changes) != 0 {
		t.Fatalf("same data again: status %s, changes %v", sum.Run.Status, sum.Changes)
	}
	mustContain(t, "run page", a.get(fmt.Sprintf("/runs/%d", sum.Run.ID)), "No changes")
	if n := strings.Count(a.get("/runs"), `badge completed"`); n != 2 {
		t.Errorf("runs page shows %d completed updates, want 2", n)
	}
}

func TestSupplierOutageThenResume(t *testing.T) {
	a := newTestApp(t)
	a.update("start", "0")

	msg := a.post("/supplier", url.Values{"feed": {"changes"}, "fail_at": {"3"}})
	mustContain(t, "supplier message", msg, "saved")
	a.update("start", "0")

	sum := a.latest()
	if sum.Run.Status != store.RunFailed || sum.Run.BatchesApplied != 2 {
		t.Fatalf("after outage: status %s after %d batches, want failed after 2", sum.Run.Status, sum.Run.BatchesApplied)
	}
	mustContain(t, "dashboard", a.get("/"), "NOT FINISHED", "supplier unavailable after 5 attempts")

	msg = a.update("start", "0")
	mustContain(t, "second start", msg, "already open", "Resume")

	a.post("/supplier", url.Values{"feed": {"changes"}, "fail_at": {"0"}})
	a.update("resume", "0")
	sum = a.latest()
	if sum.Run.Status != store.RunCompletedWithProblems || sum.Run.BatchesApplied != 3 {
		t.Fatalf("after resume: status %s after %d batches", sum.Run.Status, sum.Run.BatchesApplied)
	}
	mustContain(t, "run page", a.get(fmt.Sprintf("/runs/%d", sum.Run.ID)), "updated", "withdrawn", "→")
	mustContain(t, "problems page", a.get("/problems"), "Mark resolved")
}

func TestCrashThenResume(t *testing.T) {
	a := newTestApp(t)

	a.update("start", "1")
	sum := a.latest()
	if sum.Run.Status != store.RunRunning || sum.Run.BatchesApplied != 1 {
		t.Fatalf("after crash: status %s after %d batches, want running after 1", sum.Run.Status, sum.Run.BatchesApplied)
	}
	// Nobody holds the import lock any more: the run is shown as interrupted.
	mustContain(t, "dashboard", a.get("/"), "interrupted", "NOT FINISHED")

	a.update("resume", "0")
	sum = a.latest()
	if sum.Run.Status != store.RunCompleted || sum.Changes[store.ChangeCreated] != 10 {
		t.Fatalf("after resume: status %s, changes %v", sum.Run.Status, sum.Changes)
	}
}

func TestStopAndReset(t *testing.T) {
	a := newTestApp(t)
	if err := a.srv.setScenario(scenario{Feed: "initial", Delay: 2 * time.Second}); err != nil {
		t.Fatal(err)
	}

	msg := a.post("/update/start", nil)
	mustContain(t, "start message", msg, "started")
	mustContain(t, "reset while running", a.post("/reset", nil), "already running")

	mustContain(t, "stop message", a.post("/update/stop", nil), "Stopping")
	a.srv.waitIdle()
	if sum := a.latest(); sum.Run.Status != store.RunInterrupted {
		t.Fatalf("after stop: status %s, want interrupted", sum.Run.Status)
	}

	mustContain(t, "reset", a.post("/reset", nil), "deleted")
	mustContain(t, "dashboard", a.get("/"), "No updates yet")
	if sc := a.srv.supplier.Load().scenario; sc != defaultScenario {
		t.Errorf("scenario after reset = %+v, want the default", sc)
	}
}

func TestBadInput(t *testing.T) {
	a := newTestApp(t)
	mustContain(t, "unknown feed", a.post("/supplier", url.Values{"feed": {"nope"}}), `unknown feed "nope"`)
	mustContain(t, "bad number", a.post("/supplier", url.Values{"feed": {"initial"}, "fail_at": {"x"}}), "fail_at must be a number")
	mustContain(t, "resume nothing", a.update("resume", "0"), "no unfinished update")

	resp, err := a.client.Get(a.ts.URL + "/runs/999999999")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown run: status %d, want 404", resp.StatusCode)
	}
}
