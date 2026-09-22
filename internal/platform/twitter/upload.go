package twitter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/andrinoff/xmbbridge/internal/model"
	"github.com/andrinoff/xmbbridge/internal/platform"
)

// Media upload limits and tuning.
const (
	// maxMediaPerTweet is how many attachments one tweet may carry.
	maxMediaPerTweet = 4

	// uploadChunkSize is the segment size for the chunked upload used by video
	// and animated GIFs. The API accepts up to 5MB per APPEND.
	uploadChunkSize = 4 << 20

	// processingPollCeiling bounds how long a video transcode is waited for.
	processingPollCeiling = 10 * time.Minute

	// defaultProcessingCheckAfter is the fallback wait between processing
	// checks when the API does not suggest one.
	defaultProcessingCheckAfter = 5 * time.Second
)

// uploadSegment is the signed-media-upload portion of the adapter. The X write
// and upload endpoints only accept OAuth 1.0a user context, so this is only
// available when those credentials are configured.
type uploader struct {
	client *http.Client
	host   string
}

// CanPost reports whether the adapter can publish to X. The write and upload
// endpoints only accept OAuth 1.0a user context, so a bearer-only setup is
// read-only. This mirrors config.WritablePlatforms so routing and capability
// cannot disagree.
func (a *Adapter) CanPost() bool { return a.cfg.UsesOAuth1() }

