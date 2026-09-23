package twitter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/andrinoff/xmbbridge/internal/model"
	"github.com/andrinoff/xmbbridge/internal/platform"

	tw "github.com/g8rswimmer/go-twitter/v2"
)

// maxResults is the largest page the timeline endpoint accepts.
const maxResults = 100

// humanActionPause is how long the listener rests after the API refuses a
// request for a reason retrying cannot fix: exhausted credits, revoked
// credentials, or a tier that does not include the endpoint. Without this the
// poll loop would retry every few minutes and fill the logs while a person
// tops up credits or fixes a token.
const humanActionPause = time.Hour

// Run polls the account's tweet timeline and emits each new tweet. It blocks
// until the context is cancelled.
func (a *Adapter) Run(ctx context.Context, sink chan<- model.Post) error {
	a.log.Info("twitter listener started",
		"user_id", a.userID,
		"poll_interval", a.pollInterval.String(),
		"replies", a.cfg.IncludeReplies,
	)
	return platform.PollLoop(ctx, "twitter", a.pollInterval, a.log, func(ctx context.Context) (time.Duration, error) {
		return a.poll(ctx, sink)
	})
}

// saveCursor persists progress on a context that survives shutdown.
func (a *Adapter) saveCursor(value string) error {
	ctx, cancel := platform.ProgressContext()
	defer cancel()
	return a.state.SetCursor(ctx, model.PlatformTwitter, value)
}

// poll fetches the newest tweets once. The returned duration lets the caller
// park until a rate limit resets instead of sleeping for the default interval.
func (a *Adapter) poll(ctx context.Context, sink chan<- model.Post) (time.Duration, error) {
	cursor, err := a.state.Cursor(ctx, model.PlatformTwitter)
	if err != nil {
		return 0, err
	}

	opts := tw.UserTweetTimelineOpts{
		MaxResults: maxResults,
		Expansions: []tw.Expansion{tw.ExpansionAttachmentsMediaKeys},
		TweetFields: []tw.TweetField{
			tw.TweetFieldID,
			tw.TweetFieldText,
			tw.TweetFieldAttachments,
			tw.TweetFieldCreatedAt,
			tw.TweetFieldEntities,
			tw.TweetFieldInReplyToUserID,
			tw.TweetFieldLanguage,
			tw.TweetFieldReferencedTweets,
		},
		MediaFields: []tw.MediaField{
			tw.MediaFieldURL,
			tw.MediaFieldType,
			tw.MediaFieldPreviewImageURL,
			tw.MediaFieldWidth,
			tw.MediaFieldHeight,
			tw.MediaFieldAltText,
		},
	}
	if cursor != "" {
		opts.SinceID = cursor
	}

	response, err := a.client.UserTweetTimeline(ctx, a.userID, opts)
	if err != nil {
		wrapped := fmt.Errorf("twitter: fetch timeline: %w", err)
		if pause := pauseForRefusal(err, a.log); pause > 0 {
			return pause, wrapped
		}
		return 0, wrapped
	}

	// Park until the rate limit window resets rather than immediately burning
	// the next window's budget.
	var wait time.Duration
	if response.RateLimit != nil && response.RateLimit.Remaining <= 1 {
		reset := response.RateLimit.Reset.Time()
		if until := time.Until(reset); until > 0 && until < 24*time.Hour {
			wait = until + time.Second
		}
	}

	if response.Raw == nil || len(response.Raw.Tweets) == 0 {
		return wait, nil
	}

	mediaByKey := map[string]*tw.MediaObj{}
	if response.Raw.Includes != nil {
		for _, item := range response.Raw.Includes.Media {
			mediaByKey[item.Key] = item
		}
	}

	newest := cursor
	for _, tweet := range response.Raw.Tweets {
		if platform.IDNewer(tweet.ID, newest) {
			newest = tweet.ID
		}
	}

	// A first poll with no stored cursor would replay history; skip it unless
	// the operator asked to backfill.
	if cursor == "" && !a.backfill {
		a.log.Info("twitter: starting from the newest tweet; existing history is not bridged", "newest", newest)
		if err := a.saveCursor(newest); err != nil {
			return wait, err
		}
		return wait, nil
	}

	// The API returns newest first; emit oldest first.
	for i := len(response.Raw.Tweets) - 1; i >= 0; i-- {
		tweet := response.Raw.Tweets[i]
		if cursor != "" && !platform.IDNewer(tweet.ID, cursor) {
			continue
		}
		post := a.toPost(tweet, mediaByKey)
		if post.IsReply && !a.cfg.IncludeReplies {
			continue
		}
		if err := platform.Emit(ctx, sink, post); err != nil {
			return wait, err
		}
	}

	if newest != cursor {
		if err := a.saveCursor(newest); err != nil {
			return wait, err
		}
	}
	return wait, nil
}

// statusFromError digs the HTTP status code out of the error shapes the
// library returns: *ErrorResponse for decoded API errors and *HTTPError when
// the body could not be decoded.
func statusFromError(err error) (int, bool) {
	var apiErr *tw.ErrorResponse
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode, true
	}
	var httpErr *tw.HTTPError
	if errors.As(err, &httpErr) {
		return httpErr.StatusCode, true
	}
	var httpVal tw.HTTPError
	if errors.As(err, &httpVal) {
		return httpVal.StatusCode, true
	}
	return 0, false
}

// refusalHint describes an HTTP status that retrying cannot fix. Reads hit it
// when credits run out or a tier lacks an endpoint; writes hit it for the same
// reasons plus permission problems.
func refusalHint(status int) (string, bool) {
	switch status {
	case http.StatusPaymentRequired:
		return "API credits are exhausted; top up at developer.x.com", true
	case http.StatusUnauthorized:
		return "the credentials were rejected", true
	case http.StatusForbidden:
		return "the API tier or app permissions may not allow this request", true
	default:
		return "", false
	}
}

// pauseForRefusal returns a long rest when the API refused the request for a
// reason only a person can fix, and zero when retrying is reasonable.
func pauseForRefusal(err error, log *slog.Logger) time.Duration {
	status, ok := statusFromError(err)
	if !ok {
		return 0
	}
	hint, refused := refusalHint(status)
	if !refused {
		return 0
	}
	log.Error("twitter: the API refused the request; pausing the listener",
		"status", status,
		"resume_in", humanActionPause.String(),
		"hint", hint)
	return humanActionPause
}

// logRefusal records why a write failed, when the reason needs a person.
func logRefusal(err error, log *slog.Logger) {
	status, ok := statusFromError(err)
	if !ok {
		return
	}
	if hint, refused := refusalHint(status); refused {
		log.Error("twitter: the API refused the write",
			"status", status, "hint", hint)
	}
}
