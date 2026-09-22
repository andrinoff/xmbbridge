package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andrinoff/xmbbridge/internal/model"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

const fullConfig = `
storage: /tmp/xmbbridge-test/bridge.db
poll_interval: 45s
twitter_poll_interval: 2m
dedup_window: 10m
backfill: true
log_level: debug
media:
  directory: /tmp/xmbbridge-test/media
  image_max_dimension: 1600
  ffmpeg_path: /usr/local/bin/ffmpeg
platforms:
  mastodon:
    enabled: true
    server: https://mastodon.example
    access_token: masto-token
    visibility: unlisted
    include_replies: true
  bluesky:
    enabled: true
    handle: me.example.com
    app_password: bsky-pass
    pds: https://pds.example
    jetstream: wss://jetstream.example/subscribe
  twitter:
    enabled: true
    user_id: "123456"
    bearer_token: bearer-token
routes:
  twitter:
    - mastodon
    - bluesky
`

func TestLoadFullConfig(t *testing.T) {
	cfg, err := Load(writeConfig(t, fullConfig))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Storage != "/tmp/xmbbridge-test/bridge.db" {
		t.Errorf("storage = %q", cfg.Storage)
	}
	if cfg.PollInterval.D() != 45*time.Second {
		t.Errorf("poll interval = %v", cfg.PollInterval.D())
	}
	if cfg.TwitterPollInterval.D() != 2*time.Minute {
		t.Errorf("twitter poll interval = %v", cfg.TwitterPollInterval.D())
	}
	if cfg.DedupWindow.D() != 10*time.Minute {
		t.Errorf("dedup window = %v", cfg.DedupWindow.D())
	}
	if !cfg.Backfill {
		t.Error("backfill should be enabled")
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("log level = %q", cfg.LogLevel)
	}
	if cfg.Media.ImageMaxDimension != 1600 {
		t.Errorf("image max dimension = %d", cfg.Media.ImageMaxDimension)
	}
	if cfg.Media.FFmpegPath != "/usr/local/bin/ffmpeg" {
		t.Errorf("ffmpeg path = %q", cfg.Media.FFmpegPath)
	}
	if cfg.Platforms.Mastodon.Visibility != "unlisted" {
		t.Errorf("visibility = %q", cfg.Platforms.Mastodon.Visibility)
	}
	if !cfg.Platforms.Twitter.UsesOAuth1() {
		// Only a bearer token was given, so OAuth 1.0a is not in play.
	}
	if len(cfg.EnabledPlatforms()) != 3 {
		t.Errorf("enabled platforms = %v", cfg.EnabledPlatforms())
	}
}

func TestDurationAcceptsBareSeconds(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
storage: /tmp/bridge.db
poll_interval: 90
platforms:
  mastodon:
    enabled: true
    server: https://m
    access_token: t
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.PollInterval.D() != 90*time.Second {
		t.Fatalf("bare integer should mean seconds, got %v", cfg.PollInterval.D())
	}
}

func TestDefaultsAreApplied(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
platforms:
  bluesky:
    enabled: true
    handle: me.test
    app_password: pass
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Storage != "data/bridge.db" {
		t.Errorf("storage default = %q", cfg.Storage)
	}
	if cfg.PollInterval.D() != 30*time.Second {
		t.Errorf("poll interval default = %v", cfg.PollInterval.D())
	}
	if cfg.TwitterPollInterval.D() != 90*time.Second {
		t.Errorf("twitter poll interval default = %v", cfg.TwitterPollInterval.D())
	}
	if cfg.DedupWindow.D() != 5*time.Minute {
		t.Errorf("dedup window default = %v", cfg.DedupWindow.D())
	}
	if cfg.Platforms.Bluesky.PDS != "https://bsky.social" {
		t.Errorf("pds default = %q", cfg.Platforms.Bluesky.PDS)
	}
	if !strings.HasPrefix(cfg.Platforms.Bluesky.Jetstream, "wss://") {
		t.Errorf("jetstream default = %q", cfg.Platforms.Bluesky.Jetstream)
	}
	if !cfg.Platforms.Bluesky.Video {
		t.Error("video should default to enabled for bluesky")
	}
	if cfg.Platforms.Bluesky.MaxImageBytes != 2<<20 {
		t.Errorf("bluesky image budget = %d", cfg.Platforms.Bluesky.MaxImageBytes)
	}
	if cfg.Platforms.Bluesky.MaxVideoDuration.D() != 60*time.Second {
		t.Errorf("bluesky video duration cap = %v", cfg.Platforms.Bluesky.MaxVideoDuration.D())
	}
}