// uploadMedia sends every attachment and returns their media IDs in order.
func (a *Adapter) uploadMedia(ctx context.Context, media []model.PreparedMedia) ([]string, error) {
	if len(media) == 0 {
		return nil, nil
	}
	if !a.CanPost() {
		return nil, fmt.Errorf("twitter: posting requires OAuth 1.0a user-context credentials")
	}
	up := &uploader{client: a.uploadClient, host: a.uploadHost}

	limit := maxMediaPerTweet
	if len(media) < limit {
		limit = len(media)
	}
	if len(media) > maxMediaPerTweet {
		a.log.Warn("twitter: only the first attachments are attached",
			"allowed", maxMediaPerTweet, "offered", len(media))
	}

	ids := make([]string, 0, limit)
	for i := 0; i < limit; i++ {
		item := media[i]
		id, err := up.upload(ctx, item)
		if err != nil {
			return nil, fmt.Errorf("upload attachment %d: %w", i, err)
		}
		if item.AltText != "" {
			if err := up.setAltText(ctx, id, item.AltText); err != nil {
				// Alt text is an accessibility nicety, not a reason to lose
				// the post.
				a.log.Warn("twitter: could not set alt text", "media_id", id, "error", err)
			}
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// upload chooses the simple path for still images and the chunked path for
// anything with motion, which is what the API requires.
func (u *uploader) upload(ctx context.Context, item model.PreparedMedia) (string, error) {
	mimeType := item.MimeType
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	if item.Kind == model.MediaImage {
		return u.uploadSimple(ctx, item.Bytes, mimeType)
	}
	return u.uploadChunked(ctx, item.Bytes, mimeType, categoryFor(item.Kind))
}

func categoryFor(kind model.MediaKind) string {
	if kind == model.MediaGIF {
		return "tweet_gif"
	}
	return "tweet_video"
}

// uploadSimple posts the whole file in one multipart request.
func (u *uploader) uploadSimple(ctx context.Context, data []byte, mimeType string) (string, error) {
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile("media", "media")
	if err != nil {
		return "", fmt.Errorf("build upload form: %w", err)
	}
	if _, err := part.Write(data); err != nil {
		return "", fmt.Errorf("write upload form: %w", err)
	}
	if err := form.Close(); err != nil {
		return "", fmt.Errorf("close upload form: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.host+"/1.1/media/upload.json", &body)
	if err != nil {
		return "", fmt.Errorf("build upload request: %w", err)
	}
	req.Header.Set("Content-Type", form.FormDataContentType())

	var result mediaUploadResult
	if err := u.do(req, &result); err != nil {
		return "", err
	}
	return result.id()
}

// uploadChunked runs the INIT / APPEND / FINALIZE sequence, then waits for any
// server-side processing to finish.
func (u *uploader) uploadChunked(ctx context.Context, data []byte, mimeType, category string) (string, error) {
	mediaID, err := u.init(ctx, int64(len(data)), mimeType, category)
	if err != nil {
		return "", err
	}

	for index, offset := 0, 0; offset < len(data); index, offset = index+1, offset+uploadChunkSize {
		end := offset + uploadChunkSize
		if end > len(data) {
			end = len(data)
		}
		if err := u.appendSegment(ctx, mediaID, index, data[offset:end], mimeType); err != nil {
			return "", err
		}
	}

	result, err := u.finalize(ctx, mediaID)
	if err != nil {
		return "", err
	}
	if err := u.awaitProcessing(ctx, result); err != nil {
		return "", err
	}
	return mediaID, nil
}

func (u *uploader) init(ctx context.Context, totalBytes int64, mimeType, category string) (string, error) {
	form := url.Values{}
	form.Set("command", "INIT")
	form.Set("media_type", mimeType)
	form.Set("media_category", category)
	form.Set("total_bytes", strconv.FormatInt(totalBytes, 10))

	var result mediaUploadResult
	if err := u.postForm(ctx, form, &result); err != nil {
		return "", fmt.Errorf("begin upload: %w", err)
	}
	return result.id()
}

func (u *uploader) appendSegment(ctx context.Context, mediaID string, index int, chunk []byte, mimeType string) error {
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile("media", "segment")
	if err != nil {
		return fmt.Errorf("build segment form: %w", err)
	}
	if _, err := part.Write(chunk); err != nil {
		return fmt.Errorf("write segment: %w", err)
	}
	if err := form.Close(); err != nil {
		return fmt.Errorf("close segment form: %w", err)
	}

	endpoint := u.host + "/1.1/media/upload.json?command=APPEND" +
		"&media_id=" + url.QueryEscape(mediaID) +
		"&segment_index=" + strconv.Itoa(index)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &body)
	if err != nil {
		return fmt.Errorf("build segment request: %w", err)
	}
	req.Header.Set("Content-Type", form.FormDataContentType())

	// APPEND answers with an empty body, so only the status matters.
	return u.do(req, nil)
}

func (u *uploader) finalize(ctx context.Context, mediaID string) (mediaUploadResult, error) {
	form := url.Values{}
	form.Set("command", "FINALIZE")
	form.Set("media_id", mediaID)

	var result mediaUploadResult
	if err := u.postForm(ctx, form, &result); err != nil {
		return mediaUploadResult{}, fmt.Errorf("finalize upload: %w", err)
	}
	return result, nil
}

// awaitProcessing polls until the API reports the media is ready. Still images
// never carry processing info, so this returns immediately for them.
func (u *uploader) awaitProcessing(ctx context.Context, result mediaUploadResult) error {
	if result.ProcessingInfo == nil {
		return nil
	}
	mediaID := result.mediaID()
	deadline := time.Now().Add(processingPollCeiling)

	for {
		state := result.ProcessingInfo.State
		switch state {
		case "", "succeeded":
			return nil
		case "failed":
			return fmt.Errorf("the API could not process the media: %s", result.ProcessingInfo.ErrorMessage())
		}

		wait := time.Duration(result.ProcessingInfo.CheckAfterSecs) * time.Second
		if wait <= 0 {
			wait = defaultProcessingCheckAfter
		}
		if err := platform.Sleep(ctx, wait); err != nil {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("media %s was still %s after %s", mediaID, state, processingPollCeiling)
		}

		form := url.Values{}
		form.Set("command", "STATUS")
		form.Set("media_id", mediaID)

		var status mediaUploadResult
		if err := u.postForm(ctx, form, &status); err != nil {
			return fmt.Errorf("check media status: %w", err)
		}
		result = status
	}
}

// setAltText attaches a description to an uploaded image.
func (u *uploader) setAltText(ctx context.Context, mediaID, altText string) error {
	payload := map[string]any{
		"media_id": mediaID,
		"alt_text": map[string]string{"text": altText},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("build alt text payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		u.host+"/1.1/media/metadata/create.json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build alt text request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	return u.do(req, nil)
}

// postForm sends a form-encoded request. Form bodies are included in the OAuth
// signature by the signing transport, which is why the non-file commands use
// this shape while APPEND uses multipart.
func (u *uploader) postForm(ctx context.Context, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		u.host+"/1.1/media/upload.json", bytes.NewBufferString(form.Encode()))
	if err != nil {
		return fmt.Errorf("build upload command: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return u.do(req, out)
}

// do performs the request and decodes the body when out is non-nil.
func (u *uploader) do(req *http.Request, out any) error {
	resp, err := u.client.Do(req)
	if err != nil {
		return fmt.Errorf("media upload request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read media upload response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("media upload rejected with %s: %s", resp.Status, truncateForError(body))
	}
	if out == nil || len(body) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode media upload response: %w", err)
	}
	return nil
}

// mediaUploadResult covers the shapes returned by the upload commands.
type mediaUploadResult struct {
	MediaID        int64           `json:"media_id"`
	MediaIDString  string          `json:"media_id_string"`
	ProcessingInfo *processingInfo `json:"processing_info"`
}

// processingInfo is the transcoding state the API reports after FINALIZE.
type processingInfo struct {
	State          string `json:"state"`
	CheckAfterSecs int    `json:"check_after_secs"`
	Error          *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (i *processingInfo) ErrorMessage() string {
	if i == nil || i.Error == nil || i.Error.Message == "" {
		return "no reason given"
	}
	return i.Error.Message
}

func (r mediaUploadResult) id() (string, error) {
	if r.MediaIDString != "" {
		return r.MediaIDString, nil
	}
	if r.MediaID != 0 {
		return strconv.FormatInt(r.MediaID, 10), nil
	}
	return "", fmt.Errorf("the API returned no media id")
}

func (r mediaUploadResult) mediaID() string {
	id, _ := r.id()
	return id
}

func truncateForError(body []byte) string {
	const max = 300
	if len(body) > max {
		return string(body[:max]) + "..."
	}
	return string(body)
}
