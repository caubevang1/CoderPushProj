package supplier

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"CoderPush/internal/catalog"
)

// script answers each request with the next status code; 200 serves a
// fixed batch. It counts requests.
type script struct {
	codes []int
	calls atomic.Int32
}

func (s *script) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	i := int(s.calls.Add(1)) - 1
	code := s.codes[min(i, len(s.codes)-1)]
	if code != http.StatusOK {
		w.WriteHeader(code)
		return
	}
	json.NewEncoder(w).Encode(Batch{BatchID: "b1", NextCursor: "2", HasMore: true,
		Items: []catalog.Item{{SKU: "A", Name: "A", Price: 1, Currency: "VND", Version: 1}}})
}

func testClient(url string) *Client {
	c := NewClient(url)
	c.BaseDelay = time.Millisecond
	return c
}

// U3
func TestFetchBatchRetriesThenSucceeds(t *testing.T) {
	sc := &script{codes: []int{503, 503, 200}}
	srv := httptest.NewServer(sc)
	defer srv.Close()

	b, err := testClient(srv.URL).FetchBatch(context.Background(), "")
	if err != nil {
		t.Fatalf("FetchBatch: %v", err)
	}
	if b.BatchID != "b1" || len(b.Items) != 1 {
		t.Fatalf("batch = %+v", b)
	}
	if got := sc.calls.Load(); got != 3 {
		t.Fatalf("calls = %d, want 3", got)
	}
}

func TestFetchBatchRetriesRateLimit(t *testing.T) {
	sc := &script{codes: []int{429, 200}}
	srv := httptest.NewServer(sc)
	defer srv.Close()

	if _, err := testClient(srv.URL).FetchBatch(context.Background(), ""); err != nil {
		t.Fatalf("FetchBatch: %v", err)
	}
}

// U4
func TestFetchBatchDoesNotRetryBadRequest(t *testing.T) {
	sc := &script{codes: []int{400}}
	srv := httptest.NewServer(sc)
	defer srv.Close()

	_, err := testClient(srv.URL).FetchBatch(context.Background(), "x")
	var se *StatusError
	if !errors.As(err, &se) || se.Code != 400 {
		t.Fatalf("error = %v, want StatusError 400", err)
	}
	if got := sc.calls.Load(); got != 1 {
		t.Fatalf("calls = %d, want 1", got)
	}
}

// U5
func TestFetchBatchGivesUpAfterMaxAttempts(t *testing.T) {
	sc := &script{codes: []int{503}}
	srv := httptest.NewServer(sc)
	defer srv.Close()

	_, err := testClient(srv.URL).FetchBatch(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "after 5 attempts") {
		t.Fatalf("error = %v, want 'after 5 attempts'", err)
	}
	var se *StatusError
	if !errors.As(err, &se) || se.Code != 503 {
		t.Fatalf("error = %v, want wrapped StatusError 503", err)
	}
	if got := sc.calls.Load(); got != 5 {
		t.Fatalf("calls = %d, want 5", got)
	}
}

func TestFetchBatchRetriesNetworkErrors(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // nothing listens any more

	_, err := testClient(url).FetchBatch(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "after 5 attempts") {
		t.Fatalf("error = %v, want retries exhausted", err)
	}
}

func TestFetchBatchStopsWhenContextCancelled(t *testing.T) {
	sc := &script{codes: []int{503}}
	srv := httptest.NewServer(sc)
	defer srv.Close()

	c := NewClient(srv.URL) // real 1 s backoff
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := c.FetchBatch(ctx, "")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context deadline", err)
	}
	if time.Since(start) > 900*time.Millisecond {
		t.Fatal("FetchBatch kept waiting after the context ended")
	}
}

func TestFetchBatchRejectsUnsafeBatches(t *testing.T) {
	tests := []struct {
		name  string
		batch Batch
		want  string
	}{
		{"no batch id", Batch{NextCursor: "2", HasMore: true}, "without batch_id"},
		{"long batch id", Batch{BatchID: strings.Repeat("b", 65)}, "longer than"},
		{"has_more without cursor", Batch{BatchID: "b1", HasMore: true}, "next_cursor is empty"},
		{"same cursor again", Batch{BatchID: "b1", HasMore: true, NextCursor: "c"}, "same batch"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				json.NewEncoder(w).Encode(tc.batch)
			}))
			defer srv.Close()

			_, err := testClient(srv.URL).FetchBatch(context.Background(), "c")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			if calls.Load() != 1 {
				t.Fatalf("calls = %d, want no retry", calls.Load())
			}
		})
	}
}
