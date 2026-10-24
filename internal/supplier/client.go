package supplier

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

const maxBatchIDLen = 64 // applied_batches.batch_id VARCHAR(64)

// StatusError is a non-200 answer from the supplier.
type StatusError struct {
	Code int
	Body string
}

func (e *StatusError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("supplier returned HTTP %d", e.Code)
	}
	return fmt.Sprintf("supplier returned HTTP %d: %s", e.Code, e.Body)
}

// retryable reports whether trying again may help: server errors and
// rate limiting yes, other client errors (bad cursor, ...) no.
func (e *StatusError) retryable() bool {
	return e.Code >= 500 || e.Code == http.StatusTooManyRequests
}

// permanentError marks a failure that retrying cannot fix.
type permanentError struct{ err error }

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

// Client fetches batches from the supplier, retrying temporary failures.
type Client struct {
	BaseURL     string
	HTTP        *http.Client
	MaxAttempts int           // total tries per batch, including the first
	BaseDelay   time.Duration // wait before the 2nd try; doubles after each failure
}

// NewClient returns a client with the defaults: 10 s timeout per request,
// 5 attempts, waiting 1 s, 2 s, 4 s and 8 s (plus jitter) between them.
func NewClient(baseURL string) *Client {
	return &Client{
		BaseURL:     strings.TrimRight(baseURL, "/"),
		HTTP:        &http.Client{Timeout: 10 * time.Second},
		MaxAttempts: 5,
		BaseDelay:   time.Second,
	}
}

// FetchBatch returns the batch at cursor. It retries network errors, 5xx
// and 429 with exponential backoff, and gives up at once on other errors.
func (c *Client) FetchBatch(ctx context.Context, cursor string) (Batch, error) {
	var lastErr error
	delay := c.BaseDelay
	for attempt := 1; attempt <= c.MaxAttempts; attempt++ {
		if attempt > 1 {
			if err := sleep(ctx, withJitter(delay)); err != nil {
				return Batch{}, err
			}
			delay *= 2
		}

		b, err := c.fetchOnce(ctx, cursor)
		if err == nil {
			return b, nil
		}
		if ctx.Err() != nil {
			return Batch{}, ctx.Err()
		}
		var perm *permanentError
		if errors.As(err, &perm) {
			return Batch{}, perm.err
		}
		lastErr = err
	}
	return Batch{}, fmt.Errorf("supplier unavailable after %d attempts: %w", c.MaxAttempts, lastErr)
}

func (c *Client) fetchOnce(ctx context.Context, cursor string) (Batch, error) {
	u := c.BaseURL + "/batches?cursor=" + url.QueryEscape(cursor)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Batch{}, &permanentError{fmt.Errorf("build request: %w", err)}
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Batch{}, err // network error or timeout: retry
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		se := &StatusError{Code: resp.StatusCode, Body: strings.TrimSpace(string(body))}
		if se.retryable() {
			return Batch{}, se
		}
		return Batch{}, &permanentError{se}
	}

	var b Batch
	if err := json.NewDecoder(resp.Body).Decode(&b); err != nil {
		// Possibly a response cut off mid-way: worth another try.
		return Batch{}, fmt.Errorf("decode batch: %w", err)
	}
	if err := checkBatch(b, cursor); err != nil {
		return Batch{}, &permanentError{err}
	}
	return b, nil
}

// checkBatch rejects answers that would corrupt the import: an empty
// next_cursor with has_more would restart the feed from the beginning,
// and the same cursor again would loop forever.
func checkBatch(b Batch, cursor string) error {
	switch {
	case b.BatchID == "":
		return errors.New("supplier sent a batch without batch_id")
	case utf8.RuneCountInString(b.BatchID) > maxBatchIDLen:
		return fmt.Errorf("supplier sent a batch_id longer than %d characters", maxBatchIDLen)
	case b.HasMore && b.NextCursor == "":
		return fmt.Errorf("batch %s: has_more is true but next_cursor is empty", b.BatchID)
	case b.HasMore && b.NextCursor == cursor:
		return fmt.Errorf("batch %s: next_cursor %q points back to the same batch", b.BatchID, cursor)
	}
	return nil
}

// withJitter adds up to 20% random extra wait so many clients do not
// retry in lockstep.
func withJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return d + rand.N(d/5+1)
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
