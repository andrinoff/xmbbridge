// Package mastodon bridges posts to and from a Mastodon account.
package mastodon

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/andrinoff/xmbbridge/internal/config"
	"github.com/andrinoff/xmbbridge/internal/model"
	"github.com/andrinoff/xmbbridge/internal/platform"
	"github.com/andrinoff/xmbbridge/internal/text"

	"github.com/mattn/go-mastodon"
)

// TextMax is the conventional Mastodon post limit; most instances enforce it.
const TextMax = 500

// Options configures the adapter.
type Options struct {
	Config       config.MastodonConfig
	PollInterval time.Duration
	Backfill     bool
	State        platform.State
	Logger       *slog.Logger
}

// Adapter implements the bridge's source and target interfaces for Mastodon.
type Adapter struct {
	client       *mastodon.Client
	cfg          config.MastodonConfig
	log          *slog.Logger
	state        platform.State
	pollInterval time.Duration
	backfill     bool

	// accountID is the authenticated account, discovered lazily and reused
	// between the startup check and the listener.
	accountID mastodon.ID
}

// New builds a Mastodon adapter.
func New(opts Options) (*Adapter, error) {
	if opts.Config.Server == "" {
		return nil, fmt.Errorf("mastodon: server is required")
	}
	if opts.Config.AccessToken == "" {
		return nil, fmt.Errorf("mastodon: access token is required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	client := mastodon.NewClient(&mastodon.Config{
		Server:       opts.Config.Server,
		ClientID:     opts.Config.ClientID,
		ClientSecret: opts.Config.ClientSecret,
		AccessToken:  opts.Config.AccessToken,
	})
	return &Adapter{
		client:       client,
		cfg:          opts.Config,
		log:          opts.Logger,
		state:        opts.State,
		pollInterval: opts.PollInterval,
		backfill:     opts.Backfill,
	}, nil
}

// Name identifies the platform.
func (a *Adapter) Name() model.Platform { return model.PlatformMastodon }

// Check verifies that the credentials work by asking the instance who they
// belong to. It is called by `bridge -check`.
func (a *Adapter) Check(ctx context.Context) error {
	account, err := a.client.GetAccountCurrentUser(ctx)
	if err != nil {
		return fmt.Errorf("verify credentials against %s: %w", a.cfg.Server, err)
	}
	if a.accountID == "" {
		a.accountID = account.ID
	}
	a.log.Info("mastodon: credentials OK", "account", account.Acct, "instance", a.cfg.Server)
	return nil
}

// Limits reports what this Mastodon account accepts.
func (a *Adapter) Limits() model.Limits {
	return model.Limits{
		TextMax:           TextMax,
		ImageMaxBytes:     a.cfg.MaxImageBytes,
		ImageMaxDimension: a.cfg.MaxImageDim,
		VideoMaxBytes:     a.cfg.MaxVideoBytes,
		Video:             a.cfg.Video,
	}
}

// Post publishes an already-prepared post and returns the new status ID.
func (a *Adapter) Post(ctx context.Context, out model.Outbound, media []model.PreparedMedia) (string, error) {
	mediaIDs := make([]mastodon.ID, 0, len(media))
	for i, item := range media {
		attachment, err := a.client.UploadMediaFromMedia(ctx, &mastodon.Media{
			File:        bytes.NewReader(item.Bytes),
			Description: item.AltText,
		})
		if err != nil {
			return "", fmt.Errorf("upload attachment %d: %w", i, err)
		}
		mediaIDs = append(mediaIDs, attachment.ID)
	}

	toot := &mastodon.Toot{
		Status:     out.Text,
		Visibility: a.cfg.Visibility,
		Language:   out.Lang,
	}
	if len(mediaIDs) > 0 {
		toot.MediaIDs = mediaIDs
	}
	if out.IsReply && out.ParentTargetID != "" {
		toot.InReplyToID = mastodon.ID(out.ParentTargetID)
	}

	status, err := a.client.PostStatus(ctx, toot)
	if err != nil {
		return "", fmt.Errorf("post status: %w", err)
	}
	return string(status.ID), nil
}

// toPost converts a Mastodon status into the bridge's neutral form.
func (a *Adapter) toPost(status *mastodon.Status) model.Post {
	post := model.Post{
		Origin:    model.PlatformMastodon,
		OriginID:  string(status.ID),
		OriginURL: status.URL,
		Text:      text.StripHTML(status.Content),
		CreatedAt: status.CreatedAt,
		Lang:      status.Language,
	}
	if status.InReplyToID != nil {
		post.IsReply = true
		post.ReplyToID = fmt.Sprint(status.InReplyToID)
	}
	for _, attachment := range status.MediaAttachments {
		post.Media = append(post.Media, mediaFromAttachment(attachment))
	}
	return post
}

func mediaFromAttachment(attachment mastodon.Attachment) model.Media {
	kind := model.MediaImage
	switch attachment.Type {
	case "video":
		kind = model.MediaVideo
	case "gifv":
		kind = model.MediaGIF
	case "audio":
		kind = model.MediaAudio
	}
	return model.Media{
		Kind:      kind,
		URL:       attachment.URL,
		AltText:   attachment.Description,
		PosterURL: attachment.PreviewURL,
		Width:     int(attachment.Meta.Original.Width),
		Height:    int(attachment.Meta.Original.Height),
	}
}
