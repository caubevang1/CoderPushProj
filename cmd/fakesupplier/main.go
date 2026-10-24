// Command fakesupplier serves a feed directory as the supplier batch API,
// optionally failing on purpose to demonstrate recovery.
package main

import (
	"flag"
	"log"
	"net/http"
	"time"

	"CoderPush/internal/supplier"
)

func main() {
	var (
		addr      = flag.String("addr", ":8081", "listen address")
		feedDir   = flag.String("feed", "testdata/feeds/initial", "directory of batch files, served in file name order")
		failAt    = flag.Int("fail-at", 0, "batch number (1-based) that answers 503; 0 disables")
		failTimes = flag.Int("fail-times", 0, "how many times -fail-at fails before recovering; 0 means forever")
		failRate  = flag.Float64("fail-rate", 0, "probability (0-1) of a random 503 on any request")
		delay     = flag.Duration("delay", 0, "wait before answering each request, e.g. 2s")
	)
	flag.Parse()

	feed, err := supplier.LoadFeed(*feedDir)
	if err != nil {
		log.Fatalf("load feed: %v", err)
	}
	fake := supplier.NewFake(feed, supplier.FakeOptions{
		FailAt:    *failAt,
		FailTimes: *failTimes,
		FailRate:  *failRate,
		Delay:     *delay,
	})

	log.Printf("serving %d batches from %s on %s", len(feed), *feedDir, *addr)
	log.Fatal(http.ListenAndServe(*addr, logRequests(fake)))
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.code = code
	r.ResponseWriter.WriteHeader(code)
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(rec, r)
		log.Printf("%s %s -> %d (%s)", r.Method, r.URL.RequestURI(), rec.code, time.Since(start).Round(time.Millisecond))
	})
}
