// Package store persists bridge state: per-platform read cursors, the map of
// posts the bridge itself created, and a short-lived content-hash dedup table.
//
// The bridged-post table is what stops the bridge from echoing its own output
// forever. Every post the bridge writes to a platform is recorded against the
// post it came from; when a listener later sees that new post on the target
// platform it can tell it is looking at its own work and skip it.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/andrinoff/xmbbridge/internal/model"

	_ "modernc.org/sqlite"
)

// Bridge statuses recorded in the bridged table.
const (
	StatusPending = "pending"
	StatusOK      = "ok"
	StatusError   = "error"
)

const schema = `
CREATE TABLE IF NOT EXISTS cursors (
	platform   TEXT PRIMARY KEY,
	value      TEXT NOT NULL,
	updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS bridged (
	id              INTEGER PRIMARY KEY AUTOINCREMENT,
	origin_platform TEXT NOT NULL,
	origin_id       TEXT NOT NULL,
	target_platform TEXT NOT NULL,
	target_id       TEXT,
	status          TEXT NOT NULL,
	error           TEXT,
	created_at      TEXT NOT NULL,
	updated_at      TEXT NOT NULL,
	UNIQUE (origin_platform, origin_id, target_platform)
);

CREATE INDEX IF NOT EXISTS idx_bridged_target ON bridged (target_platform, target_id);

CREATE TABLE IF NOT EXISTS seen (
	hash       TEXT PRIMARY KEY,
	created_at TEXT NOT NULL
) WITHOUT ROWID;
`

// Store is a handle to the bridge's SQLite database. It is safe for concurrent
// use; access is serialised through a single connection.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the database at path and applies the schema.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create database directory: %w", err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// A single connection keeps SQLite writes serialised and removes a whole
	// class of "database is locked" failures for a workload this small.
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA busy_timeout = 5000",
		"PRAGMA foreign_keys = ON",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("%s: %w", pragma, err)
		}
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// Cursor returns the last read cursor for a platform, or "" when the platform
// has never been read.
func (s *Store) Cursor(ctx context.Context, platform model.Platform) (string, error) {
	var value string
	err := s.db.QueryRowContext(ctx,
		`SELECT value FROM cursors WHERE platform = ?`, string(platform)).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read cursor for %s: %w", platform, err)
	}
	return value, nil
}

// SetCursor records the last read cursor for a platform.
func (s *Store) SetCursor(ctx context.Context, platform model.Platform, value string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO cursors (platform, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT (platform) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		string(platform), value, now())
	if err != nil {
		return fmt.Errorf("write cursor for %s: %w", platform, err)
	}
	return nil
}

// IsBridgedTarget reports whether the given post on the given platform was
// created by the bridge itself. Listeners call it on every post they read so
// that bridged copies are not bridged again.
func (s *Store) IsBridgedTarget(ctx context.Context, platform model.Platform, id string) (bool, error) {
	if id == "" {
		return false, nil
	}
	var one int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM bridged WHERE target_platform = ? AND target_id = ? LIMIT 1`,
		string(platform), id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check bridged target %s/%s: %w", platform, id, err)
	}
	return true, nil
}

// ClaimBridge claims the (origin, originID) -> target slot and reports whether
// it succeeded.
//
// The row is written before the target post is created so that a crash or a
// duplicate read cannot produce a second copy of the same post. A false return
// means a previous attempt is still pending or has already succeeded.
//
// Rows left in the error state by an earlier attempt are cleared first, which
// is what makes a transient failure retryable without ever duplicating a post
// that was created successfully.
func (s *Store) ClaimBridge(ctx context.Context, origin model.Platform, originID string, target model.Platform) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("claim bridge: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `
		DELETE FROM bridged
		WHERE origin_platform = ? AND origin_id = ? AND target_platform = ? AND status = ?`,
		string(origin), originID, string(target), StatusError); err != nil {
		return false, fmt.Errorf("clear failed bridge attempt: %w", err)
	}

	res, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO bridged
			(origin_platform, origin_id, target_platform, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		string(origin), originID, string(target), StatusPending, now(), now())
	if err != nil {
		return false, fmt.Errorf("claim bridge %s/%s -> %s: %w", origin, originID, target, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("claim bridge rows: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit bridge claim: %w", err)
	}
	return affected == 1, nil
}

// CompleteBridge records the identifier of the post the bridge created and
// marks the pair as successful.
func (s *Store) CompleteBridge(ctx context.Context, origin model.Platform, originID string, target model.Platform, targetID string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE bridged SET target_id = ?, status = ?, error = NULL, updated_at = ?
		WHERE origin_platform = ? AND origin_id = ? AND target_platform = ?`,
		targetID, StatusOK, now(), string(origin), originID, string(target))
	if err != nil {
		return fmt.Errorf("complete bridge %s/%s -> %s: %w", origin, originID, target, err)
	}
	return nil
}

