package bluesky

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bluesky-social/indigo/api/atproto"
	"github.com/bluesky-social/indigo/api/bsky"
	"github.com/bluesky-social/indigo/lex/util"
)

func TestLinkFacetsCoverEachURL(t *testing.T) {
	body := "h\u00e9llo \U0001F47E read https://example.com/a/b and https://example.org"

	facets := linkFacets(body)

	if len(facets) != 2 {
		t.Fatalf("got %d facets, want 2", len(facets))
	}
	for i, facet := range facets {
		if facet.Index == nil {
			t.Fatalf("facet %d has no index", i)
		}
		if len(facet.Features) != 1 || facet.Features[0].RichtextFacet_Link == nil {
			t.Fatalf("facet %d is not a link: %+v", i, facet.Features)
		}
		link := facet.Features[0].RichtextFacet_Link
		start, end := int(facet.Index.ByteStart), int(facet.Index.ByteEnd)
		// The offsets are byte positions into the text, which matters as soon
		// as the post holds multi-byte characters before the link.
		if got := body[start:end]; got != link.Uri {
			t.Fatalf("facet %d points at %q, want %q", i, got, link.Uri)
		}
	}
}

func TestLinkFacetsAreAbsentWithoutLinks(t *testing.T) {
	if facets := linkFacets("just words"); facets != nil {
		t.Fatalf("got %v, want no facets", facets)
	}
}

func TestRecordWithLinkFacetMarshalsType(t *testing.T) {
	body := "read https://example.com"
	record := &bsky.FeedPost{
		CreatedAt: "2024-05-01T10:00:00Z",
		Text:      body,
		Facets:    linkFacets(body),
	}
	input := &atproto.RepoCreateRecord_Input{
		Repo:       testDID,
		Collection: collectionPost,
		Record:     &util.LexiconTypeDecoder{Val: record},
	}

	encoded, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out := string(encoded)

	for _, want := range []string{
		`"$type":"app.bsky.richtext.facet#link"`,
		`"uri":"https://example.com"`,
		`"byteStart":5`,
		`"byteEnd":24`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("record is missing %s: %s", want, out)
		}
	}
}
