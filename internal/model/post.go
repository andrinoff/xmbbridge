// Package model defines the platform-agnostic representation of a post as it
// travels through the bridge.
package model

import (
	"fmt"
	"slices"
	"time"
)

// Platform identifies a social network the bridge can read from or write to.
type Platform string

const (
	PlatformMastodon Platform = "mastodon"
	PlatformBluesky  Platform = "bluesky"
	PlatformTwitter  Platform = "twitter"
)

// AllPlatforms lists every platform the bridge understands, in a stable order.
var AllPlatforms = []Platform{PlatformMastodon, PlatformBluesky, PlatformTwitter}

// Valid reports whether p is a platform the bridge understands.
func (p Platform) Valid() bool {
	return slices.Contains(AllPlatforms, p)
}

func (p Platform) String() string { return string(p) }

// MediaKind classifies an attachment. Video and GIF are treated the same way
// by the outbound pipeline (both need transcoding); they are kept distinct so
// logs and fallbacks can tell them apart.
type MediaKind string

const (
	MediaImage MediaKind = "image"
	MediaVideo MediaKind = "video"
	MediaGIF   MediaKind = "gif"
	MediaAudio MediaKind = "audio"
)

// Media is a single attachment on a post.
type Media struct {
	Kind     MediaKind
	URL      string
	AltText  string
	MimeType string
	Width    int
	Height   int

	// PosterURL is a still image standing in for a video. The bridge falls
	// back to it when the target platform cannot accept the video itself.
	PosterURL string
}

// PreparedMedia is an attachment that has been fetched and re-encoded to fit a
// specific target platform, ready to be uploaded.
type PreparedMedia struct {
	Kind     MediaKind
	Bytes    []byte
	MimeType string
	AltText  string
	Width    int
	Height   int
	Filename string
}

// Limits describes what a target platform accepts. The engine uses it to
// decide how hard to compress media and how far to truncate text.
type Limits struct {
	TextMax           int
	ImageMaxBytes     int
	ImageMaxDimension int
	VideoMaxBytes     int
	VideoMaxDuration  time.Duration
	Video             bool
}

// Post is one post read from an origin platform, normalised so the engine can
// fan it out without knowing platform specifics.
type Post struct {
	Origin    Platform
	OriginID  string
	OriginURL string
	Text      string
	CreatedAt time.Time
	Lang      string
	Media     []Media

	// IsReply marks posts that are responses to another post. The bridge
	// skips them unless the operator opts in.
	IsReply bool

	// ReplyToID is the origin-side identifier of the parent post, when known.
	// It lets the bridge link bridged replies back to already-bridged parents.
	ReplyToID string
}

// Outbound is a post prepared for one specific target platform: its text has
// already been trimmed to the target's limit and, when the post is a reply,
// ParentTargetID carries the identifier of the already-bridged parent so the
// reply can be threaded rather than posted loose.
type Outbound struct {
	Origin         Platform
	OriginID       string
	Text           string
	Lang           string
	CreatedAt      time.Time
	IsReply        bool
	ParentTargetID string
}

// Ref returns a human-readable origin reference used in logs and errors.
func (p Post) Ref() string {
	return fmt.Sprintf("%s/%s", p.Origin, p.OriginID)
}
