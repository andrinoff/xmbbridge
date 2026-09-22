package feed

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/andrinoff/xmbbridge/internal/config"
	"github.com/andrinoff/xmbbridge/internal/model"
	"github.com/andrinoff/xmbbridge/internal/platform"
	"github.com/andrinoff/xmbbridge/internal/store"
	"github.com/andrinoff/xmbbridge/internal/text"
)

// fakeTarget records the posts the engine asks it to publish.
type fakeTarget struct {
	name   model.Platform
	limits model.Limits

	mu      sync.Mutex
	posts   []model.Outbound
	media   [][]model.PreparedMedia
	failFor int
	calls   int
}

func (f *fakeTarget) Name() model.Platform { return f.name }
func (f *fakeTarget) Limits() model.Limits { return f.limits }

func (f *fakeTarget) Post(_ context.Context, out model.Outbound, media []model.PreparedMedia) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.failFor > 0 && f.calls <= f.failFor {
		return "", errors.New("target is unavailable")
	}
	f.posts = append(f.posts, out)
	f.media = append(f.media, media)
	return string(f.name) + "-" + out.OriginID, nil
}

func (f *fakeTarget) published() []model.Outbound {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]model.Outbound(nil), f.posts...)
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testConfig() *config.Config {
	return &config.Config{
		DedupWindow: config.Duration(5 * time.Minute),
		Platforms: config.PlatformsConfig{
			Mastodon: config.MastodonConfig{Enabled: true, Server: "https://mastodon.test", AccessToken: "t"},
			Bluesky:  config.BlueskyConfig{Enabled: true, Handle: "me.test", AppPassword: "p"},
			Twitter:  config.TwitterConfig{Enabled: true, UserID: "1", BearerToken: "b"},
		},
	}
}

func newTestEngine(t *testing.T, cfg *config.Config, targets ...*fakeTarget) (*Engine, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	adapters := make([]Target, len(targets))
	for i, target := range targets {
		adapters[i] = target
	}
	engine := New(cfg, st, testLogger(), nil, adapters)
	// Keep failure-path tests fast: the production delays are seconds long.
	engine.attempts = 2
	engine.backoff = platform.Backoff{Base: time.Millisecond, Max: 2 * time.Millisecond}
	return engine, st
}

func defaultLimits() model.Limits {
	return model.Limits{TextMax: 300, ImageMaxBytes: 1 << 20, ImageMaxDimension: 2000}
}

func TestEngineBridgesToEveryConfiguredTarget(t *testing.T) {
	cfg := testConfig()
	mastodon := &fakeTarget{name: model.PlatformMastodon, limits: defaultLimits()}
	bluesky := &fakeTarget{name: model.PlatformBluesky, limits: defaultLimits()}
	engine, _ := newTestEngine(t, cfg, mastodon, bluesky)

	engine.handle(context.Background(), model.Post{
		Origin:    model.PlatformTwitter,
		OriginID:  "100",
		Text:      "hello from X",
		CreatedAt: time.Now(),
	})

	if got := len(mastodon.published()); got != 1 {
		t.Fatalf("mastodon received %d posts, want 1", got)
	}
	if got := len(bluesky.published()); got != 1 {
		t.Fatalf("bluesky received %d posts, want 1", got)
	}
	if got := mastodon.published()[0].Text; got != "hello from X" {
		t.Fatalf("mastodon text = %q", got)
	}
}

func TestEngineNeverTargetsTwitter(t *testing.T) {
	cfg := testConfig()
	mastodon := &fakeTarget{name: model.PlatformMastodon, limits: defaultLimits()}
	bluesky := &fakeTarget{name: model.PlatformBluesky, limits: defaultLimits()}
	engine, _ := newTestEngine(t, cfg, mastodon, bluesky)

	routes := engine.Routes()
	for _, target := range routes[model.PlatformTwitter] {
		if target == model.PlatformTwitter {
			t.Fatal("X must never be a bridge target")
		}
	}
	if len(routes[model.PlatformTwitter]) != 2 {
		t.Fatalf("X should fan out to 2 targets, got %v", routes[model.PlatformTwitter])
	}
	if len(routes[model.PlatformMastodon]) != 1 || routes[model.PlatformMastodon][0] != model.PlatformBluesky {
		t.Fatalf("mastodon should only fan out to bluesky, got %v", routes[model.PlatformMastodon])
	}
	if len(routes[model.PlatformBluesky]) != 1 || routes[model.PlatformBluesky][0] != model.PlatformMastodon {
		t.Fatalf("bluesky should only fan out to mastodon, got %v", routes[model.PlatformBluesky])
	}
}

