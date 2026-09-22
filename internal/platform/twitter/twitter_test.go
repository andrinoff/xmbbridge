package twitter

import (
	"net/http"
	"testing"
	"time"

	"github.com/andrinoff/xmbbridge/internal/config"
	"github.com/andrinoff/xmbbridge/internal/model"

	tw "github.com/g8rswimmer/go-twitter/v2"
)

func bearerConfig() config.TwitterConfig {
	return config.TwitterConfig{
		UserID:      "123",
		BearerToken: "bearer",
	}
}

func oauth1Config() config.TwitterConfig {
	return config.TwitterConfig{
		UserID:            "123",
		ConsumerKey:       "ck",
		ConsumerSecret:    "cs",
		AccessToken:       "at",
		AccessTokenSecret: "ats",
	}
}

func newTestRequest() (*http.Request, error) {
	return http.NewRequest(http.MethodGet, "https://api.twitter.com/2/users/123/tweets", nil)
}

func TestToPost(t *testing.T) {
	adapter := &Adapter{}
	tweet := &tw.TweetObj{
		ID:        "1700000000000000000",
		Text:      "hello from X",
		CreatedAt: "2024-05-01T10:00:00.000Z",
		Language:  "en",
	}

	post := adapter.toPost(tweet, nil)

	if post.Origin != model.PlatformTwitter {
		t.Fatalf("origin = %q", post.Origin)
	}
	if post.OriginID != tweet.ID {
		t.Fatalf("origin id = %q", post.OriginID)
	}
	if post.Text != "hello from X" {
		t.Fatalf("text = %q", post.Text)
	}
	if post.Lang != "en" {
		t.Fatalf("lang = %q", post.Lang)
	}
	if post.IsReply {
		t.Fatal("a top-level tweet is not a reply")
	}
	if post.CreatedAt.UTC() != time.Date(2024, 5, 1, 10, 0, 0, 0, time.UTC) {
		t.Fatalf("created at = %v", post.CreatedAt)
	}
	if post.OriginURL == "" {
		t.Fatal("a link back to the post should be recorded")
	}
}

func TestToPostDetectsRepliesAndParent(t *testing.T) {
	adapter := &Adapter{}
	tweet := &tw.TweetObj{
		ID:              "2",
		Text:            "a reply",
		InReplyToUserID: "42",
		ReferencedTweets: []*tw.TweetReferencedTweetObj{
			{Type: "replied_to", ID: "1"},
		},
	}

	post := adapter.toPost(tweet, nil)

	if !post.IsReply {
		t.Fatal("a tweet with in_reply_to_user_id is a reply")
	}
	if post.ReplyToID != "1" {
		t.Fatalf("reply parent = %q, want 1", post.ReplyToID)
	}
}

func TestToPostAttachesMedia(t *testing.T) {
	adapter := &Adapter{}
	tweet := &tw.TweetObj{
		ID:   "3",
		Text: "with pictures",
		Attachments: &tw.TweetAttachmentsObj{
			MediaKeys: []string{"3_photo", "13_video", "16_gif"},
		},
	}
	mediaByKey := map[string]*tw.MediaObj{
		"3_photo": {
			Key: "3_photo", Type: "photo",
			URL:     "https://pbs.twimg.com/media/photo.jpg",
			AltText: "a cat",
			Width:   1200,
			Height:  800,
		},
		"13_video": {
			Key: "13_video", Type: "video",
			PreviewImageURL: "https://pbs.twimg.com/media/video-poster.jpg",
			AltText:         "a clip",
		},
		"16_gif": {
			Key: "16_gif", Type: "animated_gif",
			URL:             "https://video.twimg.com/tweet_video/gif.mp4",
			PreviewImageURL: "https://pbs.twimg.com/media/gif-poster.jpg",
		},
	}

	post := adapter.toPost(tweet, mediaByKey)

	if len(post.Media) != 3 {
		t.Fatalf("got %d attachments, want 3", len(post.Media))
	}

	photo := post.Media[0]
	if photo.Kind != model.MediaImage || photo.URL != "https://pbs.twimg.com/media/photo.jpg" {
		t.Fatalf("photo = %+v", photo)
	}
	if photo.AltText != "a cat" || photo.Width != 1200 || photo.Height != 800 {
		t.Fatalf("photo metadata = %+v", photo)
	}

	// A video has no downloadable file through the API, so it is carried as a
	// video with only a poster frame.
	video := post.Media[1]
	if video.Kind != model.MediaVideo {
		t.Fatalf("video kind = %q", video.Kind)
	}
	if video.URL != "" {
		t.Fatalf("a video should not claim a downloadable URL, got %q", video.URL)
	}
	if video.PosterURL != "https://pbs.twimg.com/media/video-poster.jpg" {
		t.Fatalf("video poster = %q", video.PosterURL)
	}

	// An animated GIF is a real MP4 the bridge can transcode.
	gif := post.Media[2]
	if gif.Kind != model.MediaGIF || gif.URL != "https://video.twimg.com/tweet_video/gif.mp4" {
		t.Fatalf("gif = %+v", gif)
	}
}

