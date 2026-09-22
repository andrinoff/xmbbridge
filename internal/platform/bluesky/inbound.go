package bluesky

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/andrinoff/xmbbridge/internal/model"
	"github.com/andrinoff/xmbbridge/internal/platform"

	"github.com/gorilla/websocket"
)

// jetstreamEvent is one message off the Jetstream relay.
//
// Two cursor spellings exist in the wild: older relays report microsecond
// timestamps in time_us, newer ones report a monotonic sequence number in
// cursor. Both are round-tripped verbatim so the relay decides how to resume.
type jetstreamEvent struct {
	DID    string           `json:"did"`
	TimeUS int64            `json:"time_us"`
	Cursor uint64           `json:"cursor"`
	Kind   string           `json:"kind"`
	Commit *jetstreamCommit `json:"commit"`
}

type jetstreamCommit struct {
	Rev        string          `json:"rev"`
	Operation  string          `json:"operation"`
	Collection string          `json:"collection"`
	Rkey       string          `json:"rkey"`
	Record     json.RawMessage `json:"record"`
	CID        string          `json:"cid"`
}

// Run tails the Jetstream relay for this account's posts and emits each new
// one. It blocks until the context is cancelled, reconnecting as needed.
func (a *Adapter) Run(ctx context.Context, sink chan<- model.Post) error {
	go a.refreshLoop(ctx)

	did := a.DID()
	if did == "" {
		return fmt.Errorf("bluesky: adapter has no DID")
	}

	backoff := platform.Backoff{Base: 2 * time.Second, Max: time.Minute}
	failures := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		started := time.Now()
		err := a.tailOnce(ctx, sink, did)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			// A connection that stayed up for a while is not a failure trend.
			if time.Since(started) > 45*time.Second {
				failures = 0
			}
			failures++
			wait := backoff.Delay(failures)
			a.log.Warn("bluesky: jetstream tail ended, reconnecting",
				"attempt", failures, "wait", wait.String(), "error", err)
			if err := platform.Sleep(ctx, wait); err != nil {
				return nil
			}
			continue
		}
		failures = 0
	}
}

// tailOnce holds a single Jetstream connection open until it fails or the
// context is cancelled.
func (a *Adapter) tailOnce(ctx context.Context, sink chan<- model.Post, did string) error {
	cursor, err := a.state.Cursor(ctx, model.PlatformBluesky)
	if err != nil {
		return err
	}

	endpoint, err := withQuery(a.cfg.Jetstream, map[string]string{
		"wantedDids":        did,
		"wantedCollections": collectionPost,
		"cursor":            cursor,
	})
	if err != nil {
		return err
	}

	dialer := *websocket.DefaultDialer
	dialer.HandshakeTimeout = 20 * time.Second
	conn, resp, err := dialer.DialContext(ctx, endpoint, nil)
	if err != nil {
		if resp != nil {
			return fmt.Errorf("dial jetstream: %w (http %s)", err, resp.Status)
		}
		return fmt.Errorf("dial jetstream: %w", err)
	}
	defer conn.Close()

	a.log.Info("bluesky listener started",
		"did", did,
		"relay", a.cfg.Jetstream,
		"resumed_from_cursor", cursor != "",
		"replies", a.cfg.IncludeReplies,
	)

	// Closing the connection is what unblocks the read loop on shutdown.
	closed := make(chan struct{})
	defer close(closed)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-closed:
		}
	}()

	// The cursor is flushed periodically and once more when the connection
	// ends, on a context that survives shutdown. Losing it is safe, the relay
	// replays the events and the engine dedupes, but replay is work.
	saved := cursor
	var lastCursor string
	flush := func(value string) {
		if value == "" || value == saved {
			return
		}
		if err := a.saveCursor(value); err != nil {
			a.log.Warn("bluesky: could not persist the stream cursor", "error", err)
			return
		}
		saved = value
	}
	defer func() { flush(lastCursor) }()

	sinceFlush := 0

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read jetstream message: %w", err)
		}

		event, err := decodeJetstreamEvent(data)
		if err != nil {
			a.log.Debug("bluesky: skipping undecodable jetstream frame", "error", err)
			continue
		}
		// Account and identity markers carry no commit.
		if event.Commit == nil || event.DID != did {
			continue
		}
		if event.Commit.Collection != collectionPost || event.Commit.Operation != "create" {
			continue
		}
		if len(event.Commit.Record) == 0 {
			continue
		}

		post, err := a.toPost(event.DID, event.Commit.Rkey, event.Commit.Record)
		if err != nil {
			a.log.Warn("bluesky: could not read post record", "rkey", event.Commit.Rkey, "error", err)
			continue
		}
		if post.IsReply && !a.cfg.IncludeReplies {
			continue
		}

		if err := platform.Emit(ctx, sink, post); err != nil {
			return err
		}
		if next := event.cursor(); next != "" {
			lastCursor = next
			sinceFlush++
			// Replayed events are deduped downstream, so a bounded flush
			// interval is a trade of one write for at most this many replays.
			if sinceFlush >= cursorFlushInterval {
				flush(lastCursor)
				sinceFlush = 0
			}
		}
	}
}

// saveCursor persists progress on a context that survives shutdown.
func (a *Adapter) saveCursor(value string) error {
	ctx, cancel := platform.ProgressContext()
	defer cancel()
	return a.state.SetCursor(ctx, model.PlatformBluesky, value)
}

func decodeJetstreamEvent(data []byte) (jetstreamEvent, error) {
	var event jetstreamEvent
	if err := json.Unmarshal(data, &event); err != nil {
		return jetstreamEvent{}, err
	}
	return event, nil
}

// cursor returns the resume point to send on reconnect. The sequence number is
// preferred when the relay provides one because it is gap-free, otherwise the
// microsecond timestamp is used.
func (e jetstreamEvent) cursor() string {
	if e.Cursor > 0 {
		return strconv.FormatUint(e.Cursor, 10)
	}
	if e.TimeUS > 0 {
		return strconv.FormatInt(e.TimeUS, 10)
	}
	return ""
}
