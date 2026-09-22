package bluesky

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/andrinoff/xmbbridge/internal/config"
	"github.com/andrinoff/xmbbridge/internal/model"
)

const testDID = "did:plc:abcdefghijklmnopqrstuvwx"

func testAdapter() *Adapter {
	return &Adapter{mediaBase: "https://bsky.social"}
}

func blueskyConfigWithLimits() config.BlueskyConfig {
	return config.BlueskyConfig{
		PlatformLimits: config.PlatformLimits{
			MaxImageBytes:    111,
			MaxVideoBytes:    222,
			MaxImageDim:      333,
			MaxVideoDuration: config.Duration(30 * time.Second),
			Video:            true,
		},
	}
}

func TestToPostParsesPlainText(t *testing.T) {
	adapter := testAdapter()
	record := []byte(`{
		"$type": "app.bsky.feed.post",
		"text": "hello from bluesky",
		"createdAt": "2024-05-01T10:00:00.000Z",
		"langs": ["en", "de"]
	}`)

	post, err := adapter.toPost(testDID, "3kabc", record)
	if err != nil {
		t.Fatalf("toPost: %v", err)
	}

	if post.Origin != model.PlatformBluesky {
		t.Fatalf("origin = %q", post.Origin)
	}
	wantURI := "at://" + testDID + "/app.bsky.feed.post/3kabc"
	if post.OriginID != wantURI {
		t.Fatalf("origin id = %q, want %q", post.OriginID, wantURI)
	}
	if post.Text != "hello from bluesky" {
		t.Fatalf("text = %q", post.Text)
	}
	if post.Lang != "en" {
		t.Fatalf("lang = %q, want the first language", post.Lang)
	}
	if post.CreatedAt.UTC() != time.Date(2024, 5, 1, 10, 0, 0, 0, time.UTC) {
		t.Fatalf("created at = %v", post.CreatedAt)
	}
	if post.IsReply {
		t.Fatal("a standalone post is not a reply")
	}
}

func TestToPostDetectsReply(t *testing.T) {
	adapter := testAdapter()
	record := []byte(`{
		"$type": "app.bsky.feed.post",
		"text": "a reply",
		"createdAt": "2024-05-01T10:00:00.000Z",
		"reply": {
			"parent": {"uri": "at://did:plc:x/app.bsky.feed.post/parent", "cid": "bafyparent"},
			"root": {"uri": "at://did:plc:x/app.bsky.feed.post/root", "cid": "bafyroot"}
		}
	}`)

	post, err := adapter.toPost(testDID, "3kreply", record)
	if err != nil {
		t.Fatalf("toPost: %v", err)
	}
	if !post.IsReply {
		t.Fatal("a post with a reply reference is a reply")
	}
	if post.ReplyToID != "at://did:plc:x/app.bsky.feed.post/parent" {
		t.Fatalf("reply parent = %q", post.ReplyToID)
	}
}

func TestToPostExtractsImageBlobs(t *testing.T) {
	adapter := testAdapter()
	record := []byte(`{
		"$type": "app.bsky.feed.post",
		"text": "two pictures",
		"createdAt": "2024-05-01T10:00:00.000Z",
		"embed": {
			"$type": "app.bsky.embed.images",
			"images": [
				{
					"alt": "a cat",
					"image": {"$type": "blob", "ref": {"$link": "bafkrei1"}, "mimeType": "image/jpeg", "size": 1234},
					"aspectRatio": {"width": 800, "height": 600}
				},
				{
					"alt": "",
					"image": {"$type": "blob", "ref": {"$link": "bafkrei2"}, "mimeType": "image/png", "size": 4321}
				}
			]
		}
	}`)

	post, err := adapter.toPost(testDID, "3kimg", record)
	if err != nil {
		t.Fatalf("toPost: %v", err)
	}
	if len(post.Media) != 2 {
		t.Fatalf("got %d attachments, want 2", len(post.Media))
	}

	first := post.Media[0]
	if first.Kind != model.MediaImage {
		t.Fatalf("kind = %q", first.Kind)
	}
	if first.AltText != "a cat" {
		t.Fatalf("alt = %q", first.AltText)
	}
	if first.Width != 800 || first.Height != 600 {
		t.Fatalf("dimensions = %dx%d", first.Width, first.Height)
	}
	// Bluesky blobs are content addressed, so the URL has to point at the
	// blob endpoint of the origin PDS.
	wantURL := "https://bsky.social/xrpc/com.atproto.sync.getBlob?did=did%3Aplc%3Aabcdefghijklmnopqrstuvwx&cid=bafkrei1"
	if first.URL != wantURL {
		t.Fatalf("blob url = %q, want %q", first.URL, wantURL)
	}
	if post.Media[1].URL == first.URL {
		t.Fatal("each image should map to its own blob CID")
	}
}

