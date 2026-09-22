// Package config loads and validates the bridge configuration.
//
// Configuration lives in a YAML file, but any secret can be supplied through
// the environment so that credentials never have to touch disk. Environment
// values always win over the file.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/andrinoff/xmbbridge/internal/model"
	"gopkg.in/yaml.v3"
)

// EnvPrefix is prepended to every environment variable the bridge reads.
const EnvPrefix = "XMBBRIDGE_"

// Duration is a time.Duration that unmarshals from YAML strings such as "30s"
// and from bare integers, which are interpreted as seconds.
type Duration time.Duration

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var raw string
	if err := node.Decode(&raw); err != nil {
		return err
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		*d = 0
		return nil
	}
	if secs, err := strconv.Atoi(raw); err == nil {
		*d = Duration(time.Duration(secs) * time.Second)
		return nil
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", raw, err)
	}
	*d = Duration(parsed)
	return nil
}

// D returns the value as a time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// Config is the fully resolved bridge configuration.
type Config struct {
	Storage             string   `yaml:"storage"`
	PollInterval        Duration `yaml:"poll_interval"`
	TwitterPollInterval Duration `yaml:"twitter_poll_interval"`
	DedupWindow         Duration `yaml:"dedup_window"`
	LogLevel            string   `yaml:"log_level"`

	// Backfill bridges posts that already existed when the bridge first
	// started watching an account. Off by default so a fresh install does
	// not replay an account's entire history.
	Backfill bool `yaml:"backfill"`

	Media     MediaConfig     `yaml:"media"`
	Platforms PlatformsConfig `yaml:"platforms"`

	// Routes optionally overrides which platforms a post from a given origin
	// is copied to. When empty, every enabled platform except the origin (and
	// except read-only platforms) is a target.
	Routes map[string][]string `yaml:"routes"`
}

// MediaConfig holds settings shared by the media pipeline.
type MediaConfig struct {
	Directory         string `yaml:"directory"`
	ImageMaxDimension int    `yaml:"image_max_dimension"`
	FFmpegPath        string `yaml:"ffmpeg_path"`
}

// PlatformsConfig groups the per-platform sections.
type PlatformsConfig struct {
	Mastodon MastodonConfig `yaml:"mastodon"`
	Bluesky  BlueskyConfig  `yaml:"bluesky"`
	Twitter  TwitterConfig  `yaml:"twitter"`
}

// PlatformLimits is embedded in each platform config so operators can tune the
// outbound media budget per platform.
type PlatformLimits struct {
	MaxImageBytes    int      `yaml:"max_image_bytes"`
	MaxVideoBytes    int      `yaml:"max_video_bytes"`
	MaxImageDim      int      `yaml:"max_image_dimension"`
	MaxVideoDuration Duration `yaml:"max_video_duration"`
	Video            bool     `yaml:"video"`
}

// MastodonConfig configures the Mastodon adapter.
type MastodonConfig struct {
	Enabled        bool   `yaml:"enabled"`
	Server         string `yaml:"server"`
	ClientID       string `yaml:"client_id"`
	ClientSecret   string `yaml:"client_secret"`
	AccessToken    string `yaml:"access_token"`
	Visibility     string `yaml:"visibility"`
	IncludeReplies bool   `yaml:"include_replies"`
	PlatformLimits `yaml:",inline"`
}

// BlueskyConfig configures the Bluesky adapter.
type BlueskyConfig struct {
	Enabled        bool   `yaml:"enabled"`
	Handle         string `yaml:"handle"`
	AppPassword    string `yaml:"app_password"`
	PDS            string `yaml:"pds"`
	Jetstream      string `yaml:"jetstream"`
	IncludeReplies bool   `yaml:"include_replies"`
	PlatformLimits `yaml:",inline"`
}

// TwitterConfig configures the X adapter. Reading works with either an
// app-only bearer token or OAuth 1.0a credentials; posting requires the full
// OAuth 1.0a set because the write endpoints only accept user context.
type TwitterConfig struct {
	Enabled           bool   `yaml:"enabled"`
	UserID            string `yaml:"user_id"`
	BearerToken       string `yaml:"bearer_token"`
	ConsumerKey       string `yaml:"consumer_key"`
	ConsumerSecret    string `yaml:"consumer_secret"`
	AccessToken       string `yaml:"access_token"`
	AccessTokenSecret string `yaml:"access_token_secret"`
	IncludeReplies    bool   `yaml:"include_replies"`
	PlatformLimits    `yaml:",inline"`
}

