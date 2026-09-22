package twitter

import (
	"context"
	"fmt"

	"github.com/andrinoff/xmbbridge/internal/model"

	tw "github.com/g8rswimmer/go-twitter/v2"
)

// TextMax is X's post length limit, counted in characters.
const TextMax = 280

// Limits reports what an X account accepts. X allows 5MB images, 4 attachments
// per post, 140 second video, and 512MB uploads through the chunked endpoint.
func (a *Adapter) Limits() model.Limits {
	return model.Limits{
		TextMax:           TextMax,
		ImageMaxBytes:     a.cfg.MaxImageBytes,
		ImageMaxDimension: a.cfg.MaxImageDim,
		VideoMaxBytes:     a.cfg.MaxVideoBytes,
		VideoMaxDuration:  a.cfg.MaxVideoDuration.D(),
		Video:             a.cfg.Video,
	}
}

// Post publishes an already-prepared post and returns the new tweet ID.
func (a *Adapter) Post(ctx context.Context, out model.Outbound, media []model.PreparedMedia) (string, error) {
	mediaIDs, err := a.uploadMedia(ctx, media)
	if err != nil {
		return "", err
	}

	request := tw.CreateTweetRequest{Text: out.Text}
	if len(mediaIDs) > 0 {
		request.Media = &tw.CreateTweetMedia{IDs: mediaIDs}
	}
	if out.IsReply && out.ParentTargetID != "" {
		request.Reply = &tw.CreateTweetReply{InReplyToTweetID: out.ParentTargetID}
	}

	response, err := a.client.CreateTweet(ctx, request)
	if err != nil {
		logRefusal(err, a.log)
		return "", fmt.Errorf("twitter: create tweet: %w", err)
	}
	if response.Tweet == nil || response.Tweet.ID == "" {
		return "", fmt.Errorf("twitter: create tweet returned no id")
	}
	return response.Tweet.ID, nil
}