// This is the property that stops a two-way bridge from echoing forever: once
// the bridge has written a post, seeing it again on the target platform must
// not start another round of copying.
func TestEngineIgnoresPostsTheBridgeCreated(t *testing.T) {
	cfg := testConfig()
	mastodon := &fakeTarget{name: model.PlatformMastodon, limits: defaultLimits()}
	bluesky := &fakeTarget{name: model.PlatformBluesky, limits: defaultLimits()}
	engine, st := newTestEngine(t, cfg, mastodon, bluesky)
	ctx := context.Background()

	// Simulate a completed bridge: a Bluesky post that the bridge put on
	// Mastodon.
	if _, err := st.ClaimBridge(ctx, model.PlatformBluesky, "at://did/post/1", model.PlatformMastodon); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := st.CompleteBridge(ctx, model.PlatformBluesky, "at://did/post/1", model.PlatformMastodon, "m-1"); err != nil {
		t.Fatalf("complete: %v", err)
	}

	// The Mastodon listener now sees that bridged copy.
	engine.handle(ctx, model.Post{
		Origin:   model.PlatformMastodon,
		OriginID: "m-1",
		Text:     "hello from X",
	})

	if got := len(bluesky.published()); got != 0 {
		t.Fatalf("the bridged copy was copied back to bluesky (%d posts); the bridge would loop", got)
	}
}

func TestEngineSkipsDuplicateContentWithinWindow(t *testing.T) {
	cfg := testConfig()
	mastodon := &fakeTarget{name: model.PlatformMastodon, limits: defaultLimits()}
	bluesky := &fakeTarget{name: model.PlatformBluesky, limits: defaultLimits()}
	engine, st := newTestEngine(t, cfg, mastodon, bluesky)
	ctx := context.Background()

	const body = "the same words"
	if err := st.MarkSeen(ctx, text.Hash(body)); err != nil {
		t.Fatalf("mark seen: %v", err)
	}

	engine.handle(ctx, model.Post{Origin: model.PlatformTwitter, OriginID: "55", Text: body})

	if got := len(mastodon.published()); got != 0 {
		t.Fatalf("duplicate content was bridged %d times", got)
	}
}

func TestEngineDedupWindowEventuallyExpires(t *testing.T) {
	cfg := testConfig()
	cfg.DedupWindow = config.Duration(0)
	mastodon := &fakeTarget{name: model.PlatformMastodon, limits: defaultLimits()}
	engine, st := newTestEngine(t, cfg, mastodon)
	ctx := context.Background()

	if err := st.MarkSeen(ctx, text.Hash("repeated")); err != nil {
		t.Fatalf("mark seen: %v", err)
	}
	engine.handle(ctx, model.Post{Origin: model.PlatformTwitter, OriginID: "9", Text: "repeated"})

	if got := len(mastodon.published()); got != 1 {
		t.Fatalf("with the window closed the post should be bridged, got %d", got)
	}
}

func TestEngineTruncatesToTargetLimit(t *testing.T) {
	cfg := testConfig()
	bluesky := &fakeTarget{
		name:   model.PlatformBluesky,
		limits: model.Limits{TextMax: 20},
	}
	engine, _ := newTestEngine(t, cfg, bluesky)

	engine.handle(context.Background(), model.Post{
		Origin:   model.PlatformMastodon,
		OriginID: "1",
		Text:     "a body that is far longer than twenty characters",
	})

	published := bluesky.published()
	if len(published) != 1 {
		t.Fatalf("expected one post, got %d", len(published))
	}
	if got := len([]rune(published[0].Text)); got > 20 {
		t.Fatalf("text was not trimmed to the target limit: %d runes", got)
	}
}

func TestEngineThreadsRepliesOntoTheBridgedParent(t *testing.T) {
	cfg := testConfig()
	mastodon := &fakeTarget{name: model.PlatformMastodon, limits: defaultLimits()}
	engine, st := newTestEngine(t, cfg, mastodon)
	ctx := context.Background()

	// The parent was already bridged to Mastodon as status 555.
	if _, err := st.ClaimBridge(ctx, model.PlatformBluesky, "at://did/post/parent", model.PlatformMastodon); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := st.CompleteBridge(ctx, model.PlatformBluesky, "at://did/post/parent", model.PlatformMastodon, "555"); err != nil {
		t.Fatalf("complete: %v", err)
	}

	engine.handle(ctx, model.Post{
		Origin:    model.PlatformBluesky,
		OriginID:  "at://did/post/reply",
		Text:      "a reply",
		IsReply:   true,
		ReplyToID: "at://did/post/parent",
	})

	published := mastodon.published()
	if len(published) != 1 {
		t.Fatalf("expected one post, got %d", len(published))
	}
	if published[0].ParentTargetID != "555" {
		t.Fatalf("reply parent = %q, want 555", published[0].ParentTargetID)
	}
	if !published[0].IsReply {
		t.Fatal("the bridged post should be marked as a reply")
	}
}

