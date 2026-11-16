package projection

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (work plan D1)
)

// SchemaVersion is the projection schema this build writes (work plan D3).
const SchemaVersion = 1

// schema is portable SQL: TEXT and BIGINT columns, times as Unix
// nanoseconds in UTC, and ON CONFLICT upserts, so the same statements run on
// Postgres from v0.3 (TECHNICAL-SPEC §10).
var schema = []string{
	`CREATE TABLE IF NOT EXISTS schema_version (version BIGINT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS items (
		project        TEXT   NOT NULL,
		item           TEXT   NOT NULL,
		workflow_id    TEXT   NOT NULL,
		claim          BIGINT NOT NULL,
		seq            BIGINT NOT NULL,
		state          TEXT   NOT NULL,
		round          BIGINT NOT NULL,
		rounds_allowed BIGINT NOT NULL,
		blocking       BIGINT NOT NULL,
		host           TEXT   NOT NULL,
		entered_at     BIGINT NOT NULL,
		next_action    TEXT   NOT NULL,
		wait_reason    TEXT   NOT NULL,
		updated_at     BIGINT NOT NULL,
		PRIMARY KEY (project, item)
	)`,
	`CREATE TABLE IF NOT EXISTS decisions (
		workflow_id    TEXT   NOT NULL,
		claim          BIGINT NOT NULL,
		seq            BIGINT NOT NULL,
		project        TEXT   NOT NULL,
		item           TEXT   NOT NULL,
		at             BIGINT NOT NULL,
		event          TEXT   NOT NULL,
		from_state     TEXT   NOT NULL,
		to_state       TEXT   NOT NULL,
		rule           TEXT   NOT NULL,
		tracker_state  TEXT   NOT NULL,
		result_stage   TEXT   NOT NULL,
		result_run_id  TEXT   NOT NULL,
		result_outcome TEXT   NOT NULL,
		actions        TEXT   NOT NULL,
		PRIMARY KEY (workflow_id, claim, seq)
	)`,
	`CREATE INDEX IF NOT EXISTS decisions_by_item ON decisions (project, item, at)`,
	`CREATE TABLE IF NOT EXISTS stage_runs (
		run_id     TEXT   NOT NULL PRIMARY KEY,
		project    TEXT   NOT NULL,
		item       TEXT   NOT NULL,
		stage      TEXT   NOT NULL,
		kind       TEXT   NOT NULL,
		attempt    BIGINT NOT NULL,
		host       TEXT   NOT NULL,
		started_at BIGINT NOT NULL,
		ended_at   BIGINT NOT NULL,
		result     TEXT   NOT NULL,
		log_path   TEXT   NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS stage_runs_by_item ON stage_runs (project, item, started_at)`,
	`CREATE INDEX IF NOT EXISTS stage_runs_by_day ON stage_runs (project, started_at)`,
	`CREATE TABLE IF NOT EXISTS hosts (
		name         TEXT   NOT NULL PRIMARY KEY,
		capabilities TEXT   NOT NULL,
		last_seen    BIGINT NOT NULL,
		last_check   TEXT   NOT NULL
	)`,
}

// DB is the projection database: the read model and the decision log
// (TECHNICAL-SPEC §10). It is safe for concurrent use.
type DB struct {
	db *sql.DB
}

// Open opens or creates the projection database at path, creating its
// directory with mode 0700 and the file with mode 0600, and applies the
// schema. A database written by a newer schema is refused.
func Open(ctx context.Context, path string) (*DB, error) {
	if path == "" {
		return nil, errors.New("projection: no database path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("projection: %w", err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("projection: %w", err)
	}
	f.Close()

	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Set("_txlock", "immediate")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, fmt.Errorf("projection: %w", err)
	}
	p := &DB{db: db}
	if err := p.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return p, nil
}

// Close closes the database.
func (p *DB) Close() error { return p.db.Close() }

func (p *DB) migrate(ctx context.Context) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("projection: schema: %w", err)
	}
	defer tx.Rollback()
	for _, s := range schema {
		if _, err := tx.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("projection: schema: %w", err)
		}
	}
	var v int
	switch err := tx.QueryRowContext(ctx, `SELECT version FROM schema_version`).Scan(&v); {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_version (version) VALUES (?)`, SchemaVersion); err != nil {
			return fmt.Errorf("projection: schema: %w", err)
		}
	case err != nil:
		return fmt.Errorf("projection: schema: %w", err)
	case v > SchemaVersion:
		return fmt.Errorf("projection: database schema version %d is newer than this build's %d", v, SchemaVersion)
	}
	return tx.Commit()
}

// nanos stores a time as Unix nanoseconds; the zero time is 0.
func nanos(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

// fromNanos reads a stored time in UTC; 0 is the zero time.
func fromNanos(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n).UTC()
}