func TestToPostExtractsVideoBlob(t *testing.T) {
	adapter := testAdapter()
	record := []byte(`{
		"$type": "app.bsky.feed.post",
		"text": "a clip",
		"createdAt": "2024-05-01T10:00:00.000Z",
		"embed": {
			"$type": "app.bsky.embed.video",
			"alt": "a short clip",
			"video": {"$type": "blob", "ref": {"$link": "bafyvideo"}, "mimeType": "video/mp4", "size": 99999},
			"aspectRatio": {"width": 1280, "height": 720}
		}
	}`)

	post, err := adapter.toPost(testDID, "3kvid", record)
	if err != nil {
		t.Fatalf("toPost: %v", err)
	}
	if len(post.Media) != 1 {
		t.Fatalf("got %d attachments, want 1", len(post.Media))
	}
	video := post.Media[0]
	if video.Kind != model.MediaVideo {
		t.Fatalf("kind = %q", video.Kind)
	}
	if video.AltText != "a short clip" {
		t.Fatalf("alt = %q", video.AltText)
	}
	if video.MimeType != "video/mp4" {
		t.Fatalf("mime = %q", video.MimeType)
	}
	if video.Width != 1280 || video.Height != 720 {
		t.Fatalf("dimensions = %dx%d", video.Width, video.Height)
	}
}

func TestToPostIgnoresUnusableBlobs(t *testing.T) {
	adapter := testAdapter()
	record := []byte(`{
		"$type": "app.bsky.feed.post",
		"text": "broken embed",
		"createdAt": "2024-05-01T10:00:00.000Z",
		"embed": {"$type": "app.bsky.embed.images", "images": [{"alt": "", "image": null}]}
	}`)

	post, err := adapter.toPost(testDID, "3kbad", record)
	if err != nil {
		t.Fatalf("toPost: %v", err)
	}
	if len(post.Media) != 0 {
		t.Fatalf("expected no attachments, got %d", len(post.Media))
	}
}

func TestToPostRejectsMalformedRecord(t *testing.T) {
	adapter := testAdapter()
	if _, err := adapter.toPost(testDID, "3kbad", []byte(`{not json`)); err == nil {
		t.Fatal("expected an error for a malformed record")
	}
}

func TestParseATURI(t *testing.T) {
	repo, collection, rkey, err := parseATURI("at://did:plc:abc/app.bsky.feed.post/3kxyz")
	if err != nil {
		t.Fatalf("parseATURI: %v", err)
	}
	if repo != "did:plc:abc" || collection != "app.bsky.feed.post" || rkey != "3kxyz" {
		t.Fatalf("parsed %q %q %q", repo, collection, rkey)
	}

	for _, bad := range []string{"", "https://bsky.app/x", "at://only-did", "at://a/b", "at://a/b/c/d"} {
		if _, _, _, err := parseATURI(bad); err == nil {
			t.Errorf("parseATURI(%q) should fail", bad)
		}
	}
}

func TestJetstreamEventCursorPrefersSequence(t *testing.T) {
	withSeq := jetstreamEvent{Cursor: 12345, TimeUS: 1700000000000000}
	if got := withSeq.cursor(); got != "12345" {
		t.Fatalf("cursor = %q, want the sequence number", got)
	}

	legacy := jetstreamEvent{TimeUS: 1700000000000000}
	if got := legacy.cursor(); got != "1700000000000000" {
		t.Fatalf("cursor = %q, want the microsecond timestamp", got)
	}

	if got := (jetstreamEvent{}).cursor(); got != "" {
		t.Fatalf("an empty event should have no cursor, got %q", got)
	}
}

