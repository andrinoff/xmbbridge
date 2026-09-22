// Package feed wires the platform adapters together.
//
// Listeners push posts into a single channel; the engine consumes them, drops
// anything it recognises as its own output or as a duplicate, and fans the rest
// out to the other platforms.
package feed

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/andrinoff/xmbbridge/internal/config"
	"github.com/andrinoff/xmbbridge/internal/media"
	"github.com/andrinoff/xmbbridge/internal/model"
	"github.com/andrinoff/xmbbridge/internal/platform"
	"github.com/andrinoff/xmbbridge/internal/store"
	"github.com/andrinoff/xmbbridge/internal/text"
)

// ellipsis marks text that was shortened to fit a platform.
const ellipsis = "\u2026"

// sinkBuffer is how many posts may queue up before listeners block. It absorbs
// a burst from a fast source while the engine is uploading media.
const sinkBuffer = 256

// Source is a platform the bridge reads posts from.
type Source interface {
	Name() model.Platform
	// Run emits posts until the context is cancelled.
	Run(ctx context.Context, sink chan<- model.Post) error
}

// Target is a platform the bridge writes posts to.
type Target interface {
	Name() model.Platform
	// Limits describes what the platform accepts.
	Limits() model.Limits
	// Post publishes a prepared post and returns its identifier on that
	// platform.
	Post(ctx context.Context, out model.Outbound, media []model.PreparedMedia) (string, error)
}

// Engine moves posts from sources to targets.
type Engine struct {
	cfg     *config.Config
	store   *store.Store
	log     *slog.Logger
	fetcher *media.Fetcher
	sources []Source
	targets map[model.Platform]Target
	routes  map[model.Platform][]model.Platform

	attempts int              // outbound posts are retried this many times
	backoff  platform.Backoff // delays between those attempts
}

// New builds an engine from the configured adapters.
func New(cfg *config.Config, st *store.Store, log *slog.Logger, sources []Source, targets []Target) *Engine {
	if log == nil {
		log = slog.Default()
	}
	targetMap := make(map[model.Platform]Target, len(targets))
	for _, target := range targets {
		targetMap[target.Name()] = target
	}
	routes := make(map[model.Platform][]model.Platform, len(model.AllPlatforms))
	for _, origin := range model.AllPlatforms {
		routes[origin] = cfg.TargetsFor(origin)
	}
	return &Engine{
		cfg:      cfg,
		store:    st,
		log:      log,
		fetcher:  media.NewFetcher(nil),
		sources:  sources,
		targets:  targetMap,
		routes:   routes,
		attempts: 3,
		backoff:  platform.Backoff{Base: 3 * time.Second, Max: 30 * time.Second},
	}
}

// Routes returns the resolved routing table, for logging at startup.
func (e *Engine) Routes() map[model.Platform][]model.Platform {
	out := make(map[model.Platform][]model.Platform, len(e.routes))
	for origin, targets := range e.routes {
		out[origin] = append([]model.Platform(nil), targets...)
	}
	return out
}

// Run starts every listener and processes posts until the context is
// cancelled. A listener that fails brings the whole engine down so the process
// supervisor can restart it.
func (e *Engine) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	sink := make(chan model.Post, sinkBuffer)

	var wg sync.WaitGroup
	listenerErr := make(chan error, len(e.sources))
	for _, source := range e.sources {
		wg.Add(1)
		go func(source Source) {
			defer wg.Done()
			if err := source.Run(ctx, sink); err != nil && ctx.Err() == nil {
				listenerErr <- fmt.Errorf("%s listener stopped: %w", source.Name(), err)
				cancel()
			}
		}(source)
	}

	consumeErr := e.consume(ctx, sink)
	cancel()
	wg.Wait()

	select {
	case err := <-listenerErr:
		return err
	default:
	}
	return consumeErr
}

func (e *Engine) consume(ctx context.Context, sink <-chan model.Post) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case post := <-sink:
			e.handle(ctx, post)
		}
	}
}

