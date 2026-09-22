// Package platform holds the small pieces of machinery the platform adapters
// share: a polling loop with backoff and a bounded retry helper.
package platform

import (
	"context"
	"log/slog"
	"math/rand"
	"time"
)

// Backoff computes exponentially growing retry delays with jitter.
type Backoff struct {
	Base time.Duration
	Max  time.Duration
}

// Delay returns how long to wait before the given attempt (1-based).
func (b Backoff) Delay(attempt int) time.Duration {
	base := b.Base
	if base <= 0 {
		base = time.Second
	}
	maxDelay := b.Max
	if maxDelay <= 0 {
		maxDelay = time.Minute
	}
	if attempt < 1 {
		attempt = 1
	}
	delay := base
	for i := 1; i < attempt; i++ {
		delay *= 2
		if delay >= maxDelay {
			delay = maxDelay
			break
		}
	}
	if delay > maxDelay {
		delay = maxDelay
	}
	// Jitter keeps several platforms from retrying in lockstep.
	jitter := time.Duration(rand.Int63n(int64(delay)/4 + 1))
	return delay + jitter
}

// Sleep waits for d, returning early with ctx.Err() when the context is done.
func Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// PollFunc performs one round of polling and returns how long to wait before
// the next round. A non-zero override replaces the configured interval, which
// lets a poller park itself until a rate limit resets.
type PollFunc func(ctx context.Context) (time.Duration, error)

// PollLoop calls fn immediately and then on a schedule until the context is
// cancelled. Errors are logged and retried with exponential backoff.
func PollLoop(ctx context.Context, name string, interval time.Duration, log *slog.Logger, fn PollFunc) error {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	backoff := Backoff{Base: 5 * time.Second, Max: 5 * time.Minute}
	failures := 0
	next := time.Duration(0)

	for {
		if err := Sleep(ctx, next); err != nil {
			return nil
		}
		override, err := fn(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			failures++
			wait := backoff.Delay(failures)
			if override > wait {
				wait = override
			}
			log.Warn("poll failed, backing off", "platform", name, "attempt", failures, "wait", wait.String(), "error", err)
			next = wait
			continue
		}
		failures = 0
		next = interval
		if override > 0 {
			next = override
		}
	}
}

// Retry runs fn until it succeeds, the context is cancelled, or attempts are
// exhausted. It is used for individual outbound posts so a transient network
// or rate-limit failure does not lose the post.
func Retry(ctx context.Context, attempts int, backoff Backoff, log *slog.Logger, what string, fn func(ctx context.Context) error) error {
	if attempts < 1 {
		attempts = 1
	}
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return lastErr
			}
			return err
		}
		lastErr = fn(ctx)
		if lastErr == nil {
			return nil
		}
		if attempt == attempts {
			break
		}
		wait := backoff.Delay(attempt)
		log.Warn("retrying", "what", what, "attempt", attempt, "wait", wait.String(), "error", lastErr)
		if err := Sleep(ctx, wait); err != nil {
			return lastErr
		}
	}
	return lastErr
}
