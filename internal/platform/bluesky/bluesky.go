// Package bluesky bridges posts to and from a Bluesky (AT Protocol) account.
//
// Posts are read from the public Jetstream relay, which turns the AT Protocol
// firehose into plain JSON and can be filtered down to a single repository.
// Posts are written with com.atproto.repo.createRecord.
package bluesky

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/andrinoff/xmbbridge/internal/config"
	"github.com/andrinoff/xmbbridge/internal/model"
	"github.com/andrinoff/xmbbridge/internal/platform"
	"github.com/andrinoff/xmbbridge/internal/text"

	"github.com/bluesky-social/indigo/api/atproto"
	"github.com/bluesky-social/indigo/api/bsky"
	"github.com/bluesky-social/indigo/lex/util"
	"github.com/bluesky-social/indigo/xrpc"
)

const (
	// TextMax is Bluesky's post length limit.
	TextMax = 300

	// collectionPost is the NSID of the record type used for posts.
	collectionPost = "app.bsky.feed.post"

	// maxImages is how many images a single Bluesky post may embed.
	maxImages = 4

	userAgent = "xmbbridge/1.0"

	// cursorFlushInterval is how many events may pass before the stream cursor
	// is written to disk. The relay replays unacknowledged events and the
	// engine dedupes them, so this trades a small replay for far fewer writes.
	cursorFlushInterval = 100

	// sessionRefreshInterval is comfortably inside the roughly two hour
	// lifetime of an access token.
	sessionRefreshInterval = 45 * time.Minute
)

// Options configures the adapter.
type Options struct {
	Config     config.BlueskyConfig
	Backfill   bool
	State      platform.State
	Logger     *slog.Logger
	HTTPClient *http.Client
}

// Adapter implements the bridge's source and target interfaces for Bluesky.
type Adapter struct {
	cfg        config.BlueskyConfig
	log        *slog.Logger
	state      platform.State
	backfill   bool
	httpClient *http.Client
	mediaBase  string

	mu   sync.RWMutex
	xrpc *xrpc.Client
	did  string
}

// New builds a Bluesky adapter and authenticates it.
func New(ctx context.Context, opts Options) (*Adapter, error) {
	if opts.Config.Handle == "" {
		return nil, fmt.Errorf("bluesky: handle is required")
	}
	if opts.Config.AppPassword == "" {
		return nil, fmt.Errorf("bluesky: app password is required")
	}
	if opts.Config.PDS == "" {
		return nil, fmt.Errorf("bluesky: pds is required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 2 * time.Minute}
	}
	a := &Adapter{
		cfg:        opts.Config,
		log:        opts.Logger,
		state:      opts.State,
		backfill:   opts.Backfill,
		httpClient: opts.HTTPClient,
		mediaBase:  strings.TrimSuffix(opts.Config.PDS, "/"),
	}
	if err := a.login(ctx); err != nil {
		return nil, err
	}
	return a, nil
}

// Name identifies the platform.
func (a *Adapter) Name() model.Platform { return model.PlatformBluesky }

// Check verifies the session is still valid with a real authenticated call. It
// is called by `bridge -check`.
func (a *Adapter) Check(ctx context.Context) error {
	session, err := atproto.ServerGetSession(ctx, a.client())
	if err != nil {
		return fmt.Errorf("verify session on %s: %w", a.cfg.PDS, err)
	}
	a.log.Info("bluesky: credentials OK", "handle", session.Handle, "did", session.Did, "pds", a.cfg.PDS)
	return nil
}

// Limits reports what a Bluesky account accepts.
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

// DID returns the repository identifier of the authenticated account.
func (a *Adapter) DID() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.did
}

func (a *Adapter) client() *xrpc.Client {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.xrpc
}

func (a *Adapter) setSession(host, handle, did, accessToken, refreshToken string) {
	ua := userAgent
	client := &xrpc.Client{
		Client:    a.httpClient,
		Host:      strings.TrimSuffix(host, "/"),
		UserAgent: &ua,
		Auth: &xrpc.AuthInfo{
			Handle:     handle,
			Did:        did,
			AccessJwt:  accessToken,
			RefreshJwt: refreshToken,
		},
	}
	a.mu.Lock()
	a.xrpc = client
	a.did = did
	a.mu.Unlock()
}