func TestToPostIgnoresUnknownMediaKeys(t *testing.T) {
	adapter := &Adapter{}
	tweet := &tw.TweetObj{
		ID:          "4",
		Text:        "dangling reference",
		Attachments: &tw.TweetAttachmentsObj{MediaKeys: []string{"missing"}},
	}

	post := adapter.toPost(tweet, map[string]*tw.MediaObj{})

	if len(post.Media) != 0 {
		t.Fatalf("expected no attachments, got %d", len(post.Media))
	}
}

func TestParseTime(t *testing.T) {
	if got := parseTime("2024-05-01T10:00:00.000Z"); got.UTC() != time.Date(2024, 5, 1, 10, 0, 0, 0, time.UTC) {
		t.Fatalf("parseTime = %v", got)
	}
	// Unparseable or empty timestamps fall back to now rather than a zero time.
	if got := parseTime(""); got.IsZero() {
		t.Fatal("an empty timestamp should fall back to the current time")
	}
	if got := parseTime("nonsense"); got.IsZero() {
		t.Fatal("an invalid timestamp should fall back to the current time")
	}
}

func TestNewRequiresUserID(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("a user id is required")
	}
}

func TestNewAcceptsBearerCredentials(t *testing.T) {
	adapter, err := New(Options{Config: bearerConfig()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if adapter.Name() != model.PlatformTwitter {
		t.Fatalf("name = %q", adapter.Name())
	}
	if _, ok := adapter.client.Authorizer.(bearerAuthorizer); !ok {
		t.Fatalf("expected bearer auth, got %T", adapter.client.Authorizer)
	}
}

func TestNewAcceptsOAuth1Credentials(t *testing.T) {
	adapter, err := New(Options{Config: oauth1Config()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// OAuth 1.0a is applied by the HTTP transport, so the authorizer itself is
	// a no-op.
	if _, ok := adapter.client.Authorizer.(oauth1Authorizer); !ok {
		t.Fatalf("expected oauth1 auth, got %T", adapter.client.Authorizer)
	}
	if adapter.client.Client == nil || adapter.client.Host != "https://api.twitter.com" {
		t.Fatal("the client should be pointed at the X API")
	}
}

func TestBearerAuthorizerSetsHeader(t *testing.T) {
	req, err := newTestRequest()
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	bearerAuthorizer{token: "secret-token"}.Add(req)
	if got := req.Header.Get("Authorization"); got != "Bearer secret-token" {
		t.Fatalf("authorization header = %q", got)
	}
}

func TestOAuth1AuthorizerLeavesHeaderAlone(t *testing.T) {
	req, err := newTestRequest()
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	oauth1Authorizer{}.Add(req)
	if got := req.Header.Get("Authorization"); got != "" {
		t.Fatalf("the oauth1 authorizer must not set a header, got %q", got)
	}
}
