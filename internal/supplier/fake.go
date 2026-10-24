package supplier

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"CoderPush/internal/catalog"
)

// FeedBatch is one batch file of a feed directory. The fake supplier adds
// next_cursor and has_more from the file's position in the feed.
type FeedBatch struct {
	BatchID string         `json:"batch_id"`
	Items   []catalog.Item `json:"items"`
}

// LoadFeed reads every *.json file of dir, in file name order.
func LoadFeed(dir string) ([]FeedBatch, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no *.json batch files in %s", dir)
	}
	sort.Strings(files)

	feed := make([]FeedBatch, 0, len(files))
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var b FeedBatch
		if err := json.Unmarshal(data, &b); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		if b.BatchID == "" {
			return nil, fmt.Errorf("%s: missing batch_id", f)
		}
		feed = append(feed, b)
	}
	return feed, nil
}

// FakeOptions makes the fake supplier misbehave on purpose.
type FakeOptions struct {
	FailAt    int           // batch number (1-based) that answers 503; 0 disables
	FailTimes int           // how many times FailAt fails before recovering; 0 means forever
	FailRate  float64       // probability of a random 503 on any request
	Delay     time.Duration // wait before answering each request
}

// Fake serves a feed as GET /batches?cursor=<n>. Cursor "" (or "1") is
// the first batch, "2" the second, and so on.
type Fake struct {
	feed []FeedBatch
	opts FakeOptions

	mu       sync.Mutex
	failures int // 503s already sent for FailAt
}

// NewFake returns a fake supplier serving feed.
func NewFake(feed []FeedBatch, opts FakeOptions) *Fake {
	return &Fake{feed: feed, opts: opts}
}

func (f *Fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/batches" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "only GET is supported")
		return
	}

	if f.opts.Delay > 0 {
		select {
		case <-time.After(f.opts.Delay):
		case <-r.Context().Done():
			return
		}
	}

	n, err := f.batchNumber(r.URL.Query().Get("cursor"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if f.shouldFail(n) {
		writeError(w, http.StatusServiceUnavailable, "supplier temporarily unavailable")
		return
	}

	fb := f.feed[n-1]
	b := Batch{BatchID: fb.BatchID, Items: fb.Items, HasMore: n < len(f.feed)}
	if b.Items == nil {
		b.Items = []catalog.Item{}
	}
	if b.HasMore {
		b.NextCursor = strconv.Itoa(n + 1)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(b)
}

func (f *Fake) batchNumber(cursor string) (int, error) {
	if cursor == "" {
		return 1, nil
	}
	n, err := strconv.Atoi(cursor)
	if err != nil || n < 1 || n > len(f.feed) {
		return 0, errors.New("invalid cursor " + strconv.Quote(cursor))
	}
	return n, nil
}

func (f *Fake) shouldFail(n int) bool {
	if f.opts.FailRate > 0 && rand.Float64() < f.opts.FailRate {
		return true
	}
	if f.opts.FailAt == 0 || n != f.opts.FailAt {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.opts.FailTimes > 0 && f.failures >= f.opts.FailTimes {
		return false
	}
	f.failures++
	return true
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