// login creates a fresh session from the handle and app password.
func (a *Adapter) login(ctx context.Context) error {
	base := &xrpc.Client{
		Client: a.httpClient,
		Host:   strings.TrimSuffix(a.cfg.PDS, "/"),
		Auth:   &xrpc.AuthInfo{Handle: a.cfg.Handle},
	}
	session, err := atproto.ServerCreateSession(ctx, base, &atproto.ServerCreateSession_Input{
		Identifier: a.cfg.Handle,
		Password:   a.cfg.AppPassword,
	})
	if err != nil {
		return fmt.Errorf("bluesky: create session for %s: %w", a.cfg.Handle, err)
	}
	a.setSession(a.cfg.PDS, session.Handle, session.Did, session.AccessJwt, session.RefreshJwt)
	a.log.Debug("bluesky: session created", "handle", session.Handle, "did", session.Did)
	return nil
}

// refreshLoop keeps the access token valid for the life of the process. If a
// refresh fails it falls back to a full login, which always works because app
// passwords do not expire.
func (a *Adapter) refreshLoop(ctx context.Context) {
	ticker := time.NewTicker(sessionRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := a.refresh(ctx); err != nil && ctx.Err() == nil {
				a.log.Warn("bluesky: session refresh failed, logging in again", "error", err)
				if err := a.login(ctx); err != nil && ctx.Err() == nil {
					a.log.Error("bluesky: re-login failed", "error", err)
				}
			}
		}
	}
}

func (a *Adapter) refresh(ctx context.Context) error {
	client := a.client()
	session, err := atproto.ServerRefreshSession(ctx, client)
	if err != nil {
		return err
	}
	a.setSession(a.cfg.PDS, session.Handle, session.Did, session.AccessJwt, session.RefreshJwt)
	return nil
}

// Post publishes an already-prepared post and returns the new record URI.
func (a *Adapter) Post(ctx context.Context, out model.Outbound, media []model.PreparedMedia) (string, error) {
	record := &bsky.FeedPost{
		CreatedAt: createdAt(out.CreatedAt),
		Text:      out.Text,
		Facets:    linkFacets(out.Text),
	}
	if out.Lang != "" {
		record.Langs = []string{out.Lang}
	}

	if out.IsReply && out.ParentTargetID != "" {
		ref, err := a.strongRef(ctx, out.ParentTargetID)
		if err != nil {
			return "", err
		}
		// A single-level thread uses the parent as its own root. Deeper
		// threads still display correctly because the indexer follows the
		// parent chain.
		record.Reply = &bsky.FeedPost_ReplyRef{Parent: ref, Root: ref}
	}

	embed, err := a.buildEmbed(ctx, media)
	if err != nil {
		return "", err
	}
	record.Embed = embed

	result, err := atproto.RepoCreateRecord(ctx, a.client(), &atproto.RepoCreateRecord_Input{
		Repo:       a.DID(),
		Collection: collectionPost,
		Record:     &util.LexiconTypeDecoder{Val: record},
	})
	if err != nil {
		return "", fmt.Errorf("bluesky: create record: %w", err)
	}
	return result.Uri, nil
}

// linkFacets annotates every URL in the post text so Bluesky clients render
// it as a clickable hyperlink. Without facets the text stays inert: Bluesky
// does not linkify plain text on its own. Facet offsets are byte positions
// into the UTF-8 text, which FindLinks reports directly.
func linkFacets(body string) []*bsky.RichtextFacet {
	links := text.FindLinks(body)
	if len(links) == 0 {
		return nil
	}
	facets := make([]*bsky.RichtextFacet, 0, len(links))
	for _, link := range links {
		facets = append(facets, &bsky.RichtextFacet{
			Index: &bsky.RichtextFacet_ByteSlice{
				ByteStart: int64(link.Start),
				ByteEnd:   int64(link.End),
			},
			Features: []*bsky.RichtextFacet_Features_Elem{
				{RichtextFacet_Link: &bsky.RichtextFacet_Link{Uri: link.URL}},
			},
		})
	}
	return facets
}