// handle applies the bridge's two guards and then fans the post out.
func (e *Engine) handle(ctx context.Context, post model.Post) {
	log := e.log.With("origin", post.Origin.String(), "post", post.OriginID)

	// Guard one: never bridge a post the bridge itself created. This is what
	// keeps a two-way bridge from echoing forever.
	ours, err := e.store.IsBridgedTarget(ctx, post.Origin, post.OriginID)
	if err != nil {
		log.Error("could not check bridged targets", "error", err)
		return
	}
	if ours {
		log.Debug("skipping a post the bridge created")
		return
	}

	// Guard two: identical content published by hand on two platforms within
	// the dedup window would otherwise be copied twice.
	hash := text.Hash(post.Text)
	if hash != "" {
		seen, err := e.store.SeenRecently(ctx, hash, e.cfg.DedupWindow.D())
		if err != nil {
			log.Error("could not check recent duplicates", "error", err)
		} else if seen {
			log.Info("skipping duplicate content seen within the dedup window")
			return
		}
	}

	targets := e.routes[post.Origin]
	if len(targets) == 0 {
		log.Debug("no targets configured for this origin")
		return
	}

	bridged := 0
	for _, name := range targets {
		target, ok := e.targets[name]
		if !ok {
			continue
		}
		if e.bridge(ctx, post, target, log) {
			bridged++
		}
	}
	if bridged > 0 && hash != "" {
		if err := e.store.MarkSeen(ctx, hash); err != nil {
			log.Error("could not record dedup hash", "error", err)
		}
	}
}

// bridge copies one post to one target and reports whether the target now holds
// a copy (either created by this call or by an earlier one).
func (e *Engine) bridge(ctx context.Context, post model.Post, target Target, log *slog.Logger) bool {
	targetName := target.Name()
	log = log.With("target", targetName.String())

	claimed, err := e.store.ClaimBridge(ctx, post.Origin, post.OriginID, targetName)
	if err != nil {
		log.Error("could not claim bridge", "error", err)
		return false
	}
	if !claimed {
		log.Debug("already bridged")
		return true
	}

	limits := target.Limits()
	out := model.Outbound{
		Origin:    post.Origin,
		OriginID:  post.OriginID,
		Text:      text.Truncate(post.Text, limits.TextMax, ellipsis),
		Lang:      post.Lang,
		CreatedAt: post.CreatedAt,
		IsReply:   post.IsReply,
	}
	if post.IsReply && post.ReplyToID != "" {
		parent, ok, err := e.store.TargetID(ctx, post.Origin, post.ReplyToID, targetName)
		if err != nil {
			log.Warn("could not resolve the bridged parent; posting as a standalone reply", "error", err)
		} else if ok {
			out.ParentTargetID = parent
		}
	}

	attachments := e.prepareMedia(ctx, post, limits, log)

	var createdID string
	err = platform.Retry(ctx, e.attempts, e.backoff, log,
		fmt.Sprintf("bridge %s to %s", post.Ref(), targetName), func(ctx context.Context) error {
			var postErr error
			createdID, postErr = target.Post(ctx, out, attachments)
			return postErr
		})
	if err != nil {
		log.Error("bridge failed", "attachments", len(attachments), "error", err)
		if markErr := e.store.FailBridge(ctx, post.Origin, post.OriginID, targetName, err); markErr != nil {
			log.Error("could not record bridge failure", "error", markErr)
		}
		return false
	}

	if err := e.store.CompleteBridge(ctx, post.Origin, post.OriginID, targetName, createdID); err != nil {
		log.Error("could not record bridge result", "error", err)
	}
	log.Info("bridged post", "target_id", createdID, "attachments", len(attachments))
	return true
}