// FailBridge marks a bridge attempt as failed and records why. The claim is
// cleared on the next detection, so a later poll can retry the pair.
func (s *Store) FailBridge(ctx context.Context, origin model.Platform, originID string, target model.Platform, cause error) error {
	msg := ""
	if cause != nil {
		msg = cause.Error()
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE bridged SET status = ?, error = ?, updated_at = ?
		WHERE origin_platform = ? AND origin_id = ? AND target_platform = ?`,
		StatusError, msg, now(), string(origin), originID, string(target))
	if err != nil {
		return fmt.Errorf("fail bridge %s/%s -> %s: %w", origin, originID, target, err)
	}
	return nil
}

// TargetID returns the identifier of the post that was created on target for
// the given origin post, if it has been bridged successfully. The engine uses
// it to thread a reply onto the bridged copy of its parent.
func (s *Store) TargetID(ctx context.Context, origin model.Platform, originID string, target model.Platform) (string, bool, error) {
	var targetID string
	err := s.db.QueryRowContext(ctx, `
		SELECT target_id FROM bridged
		WHERE origin_platform = ? AND origin_id = ? AND target_platform = ? AND status = ?`,
		string(origin), originID, string(target), StatusOK).Scan(&targetID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("lookup bridged target %s/%s -> %s: %w", origin, originID, target, err)
	}
	if targetID == "" {
		return "", false, nil
	}
	return targetID, true, nil
}

// SeenRecently reports whether an identical post (by normalised content hash)
// was bridged within window. It stops the same text being copied twice when it
// is published by hand on two platforms at the same time.
func (s *Store) SeenRecently(ctx context.Context, hash string, window time.Duration) (bool, error) {
	if hash == "" {
		return false, nil
	}
	var created string
	err := s.db.QueryRowContext(ctx, `SELECT created_at FROM seen WHERE hash = ?`, hash).Scan(&created)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check seen hash: %w", err)
	}
	at, err := time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return false, nil
	}
	return time.Since(at) < window, nil
}

// MarkSeen records a content hash as bridged.
func (s *Store) MarkSeen(ctx context.Context, hash string) error {
	if hash == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO seen (hash, created_at) VALUES (?, ?)`, hash, now())
	if err != nil {
		return fmt.Errorf("mark seen hash: %w", err)
	}
	return nil
}

// PruneSeen drops dedup entries older than the given age.
func (s *Store) PruneSeen(ctx context.Context, olderThan time.Duration) error {
	cutoff := time.Now().Add(-olderThan).Format(time.RFC3339Nano)
	if _, err := s.db.ExecContext(ctx, `DELETE FROM seen WHERE created_at < ?`, cutoff); err != nil {
		return fmt.Errorf("prune seen: %w", err)
	}
	return nil
}

// Stats is a small snapshot used for the startup log line.
type Stats struct {
	Bridged int
	Pending int
	Failed  int
	Seen    int
}

// Stats counts the rows currently held in the database.
func (s *Store) Stats(ctx context.Context) (Stats, error) {
	var out Stats
	row := s.db.QueryRowContext(ctx, `
		SELECT
			(SELECT COUNT(*) FROM bridged),
			(SELECT COUNT(*) FROM bridged WHERE status = ?),
			(SELECT COUNT(*) FROM bridged WHERE status = ?),
			(SELECT COUNT(*) FROM seen)`,
		StatusPending, StatusError)
	if err := row.Scan(&out.Bridged, &out.Pending, &out.Failed, &out.Seen); err != nil {
		return Stats{}, fmt.Errorf("read stats: %w", err)
	}
	return out, nil
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }
