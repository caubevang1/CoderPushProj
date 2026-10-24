package supplier

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
)

func loadFeed(t *testing.T, name string) []FeedBatch {
	t.Helper()
	feed, err := LoadFeed("../../testdata/feeds/" + name)
	if err != nil {
		t.Fatalf("LoadFeed: %v", err)
	}
	return feed
}

func TestFakeServesWholeFeedThroughClient(t *testing.T) {
	feed := loadFeed(t, "initial")
	srv := httptest.NewServer(NewFake(feed, FakeOptions{}))
	defer srv.Close()
	c := testClient(srv.URL)

	var ids []string
	cursor := ""
	for {
		b, err := c.FetchBatch(context.Background(), cursor)
		if err != nil {
			t.Fatalf("FetchBatch(%q): %v", cursor, err)
		}
		ids = append(ids, b.BatchID)
		if !b.HasMore {
			break
		}
		cursor = b.NextCursor
	}
	want := []string{"initial-001", "initial-002", "initial-003"}
	if len(ids) != len(want) {
		t.Fatalf("batches = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("batches = %v, want %v", ids, want)
		}
	}
}

func TestFakeSameCursorSameBatch(t *testing.T) {
	srv := httptest.NewServer(NewFake(loadFeed(t, "initial"), FakeOptions{}))
	defer srv.Close()
	c := testClient(srv.URL)

	a, err := c.FetchBatch(context.Background(), "2")
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.FetchBatch(context.Background(), "2")
	if err != nil {
		t.Fatal(err)
	}
	if a.BatchID != b.BatchID || len(a.Items) != len(b.Items) {
		t.Fatalf("cursor 2 gave %s then %s", a.BatchID, b.BatchID)
	}
}

func TestFakeRejectsInvalidCursor(t *testing.T) {
	srv := httptest.NewServer(NewFake(loadFeed(t, "initial"), FakeOptions{}))
	defer srv.Close()

	for _, cursor := range []string{"abc", "0", "99"} {
		_, err := testClient(srv.URL).FetchBatch(context.Background(), cursor)
		var se *StatusError
		if !errors.As(err, &se) || se.Code != 400 {
			t.Fatalf("cursor %q: error = %v, want 400", cursor, err)
		}
	}
}

func TestFakeFailAtRecoversAfterFailTimes(t *testing.T) {
	srv := httptest.NewServer(NewFake(loadFeed(t, "initial"), FakeOptions{FailAt: 2, FailTimes: 2}))
	defer srv.Close()

	c := testClient(srv.URL)
	c.MaxAttempts = 1
	if _, err := c.FetchBatch(context.Background(), ""); err != nil {
		t.Fatalf("batch 1 should not fail: %v", err)
	}
	for i := range 2 {
		if _, err := c.FetchBatch(context.Background(), "2"); err == nil {
			t.Fatalf("try %d of batch 2 should fail", i+1)
		}
	}
	if _, err := c.FetchBatch(context.Background(), "2"); err != nil {
		t.Fatalf("batch 2 should have recovered: %v", err)
	}
}

func TestFakeFailAtForever(t *testing.T) {
	srv := httptest.NewServer(NewFake(loadFeed(t, "initial"), FakeOptions{FailAt: 3}))
	defer srv.Close()

	_, err := testClient(srv.URL).FetchBatch(context.Background(), "3")
	var se *StatusError
	if !errors.As(err, &se) || se.Code != 503 {
		t.Fatalf("error = %v, want 503 after retries", err)
	}
}
