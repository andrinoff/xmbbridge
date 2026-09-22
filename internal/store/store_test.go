package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/andrinoff/xmbbridge/internal/model"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestCursorRoundTrip(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	value, err := st.Cursor(ctx, model.PlatformMastodon)
	if err != nil {
		t.Fatalf("read empty cursor: %v", err)
	}
	if value != "" {
		t.Fatalf("fresh store should have no cursor, got %q", value)
	}

	if err := st.SetCursor(ctx, model.PlatformMastodon, "12345"); err != nil {
		t.Fatalf("set cursor: %v", err)
	}
	value, err = st.Cursor(ctx, model.PlatformMastodon)
	if err != nil {
		t.Fatalf("read cursor: %v", err)
	}
	if value != "12345" {
		t.Fatalf("cursor = %q, want 12345", value)
	}

	if err := st.SetCursor(ctx, model.PlatformMastodon, "67890"); err != nil {
		t.Fatalf("overwrite cursor: %v", err)
	}
	if value, _ = st.Cursor(ctx, model.PlatformMastodon); value != "67890" {
		t.Fatalf("cursor = %q, want 67890", value)
	}
}

func TestClaimBridgeIsOncePerPair(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	claimed, err := st.ClaimBridge(ctx, model.PlatformTwitter, "1000", model.PlatformMastodon)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if !claimed {
		t.Fatal("first claim should succeed")
	}

	claimed, err = st.ClaimBridge(ctx, model.PlatformTwitter, "1000", model.PlatformMastodon)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if claimed {
		t.Fatal("claiming the same pair again must be a no-op")
	}

	// The same origin post to a different target is a different pair.
	claimed, err = st.ClaimBridge(ctx, model.PlatformTwitter, "1000", model.PlatformBluesky)
	if err != nil {
		t.Fatalf("claim other target: %v", err)
	}
	if !claimed {
		t.Fatal("claiming a different target should succeed")
	}
}

func TestFailedBridgeCanRetried(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	if _, err := st.ClaimBridge(ctx, model.PlatformTwitter, "1", model.PlatformMastodon); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := st.FailBridge(ctx, model.PlatformTwitter, "1", model.PlatformMastodon, errors.New("boom")); err != nil {
		t.Fatalf("fail: %v", err)
	}

	// A failed attempt clears the way for a retry.
	claimed, err := st.ClaimBridge(ctx, model.PlatformTwitter, "1", model.PlatformMastodon)
	if err != nil {
		t.Fatalf("retry claim: %v", err)
	}
	if !claimed {
		t.Fatal("a failed bridge should be retryable")
	}
}

func TestSuccessfulBridgeBlocksRetriesForever(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	if _, err := st.ClaimBridge(ctx, model.PlatformBluesky, "at://did/post/1", model.PlatformMastodon); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := st.CompleteBridge(ctx, model.PlatformBluesky, "at://did/post/1", model.PlatformMastodon, "999"); err != nil {
		t.Fatalf("complete: %v", err)
	}

	claimed, err := st.ClaimBridge(ctx, model.PlatformBluesky, "at://did/post/1", model.PlatformMastodon)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed {
		t.Fatal("a completed bridge must never be retried")
	}
}

func TestIsBridgedTarget(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	if _, err := st.ClaimBridge(ctx, model.PlatformTwitter, "42", model.PlatformMastodon); err != nil {
		t.Fatalf("claim: %v", err)
	}
	// Before the target id is recorded the post is still invisible.
	ours, err := st.IsBridgedTarget(ctx, model.PlatformMastodon, "777")
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if ours {
		t.Fatal("unrelated post must not be treated as ours")
	}

	if err := st.CompleteBridge(ctx, model.PlatformTwitter, "42", model.PlatformMastodon, "777"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	ours, err = st.IsBridgedTarget(ctx, model.PlatformMastodon, "777")
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !ours {
		t.Fatal("the post the bridge created must be recognised as ours")
	}
}

func TestTargetID(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	if _, err := st.ClaimBridge(ctx, model.PlatformBluesky, "at://did/post/parent", model.PlatformMastodon); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := st.CompleteBridge(ctx, model.PlatformBluesky, "at://did/post/parent", model.PlatformMastodon, "555"); err != nil {
		t.Fatalf("complete: %v", err)
	}

	targetID, ok, err := st.TargetID(ctx, model.PlatformBluesky, "at://did/post/parent", model.PlatformMastodon)
	if err != nil || !ok {
		t.Fatalf("lookup = %v, %v, %v", targetID, ok, err)
	}
	if targetID != "555" {
		t.Fatalf("target id = %q, want 555", targetID)
	}

	// Unknown posts and other targets report not-ok rather than an error.
	_, ok, err = st.TargetID(ctx, model.PlatformBluesky, "at://did/post/other", model.PlatformMastodon)
	if err != nil || ok {
		t.Fatalf("unknown parent should be (false, nil), got (%v, %v)", ok, err)
	}
	_, ok, err = st.TargetID(ctx, model.PlatformBluesky, "at://did/post/parent", model.PlatformBluesky)
	if err != nil || ok {
		t.Fatalf("self target should be (false, nil), got (%v, %v)", ok, err)
	}
}

func TestSeenRecentlyWindow(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	seen, err := st.SeenRecently(ctx, "hash-1", time.Minute)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if seen {
		t.Fatal("nothing has been marked seen yet")
	}

	if err := st.MarkSeen(ctx, "hash-1"); err != nil {
		t.Fatalf("mark: %v", err)
	}
	seen, err = st.SeenRecently(ctx, "hash-1", time.Minute)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !seen {
		t.Fatal("the hash was just marked seen")
	}

	seen, err = st.SeenRecently(ctx, "hash-1", 0)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if seen {
		t.Fatal("a zero window must treat everything as old")
	}
}

func TestPruneSeen(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	if err := st.MarkSeen(ctx, "old"); err != nil {
		t.Fatalf("mark: %v", err)
	}
	// Age the entry out by pruning a negative offset from the far past.
	if _, err := st.db.Exec(`UPDATE seen SET created_at = ? WHERE hash = ?`,
		time.Now().Add(-time.Hour).Format(time.RFC3339Nano), "old"); err != nil {
		t.Fatalf("age entry: %v", err)
	}
	if err := st.PruneSeen(ctx, 30*time.Minute); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if seen, _ := st.SeenRecently(ctx, "old", time.Minute); seen {
		t.Fatal("the pruned entry must be gone")
	}
}