func TestEnvironmentOverridesSecrets(t *testing.T) {
	t.Setenv(EnvPrefix+"MASTODON_ACCESS_TOKEN", "from-env")
	t.Setenv(EnvPrefix+"BLUESKY_APP_PASSWORD", "env-pass")
	t.Setenv(EnvPrefix+"TWITTER_BEARER_TOKEN", "env-bearer")
	t.Setenv(EnvPrefix+"TWITTER_USER_ID", "999")

	cfg, err := Load(writeConfig(t, `
platforms:
  mastodon:
    enabled: true
    server: https://m
    access_token: from-file
  bluesky:
    enabled: true
    handle: me.test
    app_password: file-pass
  twitter:
    enabled: true
    user_id: "111"
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Platforms.Mastodon.AccessToken != "from-env" {
		t.Errorf("mastodon token = %q, environment should win", cfg.Platforms.Mastodon.AccessToken)
	}
	if cfg.Platforms.Bluesky.AppPassword != "env-pass" {
		t.Errorf("bluesky password = %q, environment should win", cfg.Platforms.Bluesky.AppPassword)
	}
	if cfg.Platforms.Twitter.BearerToken != "env-bearer" {
		t.Errorf("twitter bearer = %q", cfg.Platforms.Twitter.BearerToken)
	}
	if cfg.Platforms.Twitter.UserID != "999" {
		t.Errorf("twitter user id = %q", cfg.Platforms.Twitter.UserID)
	}
}

func TestValidationFailures(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantSub string
	}{
		{
			name:    "no platforms enabled",
			body:    "storage: /tmp/bridge.db\n",
			wantSub: "no platforms enabled",
		},
		{
			name:    "mastodon without token",
			body:    "platforms:\n  mastodon:\n    enabled: true\n    server: https://m\n",
			wantSub: "access_token is required",
		},
		{
			name:    "mastodon without server",
			body:    "platforms:\n  mastodon:\n    enabled: true\n    access_token: t\n",
			wantSub: "server is required",
		},
		{
			name:    "bluesky without password",
			body:    "platforms:\n  bluesky:\n    enabled: true\n    handle: me.test\n",
			wantSub: "app_password is required",
		},
		{
			name:    "bluesky without handle",
			body:    "platforms:\n  bluesky:\n    enabled: true\n    app_password: p\n",
			wantSub: "handle is required",
		},
		{
			name:    "twitter without user id",
			body:    "platforms:\n  twitter:\n    enabled: true\n    bearer_token: b\n",
			wantSub: "user_id is required",
		},
		{
			name:    "twitter without credentials",
			body:    "platforms:\n  twitter:\n    enabled: true\n    user_id: \"1\"\n",
			wantSub: "bearer_token or a full oauth1",
		},
		{
			name:    "invalid duration",
			body:    "poll_interval: not-a-duration\nplatforms:\n  bluesky:\n    enabled: true\n    handle: h\n    app_password: p\n",
			wantSub: "invalid duration",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tc.body))
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q does not mention %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestMissingFileIsAnError(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "absent.yaml"))
	if err == nil {
		t.Fatal("expected an error for a missing config file")
	}
}

func TestTargetsFor(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
platforms:
  mastodon:
    enabled: true
    server: https://m
    access_token: t
  bluesky:
    enabled: true
    handle: me.test
    app_password: p
  twitter:
    enabled: true
    user_id: "1"
    bearer_token: b
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// X fans out to everything writable.
	if got := cfg.TargetsFor("twitter"); len(got) != 2 {
		t.Fatalf("twitter targets = %v", got)
	}
	// A platform never targets itself, and X is never a target.
	for _, origin := range []model.Platform{model.PlatformMastodon, model.PlatformBluesky} {
		for _, target := range cfg.TargetsFor(origin) {
			if target == origin {
				t.Fatalf("%s should not target itself", origin)
			}
			if target == model.PlatformTwitter {
				t.Fatalf("%s should never target twitter", origin)
			}
		}
	}
}

func TestExplicitRoutesOverrideTheDefault(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
platforms:
  mastodon:
    enabled: true
    server: https://m
    access_token: t
  bluesky:
    enabled: true
    handle: me.test
    app_password: p
  twitter:
    enabled: true
    user_id: "1"
    bearer_token: b
routes:
  mastodon: []
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got := cfg.TargetsFor("mastodon"); len(got) != 0 {
		t.Fatalf("mastodon should have no targets, got %v", got)
	}
	// Unlisted origins keep their default routing.
	if got := cfg.TargetsFor("bluesky"); len(got) != 1 {
		t.Fatalf("bluesky targets = %v", got)
	}
}

func TestRoutesCannotEnableADisabledTarget(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
platforms:
  mastodon:
    enabled: true
    server: https://m
    access_token: t
  twitter:
    enabled: true
    user_id: "1"
    bearer_token: b
routes:
  twitter:
    - mastodon
    - bluesky
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, target := range cfg.TargetsFor("twitter") {
		if string(target) == "bluesky" {
			t.Fatal("a disabled platform must not become a target")
		}
	}
}

func TestWritablePlatformsIsReadOnlyForBearerOnlyX(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
platforms:
  mastodon:
    enabled: true
    server: https://m
    access_token: t
  bluesky:
    enabled: true
    handle: h
    app_password: p
  twitter:
    enabled: true
    user_id: "1"
    bearer_token: b
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, platform := range cfg.WritablePlatforms() {
		if platform == model.PlatformTwitter {
			t.Fatal("bearer auth cannot write, so X must not be a target")
		}
	}
	if len(cfg.WritablePlatforms()) != 2 {
		t.Fatalf("writable platforms = %v", cfg.WritablePlatforms())
	}
	// Reading X still fans out to the two writable platforms.
	if got := cfg.TargetsFor(model.PlatformTwitter); len(got) != 2 {
		t.Fatalf("twitter targets = %v", got)
	}
}

func TestWritablePlatformsIncludesXWithOAuth1(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
platforms:
  mastodon:
    enabled: true
    server: https://m
    access_token: t
  bluesky:
    enabled: true
    handle: h
    app_password: p
  twitter:
    enabled: true
    user_id: "1"
    consumer_key: ck
    consumer_secret: cs
    access_token: at
    access_token_secret: ats
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	found := false
	for _, platform := range cfg.WritablePlatforms() {
		if platform == model.PlatformTwitter {
			found = true
		}
	}
	if !found {
		t.Fatalf("OAuth 1.0a credentials should make X writable, got %v", cfg.WritablePlatforms())
	}

	// Both other platforms now mirror onto X.
	for _, origin := range []model.Platform{model.PlatformMastodon, model.PlatformBluesky} {
		targets := cfg.TargetsFor(origin)
		hasX := false
		for _, target := range targets {
			if target == model.PlatformTwitter {
				hasX = true
			}
			if target == origin {
				t.Fatalf("%s should not target itself", origin)
			}
		}
		if !hasX {
			t.Fatalf("%s should mirror to X, got %v", origin, targets)
		}
	}

	// X does not mirror to itself.
	for _, target := range cfg.TargetsFor(model.PlatformTwitter) {
		if target == model.PlatformTwitter {
			t.Fatal("X must not target itself")
		}
	}
}

func TestPartialOAuth1CredentialsAreAnError(t *testing.T) {
	// Three of the four OAuth 1.0a keys is a common mistake. With no bearer
	// token to fall back on, that is a validation error rather than a silently
	// read-only adapter.
	_, err := Load(writeConfig(t, `
platforms:
  mastodon:
    enabled: true
    server: https://m
    access_token: t
  twitter:
    enabled: true
    user_id: "1"
    consumer_key: ck
    consumer_secret: cs
    access_token: at
`))
	if err == nil {
		t.Fatal("a partial credential set should fail validation")
	}
	if !strings.Contains(err.Error(), "oauth1") {
		t.Fatalf("error %q should name the incomplete credentials", err.Error())
	}
}

func TestPartialOAuth1WithBearerFallsBackToReadOnly(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
platforms:
  mastodon:
    enabled: true
    server: https://m
    access_token: t
  twitter:
    enabled: true
    user_id: "1"
    bearer_token: b
    consumer_key: ck
    consumer_secret: cs
    access_token: at
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Platforms.Twitter.UsesOAuth1() {
		t.Fatal("an incomplete credential set is not usable")
	}
	for _, platform := range cfg.WritablePlatforms() {
		if platform == model.PlatformTwitter {
			t.Fatal("X must stay read-only without a full credential set")
		}
	}
}

func TestTwitterDefaultsMatchTheAPILimits(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
platforms:
  twitter:
    enabled: true
    user_id: "1"
    bearer_token: b
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	limits := cfg.Platforms.Twitter.PlatformLimits
	if limits.MaxImageBytes != 5<<20 {
		t.Errorf("image budget = %d", limits.MaxImageBytes)
	}
	if limits.MaxVideoDuration.D() != 140*time.Second {
		t.Errorf("video duration = %v", limits.MaxVideoDuration.D())
	}
	if !limits.Video {
		t.Error("video should default to enabled for X")
	}
}
