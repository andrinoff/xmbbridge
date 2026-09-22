package mastodon

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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

// fakeMastodon serves the subset of the API the adapter uses and records what
// the bridge posted.
type fakeMastodon struct {
	statuses []map[string]any
	posted   []map[string]string
	mu       sync.Mutex
}

func (f *fakeMastodon) handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/api/v1/accounts/verify_credentials", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"id": "1", "acct": "me"})
	})

	mux.HandleFunc("/api/v1/accounts/1/statuses", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(f.statuses)
	})

	mux.HandleFunc("/api/v2/media", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"id": "media-1", "type": "image"})
	})

	mux.HandleFunc("/api/v1/statuses", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.posted = append(f.posted, map[string]string{
			"status":      r.FormValue("status"),
			"visibility":  r.FormValue("visibility"),
			"media_ids":   r.FormValue("media_ids[]"),
			"in_reply_to": r.FormValue("in_reply_to_id"),
		})
		f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"id": "9001"})
	})

	return mux
}

func (f *fakeMastodon) published() []map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]string(nil), f.posted...)
}

func statusJSON(id, content string) map[string]any {
	return map[string]any{
		"id":         id,
		"content":    content,
		"created_at": "2024-05-01T10:00:00.000Z",
		"language":   "en",
		"url":        "https://mastodon.test/@me/" + id,
	}
}

