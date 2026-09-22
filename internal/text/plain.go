// Package text normalises post bodies as they move between platforms.
//
// Mastodon hands back HTML where Bluesky and X hand back plain text, and the
// platforms disagree about how many characters a post may contain, so a post
// almost always needs some combination of unstyling and truncation.
package text

import (
	"crypto/sha256"
	"encoding/hex"
	"html"
	"regexp"
	"strings"
	"unicode"
)

var (
	tagPattern     = regexp.MustCompile(`(?s)<[^>]*>`)
	breakPattern   = regexp.MustCompile(`(?i)<\s*(br|/p|/div|/li)\s*/?\s*>`)
	blankLines     = regexp.MustCompile(`\n{3,}`)
	urlPattern     = regexp.MustCompile(`https?://[^\s]+`)
	spaceRun       = regexp.MustCompile(`[ \t]{2,}`)
	trailingSpace  = regexp.MustCompile(`[ \t]+\n`)
	leadingSpace   = regexp.MustCompile(`\n[ \t]+`)
	htmlEntitiesOK = strings.NewReplacer("\u00a0", " ")
)

// StripHTML converts the HTML body Mastodon returns into readable plain text.
// Paragraph and line-break tags become newlines; every other tag is dropped so
// that link text survives even though the link markup does not.
func StripHTML(s string) string {
	if s == "" {
		return ""
	}
	out := breakPattern.ReplaceAllString(s, "\n")
	out = tagPattern.ReplaceAllString(out, "")
	out = html.UnescapeString(out)
	out = htmlEntitiesOK.Replace(out)
	out = trailingSpace.ReplaceAllString(out, "\n")
	out = leadingSpace.ReplaceAllString(out, "\n")
	out = blankLines.ReplaceAllString(out, "\n\n")
	return strings.TrimSpace(out)
}

// Truncate shortens s to at most max runes, appending the ellipsis when
// something was removed. Cuts never land inside a URL: a URL that would be
// split is moved wholly to the shortened side or dropped.
func Truncate(s string, max int, ellipsis string) string {
	if max <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	cut := max - len([]rune(ellipsis))
	if cut < 0 {
		cut = 0
	}
	cut = avoidSplittingURL(s, runes, cut)
	// Do not leave a dangling space in front of the ellipsis.
	for cut > 0 && unicode.IsSpace(runes[cut-1]) {
		cut--
	}
	return strings.TrimSpace(string(runes[:cut])) + ellipsis
}

// avoidSplittingURL returns a possibly reduced rune index such that the cut
// does not fall inside a URL. Within a URL the cut moves back to its start.
func avoidSplittingURL(s string, runes []rune, cut int) int {
	if cut <= 0 || cut >= len(runes) {
		return cut
	}
	// FindAllIndex reports byte offsets; convert the rune cut to a byte offset
	// to compare, then convert any URL start back to a rune index.
	cutByte := len(string(runes[:cut]))
	for _, loc := range urlPattern.FindAllIndex([]byte(s), -1) {
		if cutByte > loc[0] && cutByte < loc[1] {
			return len([]rune(s[:loc[0]]))
		}
	}
	return cut
}

// Normalize collapses the cosmetic differences that should not stop two posts
// being recognised as the same content.
func Normalize(s string) string {
	s = StripHTML(s)
	s = strings.ToLower(s)
	s = spaceRun.ReplaceAllString(s, " ")
	fields := strings.FieldsFunc(s, func(r rune) bool { return unicode.IsSpace(r) })
	return strings.Join(fields, " ")
}

// Hash returns a stable fingerprint of a post body, used to suppress repeated
// content within the dedup window. It returns "" for empty input so callers
// can skip empty posts entirely.
func Hash(s string) string {
	normalized := Normalize(s)
	if normalized == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(sum[:])
}