// UsesOAuth1 reports whether X credentials are configured for OAuth 1.0a
// user-context signing rather than app-only bearer auth.
func (c TwitterConfig) UsesOAuth1() bool {
	return c.ConsumerKey != "" && c.ConsumerSecret != "" &&
		c.AccessToken != "" && c.AccessTokenSecret != ""
}

// Load reads configuration from path. An empty path is allowed and yields the
// built-in defaults, which is useful for containerised deployments that supply
// everything through the environment.
func Load(path string) (*Config, error) {
	cfg := &Config{}
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config %s: %w", path, err)
		}
		if err := yaml.Unmarshal(raw, cfg); err != nil {
			return nil, fmt.Errorf("parse config %s: %w", path, err)
		}
	}
	cfg.applyDefaults()
	cfg.applyEnv()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Storage == "" {
		c.Storage = "data/bridge.db"
	}
	if c.PollInterval == 0 {
		c.PollInterval = Duration(30 * time.Second)
	}
	if c.TwitterPollInterval == 0 {
		c.TwitterPollInterval = Duration(90 * time.Second)
	}
	if c.DedupWindow == 0 {
		c.DedupWindow = Duration(5 * time.Minute)
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	if c.Media.Directory == "" {
		c.Media.Directory = "data/media"
	}
	if c.Media.ImageMaxDimension == 0 {
		c.Media.ImageMaxDimension = 2000
	}
	if c.Media.FFmpegPath == "" {
		c.Media.FFmpegPath = "ffmpeg"
	}

	if c.Platforms.Mastodon.Visibility == "" {
		c.Platforms.Mastodon.Visibility = "public"
	}
	// Mastodon instances commonly allow 16MB images and 40MB video, and do not
	// impose a hard duration cap by default.
	defaultLimit(&c.Platforms.Mastodon.PlatformLimits, 16<<20, 40<<20, 2000, 0, true)

	// Bluesky accepts blobs up to 2MB (images) and 100MB (video), and its
	// video service caps clips at 60 seconds.
	defaultLimit(&c.Platforms.Bluesky.PlatformLimits, 2<<20, 100<<20, 2000, 60*time.Second, true)

	if c.Platforms.Bluesky.PDS == "" {
		c.Platforms.Bluesky.PDS = "https://bsky.social"
	}
	if c.Platforms.Bluesky.Jetstream == "" {
		c.Platforms.Bluesky.Jetstream = "wss://jetstream1.us-east.bsky.network/subscribe"
	}

	// X accepts 5MB images and, through the chunked upload endpoint, video of
	// up to 512MB and 140 seconds.
	defaultLimit(&c.Platforms.Twitter.PlatformLimits, 5<<20, 512<<20, 4096, 140*time.Second, true)
}

func defaultLimit(l *PlatformLimits, imageBytes, videoBytes, imageDim int, videoDuration time.Duration, video bool) {
	if l.MaxImageBytes == 0 {
		l.MaxImageBytes = imageBytes
	}
	if l.MaxVideoBytes == 0 {
		l.MaxVideoBytes = videoBytes
	}
	if l.MaxImageDim == 0 {
		l.MaxImageDim = imageDim
	}
	if l.MaxVideoDuration == 0 && videoDuration > 0 {
		l.MaxVideoDuration = Duration(videoDuration)
	}
	if !l.Video && videoBytes > 0 {
		// Only default this on when the operator left the field unset and the
		// platform is known to accept video.
		l.Video = video
	}
}

// envKeys maps configuration fields to the environment variable suffix that
// overrides them.
var envKeys = []struct {
	suffix string
	apply  func(c *Config, v string)
}{
	{"STORAGE", func(c *Config, v string) { c.Storage = v }},
	{"LOG_LEVEL", func(c *Config, v string) { c.LogLevel = v }},
	{"MASTODON_SERVER", func(c *Config, v string) { c.Platforms.Mastodon.Server = v }},
	{"MASTODON_CLIENT_ID", func(c *Config, v string) { c.Platforms.Mastodon.ClientID = v }},
	{"MASTODON_CLIENT_SECRET", func(c *Config, v string) { c.Platforms.Mastodon.ClientSecret = v }},
	{"MASTODON_ACCESS_TOKEN", func(c *Config, v string) { c.Platforms.Mastodon.AccessToken = v }},
	{"BLUESKY_HANDLE", func(c *Config, v string) { c.Platforms.Bluesky.Handle = v }},
	{"BLUESKY_APP_PASSWORD", func(c *Config, v string) { c.Platforms.Bluesky.AppPassword = v }},
	{"BLUESKY_PDS", func(c *Config, v string) { c.Platforms.Bluesky.PDS = v }},
	{"TWITTER_USER_ID", func(c *Config, v string) { c.Platforms.Twitter.UserID = v }},
	{"TWITTER_BEARER_TOKEN", func(c *Config, v string) { c.Platforms.Twitter.BearerToken = v }},
	{"TWITTER_CONSUMER_KEY", func(c *Config, v string) { c.Platforms.Twitter.ConsumerKey = v }},
	{"TWITTER_CONSUMER_SECRET", func(c *Config, v string) { c.Platforms.Twitter.ConsumerSecret = v }},
	{"TWITTER_ACCESS_TOKEN", func(c *Config, v string) { c.Platforms.Twitter.AccessToken = v }},
	{"TWITTER_ACCESS_TOKEN_SECRET", func(c *Config, v string) { c.Platforms.Twitter.AccessTokenSecret = v }},
}