// prepareMedia fetches and re-encodes every attachment for one target,
// skipping any that cannot be made to fit.
func (e *Engine) prepareMedia(ctx context.Context, post model.Post, limits model.Limits, log *slog.Logger) []model.PreparedMedia {
	var out []model.PreparedMedia
	for i, attachment := range post.Media {
		base := fmt.Sprintf("%s-%s-%d", post.Origin, post.OriginID, i)
		item, err := e.prepareAttachment(ctx, attachment, limits, base)
		if err != nil {
			log.Warn("dropping attachment", "index", i, "kind", attachment.Kind, "error", err)
			continue
		}
		out = append(out, item)
	}
	return out
}

func (e *Engine) prepareAttachment(ctx context.Context, attachment model.Media, limits model.Limits, base string) (model.PreparedMedia, error) {
	switch attachment.Kind {
	case model.MediaImage:
		return e.prepareImage(ctx, attachment, limits, base)
	case model.MediaVideo, model.MediaGIF:
		return e.prepareVideo(ctx, attachment, limits, base)
	default:
		return model.PreparedMedia{}, fmt.Errorf("unsupported attachment kind %q", attachment.Kind)
	}
}

func (e *Engine) prepareImage(ctx context.Context, attachment model.Media, limits model.Limits, base string) (model.PreparedMedia, error) {
	if attachment.URL == "" {
		return model.PreparedMedia{}, fmt.Errorf("image has no URL")
	}
	data, mimeType, err := e.fetcher.Fetch(ctx, attachment)
	if err != nil {
		return model.PreparedMedia{}, err
	}
	result, err := media.FitImage(data, media.ImageOptions{
		MaxBytes:     limits.ImageMaxBytes,
		MaxDimension: limits.ImageMaxDimension,
		AltText:      attachment.AltText,
		BaseName:     base,
	})
	if err != nil {
		return model.PreparedMedia{}, err
	}
	if result.MimeType == "" {
		result.MimeType = mimeType
	}
	return model.PreparedMedia{
		Kind:     model.MediaImage,
		Bytes:    result.Bytes,
		MimeType: result.MimeType,
		AltText:  attachment.AltText,
		Width:    result.Width,
		Height:   result.Height,
		Filename: result.Filename,
	}, nil
}

func (e *Engine) prepareVideo(ctx context.Context, attachment model.Media, limits model.Limits, base string) (model.PreparedMedia, error) {
	if !limits.Video {
		return e.preparePoster(ctx, attachment, limits, base)
	}
	if attachment.URL == "" {
		// Some origins (notably X) expose only a poster frame for video.
		return e.preparePoster(ctx, attachment, limits, base)
	}

	data, _, err := e.fetcher.Fetch(ctx, attachment)
	if err != nil {
		return model.PreparedMedia{}, err
	}
	result, err := media.TranscodeVideo(ctx, data, media.VideoOptions{
		MaxBytes:     limits.VideoMaxBytes,
		MaxDuration:  limits.VideoMaxDuration,
		MaxDimension: limits.ImageMaxDimension,
		FFmpegPath:   e.cfg.Media.FFmpegPath,
		AltText:      attachment.AltText,
		BaseName:     base,
	})
	if err != nil {
		e.log.Warn("video could not be prepared, falling back to its poster frame", "error", err)
		return e.preparePoster(ctx, attachment, limits, base)
	}
	return model.PreparedMedia{
		Kind:     model.MediaVideo,
		Bytes:    result.Bytes,
		MimeType: result.MimeType,
		AltText:  attachment.AltText,
		Width:    result.Width,
		Height:   result.Height,
		Filename: result.Filename,
	}, nil
}

// preparePoster substitutes a still image for a video the target cannot take.
func (e *Engine) preparePoster(ctx context.Context, attachment model.Media, limits model.Limits, base string) (model.PreparedMedia, error) {
	poster := attachment
	poster.Kind = model.MediaImage
	poster.URL = attachment.PosterURL
	poster.MimeType = ""
	if poster.URL == "" {
		return model.PreparedMedia{}, fmt.Errorf("video cannot be sent and has no poster frame to fall back to")
	}
	return e.prepareImage(ctx, poster, limits, base+"-poster")
}
