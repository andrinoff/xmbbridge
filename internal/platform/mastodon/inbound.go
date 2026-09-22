package mastodon

import (
	"context"
	"fmt"
	"time"

	"github.com/andrinoff/xmbbridge/internal/model"
	"github.com/andrinoff/xmbbridge/internal/platform"

	"github.com/mattn/go-mastodon"
)

// pageSize is how many statuses are asked for per poll. Mastodon caps this at
// 40 by default, so asking for more would only be ignored.
const pageSize = 40

// Run polls the account's own statuses and emits each new one. It blocks until
// the context is cancelled.
func (a *Adapter) Run(ctx context.Context, sink chan<- model.Post) error {
	accountID := a.accountID
	var acct string
	if accountID == "" {
		account, err := a.client.GetAccountCurrentUser(ctx)
		if err != nil {
			return fmt.Errorf("mastodon: verify credentials: %w", err)
		}
		accountID = account.ID
		a.accountID = account.ID
		acct = account.Acct
	}
	a.log.Info("mastodon listener started",
		"account", acct,
		"instance", a.cfg.Server,
		"poll_interval", a.pollInterval.String(),
		"replies", a.cfg.IncludeReplies,
	)

	return platform.PollLoop(ctx, "mastodon", a.pollInterval, a.log, func(ctx context.Context) (time.Duration, error) {
		return 0, a.poll(ctx, sink, accountID)
	})
}

// saveCursor persists progress on a context that survives shutdown, so a
// cancelled bridge does not lose track of what has already been read.
func (a *Adapter) saveCursor(value string) error {
	ctx, cancel := platform.ProgressContext()
	defer cancel()
	return a.state.SetCursor(ctx, model.PlatformMastodon, value)
}

func (a *Adapter) poll(ctx context.Context, sink chan<- model.Post, accountID mastodon.ID) error {
	cursor, err := a.state.Cursor(ctx, model.PlatformMastodon)
	if err != nil {
		return err
	}

	pagination := &mastodon.Pagination{Limit: pageSize}
	if cursor != "" {
		pagination.SinceID = mastodon.ID(cursor)
	}

	statuses, err := a.client.GetAccountStatuses(ctx, accountID, pagination)
	if err != nil {
		return fmt.Errorf("mastodon: fetch statuses: %w", err)
	}
	if len(statuses) == 0 {
		return nil
	}

	newest := cursor
	for _, status := range statuses {
		if platform.IDNewer(string(status.ID), newest) {
			newest = string(status.ID)
		}
	}

	// A first poll with no stored cursor would otherwise replay the account's
	// recent history, which is not what "mirror what I post from now on" means.
	if cursor == "" && !a.backfill {
		a.log.Info("mastodon: starting from the newest post; existing history is not bridged", "newest", newest)
		return a.saveCursor(newest)
	}

	// The API returns newest first; emit oldest first so a thread is bridged
	// in reading order.
	for i := len(statuses) - 1; i >= 0; i-- {
		status := statuses[i]

		// A boost is somebody else's post, not this account's.
		if status.Reblog != nil {
			continue
		}
		// Guard against pinned posts and cursor overlap: only genuinely newer
		// statuses are new.
		if cursor != "" && !platform.IDNewer(string(status.ID), cursor) {
			continue
		}

		post := a.toPost(status)
		if post.IsReply && !a.cfg.IncludeReplies {
			continue
		}
		if err := platform.Emit(ctx, sink, post); err != nil {
			return err
		}
	}

	if newest != cursor {
		if err := a.saveCursor(newest); err != nil {
			return err
		}
	}
	return nil
}
