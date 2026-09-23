package text

import (
	"strings"
	"testing"
)

func TestStripHTML(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"plain", "hello world", "hello world"},
		{"paragraphs", "<p>one</p><p>two</p>", "one\ntwo"},
		{"line break", "one<br />two", "one\ntwo"},
		{"link text kept", `<p>see <a href="https://example.com">example</a></p>`, "see example"},
		{"entities", "<p>a &amp; b &lt;c&gt;</p>", "a & b <c>"},
		{"nested markup", "<p>bold <strong>text</strong> here</p>", "bold text here"},
		{"blank lines collapsed", "<p>a</p><p></p><p></p><p>b</p>", "a\n\nb"},
		{"non breaking space", "<p>a&nbsp;b</p>", "a b"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := StripHTML(tc.in); got != tc.want {
				t.Fatalf("StripHTML(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"fits", "short", 10, "short"},
		{"exact", "abcde", 5, "abcde"},
		{"cuts", "abcdefghij", 5, "abcd\u2026"},
		{"unicode", "héllo wörld", 6, "héllo\u2026"},
		{"trailing space trimmed", "abc def", 4, "abc\u2026"},
		{"zero max is a no-op", "anything", 0, "anything"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Truncate(tc.in, tc.max, "\u2026"); got != tc.want {
				t.Fatalf("Truncate(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
			}
		})
	}
}

func TestTruncateNeverSplitsURL(t *testing.T) {
	const link = "https://example.com/a/very/long/path"
	in := "read this " + link + " please"

	got := Truncate(in, 30, "\u2026")

	if len([]rune(got)) > 30 {
		t.Fatalf("result %q is longer than the limit", got)
	}
	if got != "read this\u2026" {
		t.Fatalf("expected the URL to be dropped whole, got %q", got)
	}
}

func TestTruncateKeepsWholeURLWhenItFits(t *testing.T) {
	const link = "https://example.com/short"
	in := link + " and then some extra words that overflow"

	got := Truncate(in, 30, "\u2026")

	if len([]rune(got)) > 30 {
		t.Fatalf("result %q is longer than the limit", got)
	}
	// The URL must survive whole; only the words after it may be cut.
	if got != "https://example.com/short and\u2026" {
		t.Fatalf("expected the whole URL to survive, got %q", got)
	}
}

func TestFindLinks(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"none", "no links in here", nil},
		{"plain", "read https://example.com/a/b now", []string{"https://example.com/a/b"}},
		{
			"several",
			"first https://example.com and second http://two.example.org/x",
			[]string{"https://example.com", "http://two.example.org/x"},
		},
		{"sentence period", "see https://example.com/thing.", []string{"https://example.com/thing"}},
		{"comma", "see https://example.com/thing, then read it", []string{"https://example.com/thing"}},
		{"query string kept", "https://example.com/search?q=go&lang=en", []string{"https://example.com/search?q=go&lang=en"}},
		{"balanced brackets kept", "see (https://en.wikipedia.org/wiki/Go_(game))", []string{"https://en.wikipedia.org/wiki/Go_(game)"}},
		{"unbalanced bracket dropped", "see (https://example.com/thing)", []string{"https://example.com/thing"}},
		{"ellipsis dropped", "from https://example.com/very/long\u2026", []string{"https://example.com/very/long"}},
		{"without a scheme", "example.com/thing www.example.com", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := FindLinks(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("FindLinks(%q) = %d links %v, want %v", tc.in, len(got), got, tc.want)
			}
			for i, want := range tc.want {
				if got[i].URL != want {
					t.Fatalf("link %d = %q, want %q", i, got[i].URL, want)
				}
				if tc.in[got[i].Start:got[i].End] != want {
					t.Fatalf("offsets %d:%d select %q, want %q",
						got[i].Start, got[i].End, tc.in[got[i].Start:got[i].End], want)
				}
			}
		})
	}
}

func TestFindLinksReportsByteOffsets(t *testing.T) {
	// Multi-byte characters before the link make byte offsets differ from
	// rune offsets; Bluesky facets count bytes.
	in := "h\u00e9llo \U0001F47E see https://example.com"
	links := FindLinks(in)
	if len(links) != 1 {
		t.Fatalf("got %d links, want 1", len(links))
	}
	want := strings.Index(in, "https://example.com")
	if links[0].Start != want {
		t.Fatalf("start = %d, want the byte offset %d", links[0].Start, want)
	}
	if got := in[links[0].Start:links[0].End]; got != "https://example.com" {
		t.Fatalf("offsets select %q", got)
	}
}

func TestNormalize(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"case and spacing", "Hello   World", "hello world"},
		{"newlines", "one\ntwo", "one two"},
		{"html", "<p>Hello</p><p>World</p>", "hello world"},
		{"empty", "   ", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Normalize(tc.in); got != tc.want {
				t.Fatalf("Normalize(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestHashIsStableAndContentSensitive(t *testing.T) {
	if Hash("") != "" {
		t.Fatal("empty text should hash to the empty string")
	}
	if Hash("   ") != "" {
		t.Fatal("whitespace-only text should hash to the empty string")
	}
	// Cosmetic differences must not change the hash: these two describe the
	// same content published through two different platforms.
	a := Hash("<p>Hello   world</p>")
	b := Hash("hello world")
	if a != b {
		t.Fatalf("expected equal hashes, got %q and %q", a, b)
	}
	if Hash("hello world!") == a {
		t.Fatal("different content should hash differently")
	}
}