func TestRunEmitsOnlyStatusesNewerThanTheCursor(t *testing.T) {
	fake := &fakeMastodon{statuses: []map[string]any{
		// The API returns newest first.
		statusJSON("102", "<p>second</p>"),
		statusJSON("101", "<p>first</p>"),
		statusJSON("100", "<p>already seen</p>"),
	}}
	server := httptest.NewServer(fake.handler())
	defer server.Close()

	st := testStore(t)
	// The stored cursor means 100 is already handled.
	if err := st.SetCursor(context.Background(), model.PlatformMastodon, "100"); err != nil {
		t.Fatalf("seed cursor: %v", err)
	}

	adapter, err := New(Options{
		Config:       config.MastodonConfig{Server: server.URL, AccessToken: "test-token", Visibility: "public"},
		PollInterval: 20 * time.Millisecond,
		State:        st,
		Logger:       discardLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	sink := make(chan model.Post, 8)
	done := make(chan struct{})
	go func() {
		defer close(done)
		adapter.Run(ctx, sink)
	}()

	var got []model.Post
	for len(got) < 2 {
		select {
		case post := <-sink:
			got = append(got, post)
		case <-ctx.Done():
			t.Fatalf("timed out; collected %d posts", len(got))
		}
	}

	// Oldest first, so a thread is bridged in reading order.
	if got[0].Text != "first" || got[1].Text != "second" {
		t.Fatalf("emitted %q then %q, want oldest first", got[0].Text, got[1].Text)
	}
	// The already-seen status must not be re-emitted.
	for _, post := range got {
		if post.OriginID == "100" {
			t.Fatal("a status at or below the cursor must not be emitted")
		}
	}
	if got[0].Origin != model.PlatformMastodon || got[0].Lang != "en" {
		t.Fatalf("mapped post = %+v", got[0])
	}

	// The cursor advances to the newest status seen.
	cancel()
	<-done
	cursor, err := st.Cursor(context.Background(), model.PlatformMastodon)
	if err != nil {
		t.Fatalf("read cursor: %v", err)
	}
	if cursor != "102" {
		t.Fatalf("cursor = %q, want 102", cursor)
	}
}

func TestRunSeedsTheCursorWithoutReplayingHistory(t *testing.T) {
	fake := &fakeMastodon{statuses: []map[string]any{
		statusJSON("500", "<p>old news</p>"),
	}}
	server := httptest.NewServer(fake.handler())
	defer server.Close()

	st := testStore(t)
	adapter, err := New(Options{
		Config:       config.MastodonConfig{Server: server.URL, AccessToken: "test-token"},
		PollInterval: 20 * time.Millisecond,
		State:        st,
		Logger:       discardLogger(),
		// Backfill is off by default.
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	sink := make(chan model.Post, 4)
	go adapter.Run(ctx, sink)

	// Wait for the cursor to be seeded.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		cursor, err := st.Cursor(context.Background(), model.PlatformMastodon)
		if err != nil {
			t.Fatalf("read cursor: %v", err)
		}
		if cursor == "500" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	select {
	case post := <-sink:
		t.Fatalf("history should not be bridged on first run, got %q", post.OriginID)
	default:
	}
}

func TestRunSkipsBoostsAndRepliesWhenNotWanted(t *testing.T) {
	boost := map[string]any{
		"id":         "201",
		"content":    "",
		"created_at": "2024-05-01T10:00:00.000Z",
		"reblog":     statusJSON("999", "<p>somebody else</p>"),
	}
	reply := map[string]any{
		"id":             "202",
		"content":        "<p>a reply</p>",
		"created_at":     "2024-05-01T10:00:00.000Z",
		"in_reply_to_id": "201",
	}
	plain := statusJSON("203", "<p>a real post</p>")

	fake := &fakeMastodon{statuses: []map[string]any{plain, reply, boost}}
	server := httptest.NewServer(fake.handler())
	defer server.Close()

	st := testStore(t)
	if err := st.SetCursor(context.Background(), model.PlatformMastodon, "200"); err != nil {
		t.Fatalf("seed cursor: %v", err)
	}

	adapter, err := New(Options{
		Config:       config.MastodonConfig{Server: server.URL, AccessToken: "test-token", IncludeReplies: false},
		PollInterval: 20 * time.Millisecond,
		State:        st,
		Logger:       discardLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	sink := make(chan model.Post, 8)
	go adapter.Run(ctx, sink)

	select {
	case post := <-sink:
		if post.OriginID != "203" {
			t.Fatalf("only the plain post should be emitted, got %q", post.OriginID)
		}
	case <-ctx.Done():
		t.Fatal("the plain post was never emitted")
	}

	select {
	case post := <-sink:
		t.Fatalf("unexpected extra post %q", post.OriginID)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestPostSendsMediaAndReplyTarget(t *testing.T) {
	fake := &fakeMastodon{}
	server := httptest.NewServer(fake.handler())
	defer server.Close()

	adapter, err := New(Options{
		Config: config.MastodonConfig{
			Server:      server.URL,
			AccessToken: "test-token",
			Visibility:  "unlisted",
		},
		State:  testStore(t),
		Logger: discardLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	id, err := adapter.Post(context.Background(), model.Outbound{
		Origin:         model.PlatformBluesky,
		OriginID:       "at://did/post/1",
		Text:           "bridged text",
		Lang:           "en",
		IsReply:        true,
		ParentTargetID: "555",
	}, []model.PreparedMedia{
		{Kind: model.MediaImage, Bytes: []byte("fake-image-bytes"), MimeType: "image/jpeg", AltText: "a cat", Filename: "a.jpg"},
	})
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if id != "9001" {
		t.Fatalf("id = %q", id)
	}

	published := fake.published()
	if len(published) != 1 {
		t.Fatalf("got %d posted statuses, want 1", len(published))
	}
	if published[0]["status"] != "bridged text" {
		t.Fatalf("status text = %q", published[0]["status"])
	}
	if published[0]["visibility"] != "unlisted" {
		t.Fatalf("visibility = %q", published[0]["visibility"])
	}
	if published[0]["media_ids"] != "media-1" {
		t.Fatalf("media id = %q, the uploaded attachment was not attached", published[0]["media_ids"])
	}
	if published[0]["in_reply_to"] != "555" {
		t.Fatalf("in_reply_to_id = %q, want the bridged parent", published[0]["in_reply_to"])
	}
}

func TestNewRejectsIncompleteConfig(t *testing.T) {
	if _, err := New(Options{Config: config.MastodonConfig{AccessToken: "t"}}); err == nil {
		t.Fatal("a server is required")
	}
	if _, err := New(Options{Config: config.MastodonConfig{Server: "https://m"}}); err == nil {
		t.Fatal("an access token is required")
	}
}
