package platform

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/andrinoff/xmbbridge/internal/model"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestIDNewer(t *testing.T) {
	tests := []struct {
		name string
		a    string
		b    string
		want bool
	}{
		{"higher is newer", "112", "111", true},
		{"lower is not newer", "110", "111", false},
		{"equal is not newer", "111", "111", false},
		{"longer number wins", "1000000000000000000", "999999", true},
		{"empty baseline accepts anything", "5", "", true},
		{"nothing is newer than something", "", "5", false},
		{"both empty", "", "", false},
		{"leading zeros normalised", "0012", "11", true},
		{"leading zeros equal", "012", "12", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IDNewer(tc.a, tc.b); got != tc.want {
				t.Fatalf("IDNewer(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestIDNewerHandlesHugeIdentifiers(t *testing.T) {
	// Mastodon identifiers can exceed a machine word; string comparison must
	// not overflow or fall back to lexicographic ordering of different lengths.
	small := "9000000000000000000000000000000000001"
	large := "9000000000000000000000000000000000002"
	if !IDNewer(large, small) {
		t.Fatal("the larger identifier should compare as newer")
	}
	if IDNewer(small, large) {
		t.Fatal("the smaller identifier should not compare as newer")
	}
}

func TestBackoffGrowsAndIsCapped(t *testing.T) {
	b := Backoff{Base: time.Second, Max: 10 * time.Second}

	first := b.Delay(1)
	if first < time.Second {
		t.Fatalf("first delay = %v, must be at least the base", first)
	}
	// Jitter is bounded by a quarter of the delay.
	if first > time.Second+time.Second/4+1 {
		t.Fatalf("first delay jitter is too large: %v", first)
	}

	for attempt := 2; attempt <= 10; attempt++ {
		delay := b.Delay(attempt)
		if delay > 10*time.Second+10*time.Second/4+1 {
			t.Fatalf("attempt %d delay = %v, exceeds the cap plus jitter", attempt, delay)
		}
		if delay < 0 {
			t.Fatalf("attempt %d delay is negative: %v", attempt, delay)
		}
	}
}

func TestBackoffDefaultsAreSane(t *testing.T) {
	// A zero-value Backoff must still produce usable delays.
	var b Backoff
	delay := b.Delay(1)
	if delay <= 0 {
		t.Fatalf("zero-value backoff produced %v", delay)
	}
	if delay > 2*time.Second {
		t.Fatalf("zero-value backoff produced an unexpectedly long %v", delay)
	}
}

func TestSleepHonoursContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Sleep(ctx, time.Hour); err == nil {
		t.Fatal("a cancelled context should abort the sleep")
	}

	// A zero delay does not block and reports the state of the context.
	if err := Sleep(context.Background(), 0); err != nil {
		t.Fatalf("a zero delay with a live context should return nil, got %v", err)
	}
	if err := Sleep(ctx, 0); err == nil {
		t.Fatal("a zero delay with a cancelled context should return the context error")
	}
}

func TestEmitHonoursContext(t *testing.T) {
	sink := make(chan model.Post, 1)
	post := model.Post{Origin: model.PlatformBluesky, OriginID: "1"}

	if err := Emit(context.Background(), sink, post); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if got := <-sink; got.OriginID != "1" {
		t.Fatalf("emitted post = %q", got.OriginID)
	}

	// Fill the buffer, then cancel: Emit must give up rather than block.
	if err := Emit(context.Background(), sink, post); err != nil {
		t.Fatalf("Emit into a full channel should succeed into the buffer: %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Emit(cancelled, sink, post); err == nil {
		t.Fatal("Emit should give up once the context is done")
	}
}

func TestRetrySucceedsAfterFailures(t *testing.T) {
	attempts := 0
	err := Retry(context.Background(), 3, Backoff{Base: time.Millisecond, Max: 2 * time.Millisecond}, discardLogger(), "test", func(context.Context) error {
		attempts++
		if attempts < 3 {
			return context.DeadlineExceeded
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
}

func TestRetryGivesUp(t *testing.T) {
	attempts := 0
	err := Retry(context.Background(), 2, Backoff{Base: time.Millisecond, Max: 2 * time.Millisecond}, discardLogger(), "test", func(context.Context) error {
		attempts++
		return context.DeadlineExceeded
	})
	if err == nil {
		t.Fatal("Retry should return the final error")
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}
