package store

import (
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"

	_ "modernc.org/sqlite" // pure-Go SQLite driver
)

// segmentColumns is the column order used by scanSegment.
const segmentColumns = "ch, start_ts, end_ts, size, event_type, status, synced_at, path, bytes, err"

// upsertSegmentSQL inserts a segment or merges non-zero incoming fields into
// the row with the same (ch, start_ts, end_ts) key. The error column follows
// the original semantics: a non-empty Status overwrites Err even when the
// incoming Err is empty, so a successful retry clears a previous failure.
const upsertSegmentSQL = `
INSERT INTO segments (ch, start_ts, end_ts, size, event_type, status, synced_at, path, bytes, err)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(ch, start_ts, end_ts) DO UPDATE SET
	size = CASE WHEN excluded.size <> 0 THEN excluded.size ELSE segments.size END,
	event_type = CASE WHEN excluded.event_type <> 0 THEN excluded.event_type ELSE segments.event_type END,
	status = CASE WHEN excluded.status <> '' THEN excluded.status ELSE segments.status END,
	synced_at = CASE WHEN excluded.synced_at <> 0 THEN excluded.synced_at ELSE segments.synced_at END,
	path = CASE WHEN excluded.path <> '' THEN excluded.path ELSE segments.path END,
	bytes = CASE WHEN excluded.bytes <> 0 THEN excluded.bytes ELSE segments.bytes END,
	err = CASE
		WHEN excluded.status <> '' THEN excluded.err
		WHEN excluded.err <> '' THEN excluded.err
		ELSE segments.err
	END
`

// schemaTables must run before schemaIndexes: a database created by an older
// build can have a logs table without the ch column, and creating the ch index
// before the migration would fail.
var schemaTables = []string{
	`CREATE TABLE IF NOT EXISTS segments (
		ch         INTEGER NOT NULL,
		start_ts   INTEGER NOT NULL,
		end_ts     INTEGER NOT NULL,
		size       INTEGER NOT NULL DEFAULT 0,
		event_type INTEGER NOT NULL DEFAULT 0,
		status     TEXT    NOT NULL DEFAULT '',
		synced_at  INTEGER NOT NULL DEFAULT 0,
		path       TEXT    NOT NULL DEFAULT '',
		bytes      INTEGER NOT NULL DEFAULT 0,
		err        TEXT    NOT NULL DEFAULT '',
		PRIMARY KEY (ch, start_ts, end_ts)
	)`,
	`CREATE TABLE IF NOT EXISTS logs (
		id    INTEGER PRIMARY KEY AUTOINCREMENT,
		ts    INTEGER NOT NULL,
		level TEXT    NOT NULL,
		kind  TEXT    NOT NULL,
		ch    INTEGER NOT NULL DEFAULT 0,
		msg   TEXT    NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS sync_state (
		id   INTEGER PRIMARY KEY CHECK (id = 1),
		data TEXT    NOT NULL
	)`,
}

var schemaIndexes = []string{
	`CREATE INDEX IF NOT EXISTS idx_segments_status ON segments(status)`,
	`CREATE INDEX IF NOT EXISTS idx_segments_start ON segments(start_ts, ch, end_ts)`,
	`CREATE INDEX IF NOT EXISTS idx_segments_end ON segments(end_ts)`,
	`CREATE INDEX IF NOT EXISTS idx_logs_level ON logs(level, id DESC)`,
	`CREATE INDEX IF NOT EXISTS idx_logs_ch ON logs(ch, id DESC)`,
}

// openDB opens (or creates) the SQLite database and makes sure the schema is
// present. The DSN enables WAL, a 5s busy timeout and foreign keys, as
// required by the deployment.
func openDB(path string) (*sql.DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("store: resolve database path %q: %w", path, err)
	}

	dsn := url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}
	dsn.RawQuery = "_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(1)"

	db, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return nil, fmt.Errorf("store: open database %s: %w", path, err)
	}

	// SQLite allows a single writer; serialising every statement through one
	// connection keeps things simple and avoids SQLITE_BUSY between our own
	// goroutines. The busy timeout still covers other processes.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: open database %s: %w", path, err)
	}
	if err := initSchema(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func initSchema(db *sql.DB) error {
	for _, stmt := range schemaTables {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("store: create table: %w", err)
		}
	}
	// Additive migrations must run before the indexes: an old logs table does
	// not have the ch column yet.
	if err := migrate(db); err != nil {
		return fmt.Errorf("store: migrate schema: %w", err)
	}
	for _, stmt := range schemaIndexes {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("store: create index: %w", err)
		}
	}
	return nil
}

// migrate applies additive changes needed by databases created by older builds.
func migrate(db *sql.DB) error {
	return ensureLogChannel(db)
}

func scanSegment(rows *sql.Rows) (Segment, error) {
	var seg Segment
	err := rows.Scan(
		&seg.Ch, &seg.Start, &seg.End, &seg.Size, &seg.EventType,
		&seg.Status, &seg.SyncedAt, &seg.Path, &seg.Bytes, &seg.Err,
	)
	return seg, err
}
