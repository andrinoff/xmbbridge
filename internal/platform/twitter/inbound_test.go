package twitter

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andrinoff/xmbbridge/internal/config"
	"github.com/andrinoff/xmbbridge/internal/model"
	"github.com/andrinoff/xmbbridge/internal/store"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// fakeX serves the user tweet timeline endpoint and records the requests.
type fakeX struct {
	mu       sync.Mutex
	requests []string
	body     string
	rateHdr  map[string]string
}

func (f *fakeX) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, r.URL.RawQuery)
		body := f.body
		headers := f.rateHdr
		f.mu.Unlock()

		if got := r.Header.Get("Authorization"); got != "Bearer test-bearer" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		for key, value := range headers {
			w.Header().Set(key, value)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body)
	})
}

func timelineBody(tweets string) string {
	return `{"data":[` + tweets + `],"meta":{"result_count":1,"newest_id":"200","oldest_id":"200"}}`
}

func (f *fakeX) lastQuery() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		return ""
	}
	return f.requests[len(f.requests)-1]
}

func newTestAdapter(t *testing.T, server *httptest.Server, st *store.Store, cfg config.TwitterConfig) *Adapter {
	t.Helper()
	adapter, err := New(Options{
		Config:       cfg,
		PollInterval: 20 * time.Millisecond,
		State:        st,
		Logger:       discardLogger(),
		APIHost:      server.URL,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return adapter
}

func bearerTestConfig() config.TwitterConfig {
	return config.TwitterConfig{UserID: "123", BearerToken: "test-bearer"}
}

func TestPollEmitsNewTweetsOldestFirst(t *testing.T) {
	fake := &fakeX{body: `{"data":[
		{"id":"202","text":"second","created_at":"2024-05-01T10:00:02.000Z","lang":"en"},
		{"id":"201","text":"first","created_at":"2024-05-01T10:00:01.000Z","lang":"en"},
		{"id":"200","text":"already seen","created_at":"2024-05-01T10:00:00.000Z","lang":"en"}
	],"meta":{"result_count":3,"newest_id":"202","oldest_id":"200"}}`}
	server := httptest.NewServer(fake.handler())
	defer server.Close()

	st := testStore(t)
	if err := st.SetCursor(context.Background(), model.PlatformTwitter, "200"); err != nil {
		t.Fatalf("seed cursor: %v", err)
	}

	adapter := newTestAdapter(t, server, st, bearerTestConfig())
	sink := make(chan model.Post, 8)

	if _, err := adapter.poll(context.Background(), sink); err != nil {
		t.Fatalf("poll: %v", err)
	}

	close(sink)
	var got []model.Post
	for post := range sink {
		got = append(got, post)
	}
	if len(got) != 2 {
		t.Fatalf("emitted %d tweets, want 2", len(got))
	}
	// Oldest first so a thread is copied in reading order.
	if got[0].Text != "first" || got[1].Text != "second" {
		t.Fatalf("order = %q then %q", got[0].Text, got[1].Text)
	}
	for _, post := range got {
		if post.OriginID == "200" {
			t.Fatal("a tweet at or below the cursor must not be emitted")
		}
	}

	// The request must carry the stored cursor so X does the filtering.
	if q := fake.lastQuery(); q == "" || !contains(q, "since_id=200") {
		t.Fatalf("query = %q, want a since_id cursor", q)
	}
	// Media expansion is what makes attachments resolvable.
	if q := fake.lastQuery(); !contains(q, "attachments.media_keys") {
		t.Fatalf("query = %q, want media expansions", q)
	}

	cursor, err := st.Cursor(context.Background(), model.PlatformTwitter)
	if err != nil {
		t.Fatalf("read cursor: %v", err)
	}
	if cursor != "202" {
		t.Fatalf("cursor = %q, want 202", cursor)
	}
}

func TestPollResolvesMedia(t *testing.T) {
	fake := &fakeX{body: `{
		"data":[{"id":"300","text":"with a photo","created_at":"2024-05-01T10:00:00.000Z",
		         "attachments":{"media_keys":["3_photo"]}}],
		"includes":{"media":[{"media_key":"3_photo","type":"photo",
		                      "url":"https://pbs.twimg.com/media/abc.jpg",
		                      "alt_text":"a cat","width":1200,"height":800}]},
		"meta":{"result_count":1,"newest_id":"300","oldest_id":"300"}
	}`}
	server := httptest.NewServer(fake.handler())
	defer server.Close()

	st := testStore(t)
	// The first poll would only seed the cursor; give it one so the tweet
	// flows through to the emit path.
	if err := st.SetCursor(context.Background(), model.PlatformTwitter, "299"); err != nil {
		t.Fatalf("seed cursor: %v", err)
	}
	adapter := newTestAdapter(t, server, st, bearerTestConfig())
	sink := make(chan model.Post, 4)

	if _, err := adapter.poll(context.Background(), sink); err != nil {
		t.Fatalf("poll: %v", err)
	}
	close(sink)

	post, ok := <-sink
	if !ok {
		t.Fatal("no post emitted")
	}
	if len(post.Media) != 1 {
		t.Fatalf("got %d attachments, want 1", len(post.Media))
	}
	if post.Media[0].URL != "https://pbs.twimg.com/media/abc.jpg" {
		t.Fatalf("media url = %q", post.Media[0].URL)
	}
	if post.Media[0].AltText != "a cat" {
		t.Fatalf("alt = %q", post.Media[0].AltText)
	}
}

func TestPollSeedsCursorWithoutReplayingHistory(t *testing.T) {
	fake := &fakeX{body: timelineBody(`{"id":"900","text":"old news","created_at":"2024-05-01T10:00:00.000Z"}`)}
	server := httptest.NewServer(fake.handler())
	defer server.Close()

	st := testStore(t)
	adapter := newTestAdapter(t, server, st, bearerTestConfig())
	sink := make(chan model.Post, 4)

	if _, err := adapter.poll(context.Background(), sink); err != nil {
		t.Fatalf("poll: %v", err)
	}

	select {
	case post := <-sink:
		t.Fatalf("history should not be bridged on first run, got %q", post.OriginID)
	default:
	}
	cursor, err := st.Cursor(context.Background(), model.PlatformTwitter)
	if err != nil {
		t.Fatalf("read cursor: %v", err)
	}
	if cursor != "900" {
		t.Fatalf("cursor = %q, want the newest tweet", cursor)
	}

	// The next poll must send that cursor so X only returns newer tweets.
	fake.mu.Lock()
	fake.requests = nil
	fake.mu.Unlock()
	if _, err := adapter.poll(context.Background(), sink); err != nil {
		t.Fatalf("second poll: %v", err)
	}
	if q := fake.lastQuery(); !contains(q, "since_id=900") {
		t.Fatalf("second query = %q, want since_id=900", q)
	}
}

func TestPollSkipsRepliesUnlessConfigured(t *testing.T) {
	fake := &fakeX{body: `{"data":[
		{"id":"401","text":"a reply","created_at":"2024-05-01T10:00:01.000Z","in_reply_to_user_id":"123"},
		{"id":"400","text":"a post","created_at":"2024-05-01T10:00:00.000Z"}
	],"meta":{"result_count":2,"newest_id":"401","oldest_id":"400"}}`}
	server := httptest.NewServer(fake.handler())
	defer server.Close()

	st := testStore(t)
	if err := st.SetCursor(context.Background(), model.PlatformTwitter, "399"); err != nil {
		t.Fatalf("seed cursor: %v", err)
	}

	adapter := newTestAdapter(t, server, st, bearerTestConfig())
	sink := make(chan model.Post, 4)
	if _, err := adapter.poll(context.Background(), sink); err != nil {
		t.Fatalf("poll: %v", err)
	}
	close(sink)

	var got []model.Post
	for post := range sink {
		got = append(got, post)
	}
	if len(got) != 1 || got[0].OriginID != "400" {
		t.Fatalf("only the top-level tweet should be emitted, got %+v", got)
	}
}

func TestPollParksUntilTheRateLimitResets(t *testing.T) {
	fake := &fakeX{
		body: timelineBody(`{"id":"500","text":"hi","created_at":"2024-05-01T10:00:00.000Z"}`),
		rateHdr: map[string]string{
			"x-rate-limit-limit":     "25",
			"x-rate-limit-remaining": "0",
			"x-rate-limit-reset":     resetHeader(90 * time.Second),
		},
	}
	server := httptest.NewServer(fake.handler())
	defer server.Close()

	st := testStore(t)
	adapter := newTestAdapter(t, server, st, bearerTestConfig())
	sink := make(chan model.Post, 4)

	wait, err := adapter.poll(context.Background(), sink)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	// With the window exhausted the adapter asks the poll loop to wait for the
	// reset instead of immediately burning the next window.
	if wait <= 0 {
		t.Fatal("an exhausted rate limit should produce a wait")
	}
	if wait > 100*time.Second {
		t.Fatalf("wait = %v, far beyond the reset window", wait)
	}
}

func TestPollSurfacesAPIErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"title":"Unauthorized"}`, http.StatusUnauthorized)
	}))
	defer server.Close()

	st := testStore(t)
	adapter := newTestAdapter(t, server, st, bearerTestConfig())
	sink := make(chan model.Post, 4)

	if _, err := adapter.poll(context.Background(), sink); err == nil {
		t.Fatal("expected an error for a rejected credential")
	}
}

func TestRunStopsWithTheContext(t *testing.T) {
	fake := &fakeX{body: `{"data":[],"meta":{"result_count":0}}`}
	server := httptest.NewServer(fake.handler())
	defer server.Close()

	st := testStore(t)
	if err := st.SetCursor(context.Background(), model.PlatformTwitter, "1"); err != nil {
		t.Fatalf("seed cursor: %v", err)
	}
	adapter := newTestAdapter(t, server, st, bearerTestConfig())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		adapter.Run(ctx, make(chan model.Post, 4))
	}()

	time.Sleep(60 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not stop after the context was cancelled")
	}
}

func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }

func resetHeader(d time.Duration) string {
	return strconv.FormatInt(time.Now().Add(d).Unix(), 10)
}