// buildEmbed uploads attachments and assembles the post's embed. Video wins
// over images because Bluesky does not allow both in one post.
func (a *Adapter) buildEmbed(ctx context.Context, media []model.PreparedMedia) (*bsky.FeedPost_Embed, error) {
	var images []model.PreparedMedia
	var video *model.PreparedMedia

	for i := range media {
		item := media[i]
		switch item.Kind {
		case model.MediaImage:
			if len(images) < maxImages {
				images = append(images, item)
			}
		case model.MediaVideo, model.MediaGIF:
			if video == nil {
				video = &item
			}
		}
	}

	if video != nil {
		blob, err := a.uploadBlob(ctx, *video)
		if err != nil {
			return nil, fmt.Errorf("upload video: %w", err)
		}
		embed := &bsky.EmbedVideo{Video: blob}
		if video.AltText != "" {
			alt := video.AltText
			embed.Alt = &alt
		}
		if ar := aspectRatio(video.Width, video.Height); ar != nil {
			embed.AspectRatio = ar
		}
		return &bsky.FeedPost_Embed{EmbedVideo: embed}, nil
	}

	if len(images) == 0 {
		return nil, nil
	}

	embedImages := &bsky.EmbedImages{Images: make([]*bsky.EmbedImages_Image, 0, len(images))}
	for _, item := range images {
		blob, err := a.uploadBlob(ctx, item)
		if err != nil {
			return nil, fmt.Errorf("upload image: %w", err)
		}
		embedImages.Images = append(embedImages.Images, &bsky.EmbedImages_Image{
			Alt:         item.AltText,
			Image:       blob,
			AspectRatio: aspectRatio(item.Width, item.Height),
		})
	}
	return &bsky.FeedPost_Embed{EmbedImages: embedImages}, nil
}

func (a *Adapter) uploadBlob(ctx context.Context, item model.PreparedMedia) (*util.LexBlob, error) {
	result, err := atproto.RepoUploadBlob(ctx, a.client(), bytes.NewReader(item.Bytes))
	if err != nil {
		return nil, fmt.Errorf("bluesky: upload blob: %w", err)
	}
	if result.Blob == nil {
		return nil, fmt.Errorf("bluesky: upload blob returned no blob")
	}
	return result.Blob, nil
}

// strongRef resolves a record URI into the URI and CID pair a reply needs.
func (a *Adapter) strongRef(ctx context.Context, uri string) (*atproto.RepoStrongRef, error) {
	repo, collection, rkey, err := parseATURI(uri)
	if err != nil {
		return nil, err
	}
	record, err := atproto.RepoGetRecord(ctx, a.client(), "", collection, repo, rkey)
	if err != nil {
		return nil, fmt.Errorf("bluesky: resolve reply parent %s: %w", uri, err)
	}
	if record.Cid == nil || *record.Cid == "" {
		return nil, fmt.Errorf("bluesky: reply parent %s has no cid", uri)
	}
	return &atproto.RepoStrongRef{Uri: record.Uri, Cid: *record.Cid}, nil
}

// blobURL builds the public endpoint a blob can be downloaded from. Blobs of
// the authenticated account live on our own PDS.
func (a *Adapter) blobURL(did, cid string) string {
	return fmt.Sprintf("%s/xrpc/com.atproto.sync.getBlob?did=%s&cid=%s",
		a.mediaBase, url.QueryEscape(did), url.QueryEscape(cid))
}

func createdAt(t time.Time) string {
	if t.IsZero() {
		t = time.Now()
	}
	return t.UTC().Format(time.RFC3339)
}

func aspectRatio(width, height int) *bsky.EmbedDefs_AspectRatio {
	if width <= 0 || height <= 0 {
		return nil
	}
	return &bsky.EmbedDefs_AspectRatio{Width: int64(width), Height: int64(height)}
}
