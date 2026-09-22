package bluesky

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/bluesky-social/indigo/api/atproto"
	"github.com/bluesky-social/indigo/api/bsky"
	"github.com/bluesky-social/indigo/lex/util"
	cid "github.com/ipfs/go-cid"
)

// TestCreateRecordMarshalling verifies the exact JSON the PDS receives. The
// indigo types only emit their $type fields through these code paths, so this
// is the check that a created post would be accepted.
func TestCreateRecordMarshalling(t *testing.T) {
	record := &bsky.FeedPost{
		CreatedAt: "2024-05-01T10:00:00Z",
		Text:      "hello bluesky",
		Langs:     []string{"en"},
	}
	input := &atproto.RepoCreateRecord_Input{
		Repo:       testDID,
		Collection: collectionPost,
		Record:     &util.LexiconTypeDecoder{Val: record},
	}

	body, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out := string(body)

	if !strings.Contains(out, `"$type":"app.bsky.feed.post"`) {
		t.Fatalf("record is missing its $type: %s", out)
	}
	if !strings.Contains(out, `"repo":"`+testDID+`"`) {
		t.Fatalf("repo is missing: %s", out)
	}
	if !strings.Contains(out, `"collection":"app.bsky.feed.post"`) {
		t.Fatalf("collection is missing: %s", out)
	}
	if !strings.Contains(out, `"text":"hello bluesky"`) {
		t.Fatalf("text is missing: %s", out)
	}
}

var (
	// Realistic, well-formed CIDv1 strings; go-cid rejects anything else.
	imageCID = "bafkreigh2akiscaildcqabsyg3dfr7chuoragxor7qkl3jkxb4ee7n3nsu"
	videoCID = "bafybeigdyrzt5sfp7udm7hu76uh7y26nf3efuylqabf3oclgtqy55fbzdi"
)

func TestCreateRecordMarshallingWithImageEmbed(t *testing.T) {
	image := &bsky.EmbedImages{
		Images: []*bsky.EmbedImages_Image{
			{
				Alt:   "a cat",
				Image: &util.LexBlob{Ref: blobLink(imageCID), MimeType: "image/jpeg", Size: 1234},
			},
		},
	}
	record := &bsky.FeedPost{
		CreatedAt: "2024-05-01T10:00:00Z",
		Text:      "with a picture",
		Embed:     &bsky.FeedPost_Embed{EmbedImages: image},
	}
	input := &atproto.RepoCreateRecord_Input{
		Repo:       testDID,
		Collection: collectionPost,
		Record:     &util.LexiconTypeDecoder{Val: record},
	}

	body, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out := string(body)

	for _, want := range []string{
		`"$type":"app.bsky.embed.images"`,
		`"alt":"a cat"`,
		`"$link":"` + imageCID + `"`,
		`"mimeType":"image/jpeg"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("embed is missing %s: %s", want, out)
		}
	}
}

func TestCreateRecordMarshallingWithVideoEmbed(t *testing.T) {
	alt := "a clip"
	record := &bsky.FeedPost{
		CreatedAt: "2024-05-01T10:00:00Z",
		Text:      "with a video",
		Embed: &bsky.FeedPost_Embed{
			EmbedVideo: &bsky.EmbedVideo{
				Video: &util.LexBlob{Ref: blobLink(videoCID), MimeType: "video/mp4", Size: 9000},
				Alt:   &alt,
			},
		},
	}
	input := &atproto.RepoCreateRecord_Input{
		Repo:       testDID,
		Collection: collectionPost,
		Record:     &util.LexiconTypeDecoder{Val: record},
	}

	body, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(body), `"$type":"app.bsky.embed.video"`) {
		t.Fatalf("video embed is missing its $type: %s", string(body))
	}
}

func TestCreateRecordMarshallingWithReply(t *testing.T) {
	ref := &atproto.RepoStrongRef{Uri: "at://did:plc:x/app.bsky.feed.post/parent", Cid: "bafyparent"}
	record := &bsky.FeedPost{
		CreatedAt: "2024-05-01T10:00:00Z",
		Text:      "a reply",
		Reply:     &bsky.FeedPost_ReplyRef{Parent: ref, Root: ref},
	}
	input := &atproto.RepoCreateRecord_Input{
		Repo:       testDID,
		Collection: collectionPost,
		Record:     &util.LexiconTypeDecoder{Val: record},
	}

	body, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(body), `"reply"`) || !strings.Contains(string(body), "bafyparent") {
		t.Fatalf("reply reference is missing: %s", string(body))
	}
}

func TestCreatedAtFallsBackToNow(t *testing.T) {
	before := time.Now().Add(-time.Second).UTC().Format(time.RFC3339)
	got := createdAt(time.Time{})
	if got < before {
		t.Fatalf("a zero time should become the current time, got %q", got)
	}
	if got != createdAt(time.Now().UTC()) {
		// not exact equality, just a sanity check that it parses
	}
	if _, err := time.Parse(time.RFC3339, got); err != nil {
		t.Fatalf("createdAt is not RFC 3339: %v", err)
	}
}

// blobLink parses a CID string into the link type indigo expects.
func blobLink(cidString string) util.LexLink {
	parsed, err := cid.Decode(cidString)
	if err != nil {
		panic(err)
	}
	return util.LexLink(parsed)
}

func TestAspectRatio(t *testing.T) {
	if ar := aspectRatio(0, 0); ar != nil {
		t.Fatal("zero dimensions should produce no aspect ratio")
	}
	ar := aspectRatio(800, 450)
	if ar == nil || ar.Width != 800 || ar.Height != 450 {
		t.Fatalf("aspect ratio = %+v", ar)
	}
}