func (c *Config) applyEnv() {
	for _, key := range envKeys {
		if v, ok := os.LookupEnv(EnvPrefix + key.suffix); ok && v != "" {
			key.apply(c, v)
		}
	}
}

// EnabledPlatforms returns the enabled platforms in a stable order.
func (c *Config) EnabledPlatforms() []model.Platform {
	var out []model.Platform
	if c.Platforms.Mastodon.Enabled {
		out = append(out, model.PlatformMastodon)
	}
	if c.Platforms.Bluesky.Enabled {
		out = append(out, model.PlatformBluesky)
	}
	if c.Platforms.Twitter.Enabled {
		out = append(out, model.PlatformTwitter)
	}
	return out
}

// WritablePlatforms returns the enabled platforms the bridge can post to. X
// only appears when its OAuth 1.0a user-context credentials are configured,
// because the write endpoints do not accept app-only bearer auth.
func (c *Config) WritablePlatforms() []model.Platform {
	var out []model.Platform
	if c.Platforms.Mastodon.Enabled {
		out = append(out, model.PlatformMastodon)
	}
	if c.Platforms.Bluesky.Enabled {
		out = append(out, model.PlatformBluesky)
	}
	if c.Platforms.Twitter.Enabled && c.Platforms.Twitter.UsesOAuth1() {
		out = append(out, model.PlatformTwitter)
	}
	return out
}

// TargetsFor returns the platforms a post originating on origin should be
// mirrored to. It honours an explicit routes override, then falls back to
// every writable platform except the origin itself.
func (c *Config) TargetsFor(origin model.Platform) []model.Platform {
	writable := c.WritablePlatforms()
	if explicit, ok := c.Routes[string(origin)]; ok {
		var out []model.Platform
		for _, name := range explicit {
			p := model.Platform(name)
			if !p.Valid() || p == origin {
				continue
			}
			if !contains(writable, p) {
				continue
			}
			out = append(out, p)
		}
		return out
	}
	var out []model.Platform
	for _, p := range writable {
		if p != origin {
			out = append(out, p)
		}
	}
	return out
}

func contains(list []model.Platform, p model.Platform) bool {
	for _, item := range list {
		if item == p {
			return true
		}
	}
	return false
}

// Validate reports the first configuration problem it finds.
func (c *Config) Validate() error {
	enabled := 0
	if c.Platforms.Mastodon.Enabled {
		enabled++
		if c.Platforms.Mastodon.Server == "" {
			return fmt.Errorf("mastodon: server is required")
		}
		if c.Platforms.Mastodon.AccessToken == "" {
			return fmt.Errorf("mastodon: access_token is required (set %sMASTODON_ACCESS_TOKEN)", EnvPrefix)
		}
	}
	if c.Platforms.Bluesky.Enabled {
		enabled++
		if c.Platforms.Bluesky.Handle == "" {
			return fmt.Errorf("bluesky: handle is required")
		}
		if c.Platforms.Bluesky.AppPassword == "" {
			return fmt.Errorf("bluesky: app_password is required (set %sBLUESKY_APP_PASSWORD)", EnvPrefix)
		}
	}
	if c.Platforms.Twitter.Enabled {
		enabled++
		if c.Platforms.Twitter.UserID == "" {
			return fmt.Errorf("twitter: user_id is required")
		}
		if c.Platforms.Twitter.BearerToken == "" && !c.Platforms.Twitter.UsesOAuth1() {
			return fmt.Errorf("twitter: need either bearer_token or a full oauth1 credential set")
		}
	}
	if enabled == 0 {
		return fmt.Errorf("no platforms enabled: enable at least one of mastodon, bluesky, twitter")
	}
	if c.Storage == "" {
		return fmt.Errorf("storage path must not be empty")
	}
	return nil
}
