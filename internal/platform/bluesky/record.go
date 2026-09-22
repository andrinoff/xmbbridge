package bluesky

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/andrinoff/xmbbridge/internal/model"
)

// feedPostRecord mirrors the parts of an app.bsky.feed.post record the bridge
// reads. Jetstream delivers records as plain JSON so only the needed fields
// need to be modelled.
type feedPostRecord struct {
	Type      string          `json:"$type"`
	Text      string          `json:"text"`
	CreatedAt string          `json:"createdAt"`
	Langs     []string        `json:"langs"`
	Reply     *replyRef       `json:"reply"`
	Embed     *embedContainer `json:"embed"`
}

type replyRef struct {
	Parent *recordRef `json:"parent"`
	Root   *recordRef `json:"root"`
}

type recordRef struct {
	URI string `json:"uri"`
	CID string `json:"cid"`
}

type embedContainer struct {
	Type        string           `json:"$type"`
	Alt         string           `json:"alt"`
	Images      []embedImage     `json:"images"`
	Video       *blobRef         `json:"video"`
	AspectRatio *aspectRatioJSON `json:"aspectRatio"`
}

type embedImage struct {
	Alt         string           `json:"alt"`
	Image       *blobRef         `json:"image"`
	AspectRatio *aspectRatioJSON `json:"aspectRatio"`
}

// blobRef is an AT Protocol blob: a CID link plus the media type and size.
type blobRef struct {
	Ref      *linkRef `json:"ref"`
	MimeType string   `json:"mimeType"`
	Size     int64    `json:"size"`
}

type linkRef struct {
	Link string `json:"$link"`
}

type aspectRatioJSON struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

// toPost converts a Bluesky post record into the bridge's neutral form.
func (a *Adapter) toPost(did, rkey string, raw json.RawMessage) (model.Post, error) {
	var record feedPostRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return model.Post{}, fmt.Errorf("parse feed post record: %w", err)
	}

	uri := fmt.Sprintf("at://%s/%s/%s", did, collectionPost, rkey)
	post := model.Post{
		Origin:    model.PlatformBluesky,
		OriginID:  uri,
		OriginURL: fmt.Sprintf("https://bsky.app/profile/%s/post/%s", did, rkey),
		Text:      record.Text,
		Lang:      firstLang(record.Langs),
	}
	if record.CreatedAt != "" {
		if created, err := time.Parse(time.RFC3339, record.CreatedAt); err == nil {
			post.CreatedAt = created
		}
	}
	if post.CreatedAt.IsZero() {
		post.CreatedAt = time.Now()
	}
	if record.Reply != nil && record.Reply.Parent != nil {
		post.IsReply = true
		post.ReplyToID = record.Reply.Parent.URI
	}

	if record.Embed != nil {
		for _, image := range record.Embed.Images {
			if image.Image == nil || image.Image.Ref == nil || image.Image.Ref.Link == "" {
				continue
			}
			attachment := model.Media{
				Kind:     model.MediaImage,
				URL:      a.blobURL(did, image.Image.Ref.Link),
				AltText:  image.Alt,
				MimeType: image.Image.MimeType,
			}
			if image.AspectRatio != nil {
				attachment.Width = image.AspectRatio.Width
				attachment.Height = image.AspectRatio.Height
			}
			post.Media = append(post.Media, attachment)
		}
		if video := record.Embed.Video; video != nil && video.Ref != nil && video.Ref.Link != "" {
			attachment := model.Media{
				Kind:     model.MediaVideo,
				URL:      a.blobURL(did, video.Ref.Link),
				AltText:  record.Embed.Alt,
				MimeType: video.MimeType,
			}
			if record.Embed.AspectRatio != nil {
				attachment.Width = record.Embed.AspectRatio.Width
				attachment.Height = record.Embed.AspectRatio.Height
			}
			post.Media = append(post.Media, attachment)
		}
	}

	return post, nil
}

// parseATURI splits at://<repo>/<collection>/<rkey> into its parts.
func parseATURI(uri string) (repo, collection, rkey string, err error) {
	rest, ok := strings.CutPrefix(uri, "at://")
	if !ok {
		return "", "", "", fmt.Errorf("not an at-uri: %q", uri)
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", fmt.Errorf("malformed at-uri: %q", uri)
	}
	return parts[0], parts[1], parts[2], nil
}

// bskyWebURL is unused by the pipeline but handy when debugging a post by hand.
func bskyWebURL(uri string) string {
	repo, _, rkey, err := parseATURI(uri)
	if err != nil {
		return uri
	}
	return fmt.Sprintf("https://bsky.app/profile/%s/post/%s", repo, rkey)
}

// withQuery returns raw with the given parameters applied.
func withQuery(raw string, params map[string]string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse url %q: %w", raw, err)
	}
	query := parsed.Query()
	for key, value := range params {
		if value != "" {
			query.Set(key, value)
		}
	}
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func firstLang(langs []string) string {
	for _, lang := range langs {
		if lang != "" {
			return lang
		}
	}
	return ""
}