func TestEngineLeavesReplyLooseWhenParentIsNotBridged(t *testing.T) {
	cfg := testConfig()
	mastodon := &fakeTarget{name: model.PlatformMastodon, limits: defaultLimits()}
	engine, _ := newTestEngine(t, cfg, mastodon)

	engine.handle(context.Background(), model.Post{
		Origin:    model.PlatformBluesky,
		OriginID:  "at://did/post/reply",
		Text:      "a reply",
		IsReply:   true,
		ReplyToID: "at://did/post/unknown",
	})

	published := mastodon.published()
	if len(published) != 1 {
		t.Fatalf("expected one post, got %d", len(published))
	}
	if published[0].ParentTargetID != "" {
		t.Fatalf("no parent should be resolved, got %q", published[0].ParentTargetID)
	}
}

func TestEngineRetriesAndSucceeds(t *testing.T) {
	cfg := testConfig()
	mastodon := &fakeTarget{name: model.PlatformMastodon, limits: defaultLimits(), failFor: 1}
	engine, _ := newTestEngine(t, cfg, mastodon)

	engine.handle(context.Background(), model.Post{
		Origin:   model.PlatformTwitter,
		OriginID: "7",
		Text:     "flaky but fine",
	})

	if got := len(mastodon.published()); got != 1 {
		t.Fatalf("the retry should have succeeded, got %d posts", got)
	}
}

func TestEngineRecordsPersistentFailure(t *testing.T) {
	cfg := testConfig()
	mastodon := &fakeTarget{name: model.PlatformMastodon, limits: defaultLimits(), failFor: 100}
	engine, st := newTestEngine(t, cfg, mastodon)
	ctx := context.Background()

	engine.handle(ctx, model.Post{Origin: model.PlatformTwitter, OriginID: "8", Text: "never lands"})

	if got := len(mastodon.published()); got != 0 {
		t.Fatalf("expected no published posts, got %d", got)
	}
	// The failure is recorded, and the pair remains retryable rather than
	// being marked as done.
	if _, ok, err := st.TargetID(ctx, model.PlatformTwitter, "8", model.PlatformMastodon); err != nil || ok {
		t.Fatalf("a failed bridge must not look complete (ok=%v err=%v)", ok, err)
	}
	claimed, err := st.ClaimBridge(ctx, model.PlatformTwitter, "8", model.PlatformMastodon)
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if !claimed {
		t.Fatal("a failed bridge should be retryable on a later poll")
	}
}

func TestEngineDoesNotBridgeWhenNoTargets(t *testing.T) {
	cfg := &config.Config{
		DedupWindow: config.Duration(time.Minute),
		Platforms: config.PlatformsConfig{
			Mastodon: config.MastodonConfig{Enabled: true, Server: "https://m", AccessToken: "t"},
		},
	}
	mastodon := &fakeTarget{name: model.PlatformMastodon, limits: defaultLimits()}
	engine, _ := newTestEngine(t, cfg, mastodon)

	// The only enabled platform is the origin itself, so nothing to copy to.
	engine.handle(context.Background(), model.Post{Origin: model.PlatformMastodon, OriginID: "1", Text: "alone"})

	if got := len(mastodon.published()); got != 0 {
		t.Fatalf("a post must not be copied back to its own platform, got %d", got)
	}
}

func TestEngineRunPropagatesListenerFailure(t *testing.T) {
	cfg := testConfig()
	engine, _ := newTestEngine(t, cfg)

	engine.sources = []Source{failingSource{name: model.PlatformMastodon, err: errListenerFailure}}

	err := engine.Run(context.Background())
	if err == nil {
		t.Fatal("a failing listener should stop the engine")
	}
	if !errors.Is(err, errListenerFailure) {
		t.Fatalf("unexpected error: %v", err)
	}
}

type failingSource struct {
	name model.Platform
	err  error
}

func (s failingSource) Name() model.Platform                         { return s.name }
func (s failingSource) Run(context.Context, chan<- model.Post) error { return s.err }

var errListenerFailure = errors.New("credentials rejected")
