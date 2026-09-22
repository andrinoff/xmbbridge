// Package twitter reads posts from an X (Twitter) account.
//
// X is deliberately inbound-only: the bridge mirrors X posts to every other
// platform but never writes to X. Both app-only bearer auth and OAuth 1.0a
// user context are supported.
package twitter

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/andrinoff/xmbbridge/internal/config"
	"github.com/andrinoff/xmbbridge/internal/model"
	"github.com/andrinoff/xmbbridge/internal/platform"

	"github.com/dghubble/oauth1"
	tw "github.com/g8rswimmer/go-twitter/v2"
)

// Options configures the adapter.
type Options struct {
	Config       config.TwitterConfig
	PollInterval time.Duration
	Backfill     bool
	State        platform.State
	Logger       *slog.Logger
	HTTPClient   *http.Client

	// APIHost overrides the X API root, which tests use to point the client at
	// a local server. Empty means the real API.
	APIHost string
}

// bearerAuthorizer signs requests as app-only.
type bearerAuthorizer struct{ token string }

func (a bearerAuthorizer) Add(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+a.token)
}

// oauth1Authorizer is a no-op because signing happens in the HTTP client's
// transport. The library still calls Add on every request.
type oauth1Authorizer struct{}

func (oauth1Authorizer) Add(*http.Request) {}

// Adapter implements the bridge's source interface for X.
type Adapter struct {
	client       *tw.Client
	cfg          config.TwitterConfig
	log          *slog.Logger
	state        platform.State
	userID       string
	pollInterval time.Duration
	backfill     bool
}

// New builds an X adapter. It does not perform any network calls; credentials
// are validated on the first poll.
func New(opts Options) (*Adapter, error) {
	if opts.Config.UserID == "" {
		return nil, fmt.Errorf("twitter: user_id is required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}

	var authorizer tw.Authorizer
	var httpClient *http.Client
	if opts.Config.BearerToken != "" {
		authorizer = bearerAuthorizer{token: opts.Config.BearerToken}
		httpClient = opts.HTTPClient
	} else {
		oauthConfig := oauth1.NewConfig(opts.Config.ConsumerKey, opts.Config.ConsumerSecret)
		token := oauth1.NewToken(opts.Config.AccessToken, opts.Config.AccessTokenSecret)
		httpClient = oauthConfig.Client(context.Background(), token)
		authorizer = oauth1Authorizer{}
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}

	host := opts.APIHost
	if host == "" {
		host = "https://api.twitter.com"
	}

	return &Adapter{
		client: &tw.Client{
			Authorizer: authorizer,
			Client:     httpClient,
			Host:       host,
		},
		cfg:          opts.Config,
		log:          opts.Logger,
		state:        opts.State,
		userID:       opts.Config.UserID,
		pollInterval: opts.PollInterval,
		backfill:     opts.Backfill,
	}, nil
}

// Name identifies the platform.
func (a *Adapter) Name() model.Platform { return model.PlatformTwitter }

// Check verifies the credentials work and the configured user id resolves. It
// is called by `bridge -check`.
func (a *Adapter) Check(ctx context.Context) error {
	response, err := a.client.UserLookup(ctx, []string{a.userID}, tw.UserLookupOpts{})
	if err != nil {
		return fmt.Errorf("verify credentials against api.twitter.com: %w", err)
	}
	if response.Raw == nil || len(response.Raw.Users) == 0 {
		return fmt.Errorf("no such twitter user id %s", a.userID)
	}
	a.log.Info("twitter: credentials OK", "user_id", a.userID,
		"username", response.Raw.Users[0].UserName)
	return nil
}

// toPost converts an X tweet into the bridge's neutral form.
func (a *Adapter) toPost(tweet *tw.TweetObj, mediaByKey map[string]*tw.MediaObj) model.Post {
	post := model.Post{
		Origin:    model.PlatformTwitter,
		OriginID:  tweet.ID,
		OriginURL: fmt.Sprintf("https://x.com/i/web/status/%s", tweet.ID),
		Text:      tweet.Text,
		CreatedAt: parseTime(tweet.CreatedAt),
		Lang:      tweet.Language,
	}
	if tweet.InReplyToUserID != "" {
		post.IsReply = true
	}
	for _, reference := range tweet.ReferencedTweets {
		if reference.Type == "replied_to" && post.ReplyToID == "" {
			post.ReplyToID = reference.ID
		}
	}
	if tweet.Attachments != nil {
		for _, key := range tweet.Attachments.MediaKeys {
			item := mediaByKey[key]
			if item == nil {
				continue
			}
			post.Media = append(post.Media, mediaFromTweet(item))
		}
	}
	return post
}

func mediaFromTweet(item *tw.MediaObj) model.Media {
	attachment := model.Media{
		AltText:   item.AltText,
		Width:     item.Width,
		Height:    item.Height,
		PosterURL: item.PreviewImageURL,
	}
	switch item.Type {
	case "photo":
		attachment.Kind = model.MediaImage
		attachment.URL = item.URL
	case "video":
		// The v2 API does not expose a downloadable video file, only the
		// poster frame; the bridge carries the poster across.
		attachment.Kind = model.MediaVideo
	case "animated_gif":
		// The URL for an animated_gif is the looping MP4 file itself.
		attachment.Kind = model.MediaGIF
		attachment.URL = item.URL
	default:
		attachment.Kind = model.MediaImage
		attachment.URL = item.URL
	}
	return attachment
}

func parseTime(raw string) time.Time {
	if raw == "" {
		return time.Now()
	}
	// X timestamps are RFC 3339 with millisecond precision.
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if parsed, err := time.Parse(layout, raw); err == nil {
			return parsed
		}
	}
	return time.Now()
}
