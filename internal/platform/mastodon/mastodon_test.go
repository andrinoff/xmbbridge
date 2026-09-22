package mastodon

import (
	"testing"
	"time"

	"github.com/andrinoff/xmbbridge/internal/config"
	"github.com/andrinoff/xmbbridge/internal/model"

	"github.com/mattn/go-mastodon"
)

func configWithLimits() config.MastodonConfig {
	return config.MastodonConfig{
		PlatformLimits: config.PlatformLimits{
			MaxImageBytes: 12345,
			MaxVideoBytes: 67890,
			MaxImageDim:   999,
			Video:         true,
		},
	}
}

func TestToPostStripsHTML(t *testing.T) {
	adapter := &Adapter{}
	status := &mastodon.Status{
		ID:        "111",
		URL:       "https://mastodon.example/@me/111",
		Content:   "<p>hello <a href=\"https://example.com\">world</a></p>",
		CreatedAt: time.Date(2024, 5, 1, 10, 0, 0, 0, time.UTC),
		Language:  "en",
	}

	post := adapter.toPost(status)

	if post.Origin != model.PlatformMastodon {
		t.Fatalf("origin = %q", post.Origin)
	}
	if post.OriginID != "111" {
		t.Fatalf("origin id = %q", post.OriginID)
	}
	if post.Text != "hello world" {
		t.Fatalf("text = %q, want the HTML stripped", post.Text)
	}
	if post.Lang != "en" {
		t.Fatalf("lang = %q", post.Lang)
	}
	if post.IsReply {
		t.Fatal("a top-level status is not a reply")
	}
}

func TestToPostDetectsRepliesAndBoosts(t *testing.T) {
	adapter := &Adapter{}

	reply := adapter.toPost(&mastodon.Status{
		ID:          "2",
		Content:     "<p>a reply</p>",
		InReplyToID: "1",
	})
	if !reply.IsReply {
		t.Fatal("a status with in_reply_to_id is a reply")
	}
	if reply.ReplyToID != "1" {
		t.Fatalf("reply parent = %q, want 1", reply.ReplyToID)
	}

	// Boosts carry a nested status; the caller filters them out, but the
	// mapping itself must not claim to be a reply.
	boost := adapter.toPost(&mastodon.Status{
		ID:      "3",
		Content: "",
		Reblog:  &mastodon.Status{ID: "1", Content: "<p>someone else</p>"},
	})
	if boost.IsReply {
		t.Fatal("a boost is not a reply")
	}
}

func TestToPostCarriesAttachments(t *testing.T) {
	adapter := &Adapter{}
	status := &mastodon.Status{
		ID:      "9",
		Content: "<p>pictures</p>",
		MediaAttachments: []mastodon.Attachment{
			{
				Type:        "image",
				URL:         "https://mastodon.example/media/1.jpg",
				PreviewURL:  "https://mastodon.example/media/1-small.jpg",
				Description: "a cat",
				Meta:        mastodon.AttachmentMeta{Original: mastodon.AttachmentSize{Width: 800, Height: 600}},
			},
			{
				Type:        "video",
				URL:         "https://mastodon.example/media/2.mp4",
				PreviewURL:  "https://mastodon.example/media/2-poster.jpg",
				Description: "a clip",
			},
			{
				Type: "gifv",
				URL:  "https://mastodon.example/media/3.mp4",
			},
		},
	}

	post := adapter.toPost(status)

	if len(post.Media) != 3 {
		t.Fatalf("got %d attachments, want 3", len(post.Media))
	}
	if post.Media[0].Kind != model.MediaImage || post.Media[0].URL != "https://mastodon.example/media/1.jpg" {
		t.Fatalf("first attachment = %+v", post.Media[0])
	}
	if post.Media[0].AltText != "a cat" {
		t.Fatalf("alt text = %q", post.Media[0].AltText)
	}
	if post.Media[0].Width != 800 || post.Media[0].Height != 600 {
		t.Fatalf("dimensions = %dx%d", post.Media[0].Width, post.Media[0].Height)
	}
	if post.Media[1].Kind != model.MediaVideo || post.Media[1].PosterURL == "" {
		t.Fatalf("video attachment = %+v", post.Media[1])
	}
	if post.Media[2].Kind != model.MediaGIF {
		t.Fatalf("gifv should map to the gif kind, got %q", post.Media[2].Kind)
	}
}

func TestMediaFromAttachmentKinds(t *testing.T) {
	tests := map[string]model.MediaKind{
		"image": model.MediaImage,
		"video": model.MediaVideo,
		"gifv":  model.MediaGIF,
		"audio": model.MediaAudio,
		"":      model.MediaImage,
	}
	for input, want := range tests {
		got := mediaFromAttachment(mastodon.Attachment{Type: input}).Kind
		if got != want {
			t.Errorf("type %q mapped to %q, want %q", input, got, want)
		}
	}
}

func TestLimitsReflectConfiguration(t *testing.T) {
	adapter := &Adapter{cfg: configWithLimits()}
	limits := adapter.Limits()

	if limits.TextMax != TextMax {
		t.Fatalf("text max = %d", limits.TextMax)
	}
	if limits.ImageMaxBytes != 12345 {
		t.Fatalf("image bytes = %d", limits.ImageMaxBytes)
	}
	if limits.VideoMaxBytes != 67890 {
		t.Fatalf("video bytes = %d", limits.VideoMaxBytes)
	}
	if !limits.Video {
		t.Fatal("video should be enabled")
	}
}
