package platform

import (
	"context"
	"strings"
	"time"

	"github.com/andrinoff/xmbbridge/internal/model"
)

// State is the persistent state a listener needs: the read cursor for its own
// stream, so a restart resumes where it left off instead of replaying history.
type State interface {
	Cursor(ctx context.Context, platform model.Platform) (string, error)
	SetCursor(ctx context.Context, platform model.Platform, value string) error
}

// ProgressContext returns a context for recording progress that is already
// done. It is deliberately detached from the caller's context, because
// cancelling the bridge must not discard the record of what has been processed:
// losing a cursor write means redoing work after a restart.
func ProgressContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}

// Emit hands a post to the engine, giving up if the context is cancelled while
// the engine is busy.
func Emit(ctx context.Context, sink chan<- model.Post, post model.Post) error {
	select {
	case sink <- post:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// IDNewer reports whether identifier a is newer than b, for the platforms whose
// identifiers are monotonically increasing decimal numbers (Mastodon and X).
//
// It compares digit strings rather than parsing integers so identifiers larger
// than a machine word cannot overflow. An empty b is always older, which makes
// the first poll treat everything as new.
func IDNewer(a, b string) bool {
	a = canonicalID(a)
	b = canonicalID(b)
	if b == "" {
		return a != ""
	}
	if a == "" {
		return false
	}
	if len(a) != len(b) {
		return len(a) > len(b)
	}
	return a > b
}

// canonicalID strips leading zeros so that "007" and "7" compare equal.
func canonicalID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	trimmed := strings.TrimLeft(id, "0")
	if trimmed == "" {
		return "0"
	}
	return trimmed
}