func TestDecodeJetstreamCommitEvent(t *testing.T) {
	frame := []byte(`{
		"did": "` + testDID + `",
		"time_us": 1714557600000000,
		"kind": "commit",
		"commit": {
			"rev": "3kabc",
			"operation": "create",
			"collection": "app.bsky.feed.post",
			"rkey": "3krkey",
			"record": {"$type": "app.bsky.feed.post", "text": "hi", "createdAt": "2024-05-01T10:00:00.000Z"},
			"cid": "bafyrecord"
		}
	}`)

	event, err := decodeJetstreamEvent(frame)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if event.DID != testDID {
		t.Fatalf("did = %q", event.DID)
	}
	if event.Kind != "commit" {
		t.Fatalf("kind = %q", event.Kind)
	}
	if event.Commit == nil {
		t.Fatal("commit should be present")
	}
	if event.Commit.Operation != "create" || event.Commit.Collection != "app.bsky.feed.post" {
		t.Fatalf("commit = %+v", event.Commit)
	}
	if event.Commit.Rkey != "3krkey" {
		t.Fatalf("rkey = %q", event.Commit.Rkey)
	}

	// The embedded record must survive as raw JSON for toPost to read.
	var record struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(event.Commit.Record, &record); err != nil {
		t.Fatalf("unmarshal record: %v", err)
	}
	if record.Text != "hi" {
		t.Fatalf("record text = %q", record.Text)
	}
}

func TestDecodeJetstreamMarkerEvent(t *testing.T) {
	// Account and identity markers carry no commit and must not panic.
	frame := []byte(`{"did":"` + testDID + `","kind":"identity","identity":{"did":"` + testDID + `","handle":"me.test","seq":1,"time":"2024-05-01T10:00:00Z"}}`)
	event, err := decodeJetstreamEvent(frame)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if event.Commit != nil {
		t.Fatal("a marker event should not carry a commit")
	}

	if _, err := decodeJetstreamEvent([]byte(`not json`)); err == nil {
		t.Fatal("expected an error for a malformed frame")
	}
}

func TestWithQuery(t *testing.T) {
	got, err := withQuery("wss://jetstream.example/subscribe", map[string]string{
		"wantedDids":        testDID,
		"wantedCollections": "app.bsky.feed.post",
		"cursor":            "",
	})
	if err != nil {
		t.Fatalf("withQuery: %v", err)
	}
	want := "wss://jetstream.example/subscribe?wantedCollections=app.bsky.feed.post&wantedDids=did%3Aplc%3Aabcdefghijklmnopqrstuvwx"
	if got != want {
		t.Fatalf("withQuery = %q, want %q (empty values are omitted)", got, want)
	}

	// An existing cursor on the URL is preserved and overridden only when set.
	got, err = withQuery("wss://jetstream.example/subscribe?cursor=5", map[string]string{"cursor": "9"})
	if err != nil {
		t.Fatalf("withQuery: %v", err)
	}
	want = "wss://jetstream.example/subscribe?cursor=9"
	if got != want {
		t.Fatalf("withQuery = %q, want %q", got, want)
	}

	if _, err := withQuery("://bad", nil); err == nil {
		t.Fatal("expected an error for an unparseable URL")
	}
}

func TestLimitsReflectConfiguration(t *testing.T) {
	adapter := &Adapter{
		cfg: blueskyConfigWithLimits(),
	}
	limits := adapter.Limits()

	if limits.TextMax != TextMax {
		t.Fatalf("text max = %d, want %d", limits.TextMax, TextMax)
	}
	if limits.ImageMaxBytes != 111 {
		t.Fatalf("image bytes = %d", limits.ImageMaxBytes)
	}
	if limits.VideoMaxBytes != 222 {
		t.Fatalf("video bytes = %d", limits.VideoMaxBytes)
	}
	if limits.VideoMaxDuration != 30*time.Second {
		t.Fatalf("video duration = %v", limits.VideoMaxDuration)
	}
}
