// Package store provides the persistence layer for nvrsync.
//
// Settings live in a small JSON file, $DATA/config.json, which is rewritten
// atomically (write to a temporary file, fsync, rename). Everything else --
// segments, logs and sync progress -- lives in a SQLite database,
// $DATA/nvrsync.db, using the pure-Go modernc.org/sqlite driver.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	configFileName = "config.json"
	configTmpName  = "config.json.tmp"
	dbFileName     = "nvrsync.db"

	// maxLogs is the number of log entries kept in the database.
	maxLogs = 5000

	// defaultLogLimit is used by Logs when the caller asks for a
	// non-positive number of entries.
	defaultLogLimit = 1000

	// defaultSegmentLimit caps a Segments query with a non-positive Limit.
	defaultSegmentLimit = 5000
)

// DefaultAuthUser is the login name used when Settings.AuthUser is empty.
const DefaultAuthUser = "admin"

// Segment is one recording segment reported by the NVR.
type Segment struct {
	Ch        int    `json:"ch"`
	Start     int64  `json:"start"`
	End       int64  `json:"end"`
	Size      int64  `json:"size"`
	EventType int    `json:"event_type"`
	Status    string `json:"status"`
	SyncedAt  int64  `json:"synced_at"`
	Path      string `json:"path"`
	Bytes     int64  `json:"bytes"`
	Err       string `json:"err"`
}

// LogEntry is one line of sync history.
type LogEntry struct {
	TS    int64  `json:"ts"`
	Ch    int    `json:"ch"`
	Level string `json:"level"`
	Kind  string `json:"kind"`
	Msg   string `json:"msg"`
}

// Settings holds the user-configurable application settings.
type Settings struct {
	NVRHost     string `json:"nvr_host"`
	NVRUser     string `json:"nvr_user"`
	NVRPass     string `json:"nvr_pass"`
	AdminPort   int    `json:"admin_port"`
	StorageDir  string `json:"storage_dir"`
	Channels    []int  `json:"channels"`
	AutoMinutes int    `json:"auto_minutes"`
	RetentionD  int    `json:"retention_days"`
	Transcode   bool   `json:"transcode"`
	FFmpeg      string `json:"ffmpeg"`
	// Cutoff limits how recent a recording may be to be synced:
	// "latest" (now), "yesterday" (end of yesterday) or "days" (N days ago,
	// CutoffDays). Synchronising only closed days never touches a segment
	// that is still being written.
	Cutoff     string `json:"cutoff"`
	CutoffDays int    `json:"cutoff_days"`
	// CatalogDays is how far back the automatic NVR catalogue refresh looks
	// when reconciling the local index with what the NVR still holds. 0 disables
	// the automatic refresh.
	CatalogDays int `json:"catalog_days"`
	// CatalogMinutes is the background reconcile interval in minutes
	// (0 uses the built-in default).
	CatalogMinutes int `json:"catalog_minutes"`
	// CatalogChannels overrides which channels the catalogue refresh reads.
	// Empty means "the same channels as syncing".
	CatalogChannels []int `json:"catalog_channels"`

	// AuthEnabled turns on password protection for the web UI and API.
	// The zero value keeps the historical behaviour: no login required.
	AuthEnabled bool `json:"auth_enabled"`
	// AuthUser is the login name. Empty means DefaultAuthUser.
	AuthUser string `json:"auth_user"`
	// AuthHash is the bcrypt hash of the login password. It is persisted in
	// config.json but never returned by Settings or any HTTP endpoint.
	AuthHash string `json:"auth_hash"`
}

// SyncState describes the progress of the currently running (or last) sync.
// Cur is the label of the segment currently being downloaded, Done is the
// number of finished segments and Total is the planned segment count.
type SyncState struct {
	Running   bool    `json:"running"`
	Cur       string  `json:"cur"`
	Done      int     `json:"done"`
	Total     int     `json:"total"`
	Bytes     int64   `json:"bytes"`
	Speed     float64 `json:"speed"`
	StartedAt int64   `json:"started_at"`
	LastError string  `json:"last_error"`
}

// DayInfo is a per-day aggregate of successfully synced segments.
type DayInfo struct {
	Date     string `json:"date"`
	Chs      int    `json:"chs"`
	Segments int    `json:"segments"`
	Seconds  int64  `json:"seconds"`
	Bytes    int64  `json:"bytes"`
}

// SegFilter selects and paginates segments.
//
// Ch and Status are exact matches when non-zero/non-empty. From and To are
// unix seconds forming a half-open interval [From, To): a segment is included
// when it overlaps the interval, i.e. End > From and Start < To. Limit and
// Offset are applied after filtering and sorting; a non-positive Limit is
// capped at 5000 and a non-positive Offset means zero.
type SegFilter struct {
	Ch     int
	From   int64
	To     int64
	Status string
	Limit  int
	Offset int
}

// Store is a concurrency-safe persistence layer: settings in config.json and
// segments/logs/sync state in SQLite.
type Store struct {
	dir        string
	configPath string
	dbPath     string

	db *sql.DB

	// settingsMu guards settings. SaveSettings holds the write lock across
	// the atomic file write so readers never observe settings that are not
	// on disk yet.
	settingsMu sync.RWMutex
	settings   Settings
}

// ensureLogChannel adds the ch column to log tables created by older builds.
func ensureLogChannel(db *sql.DB) error {
	rows, err := db.Query("PRAGMA table_info(logs)")
	if err != nil {
		return err
	}
	defer rows.Close()
	has := false
	for rows.Next() {
		var cid, notNull, pk int
		var name, typ string
		var dflt any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			return err
		}
		if name == "ch" {
			has = true
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if has {
		return nil
	}
	_, err = db.Exec("ALTER TABLE logs ADD COLUMN ch INTEGER NOT NULL DEFAULT 0")
	return err
}

// Open opens (or creates) the store rooted at dir. It creates nvrsync.db and
// config.json as needed. A legacy store.json is deliberately ignored: this
// version has not shipped and does not migrate old data automatically.
func Open(dir string) (*Store, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("store: data directory must not be empty")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("store: create data directory %q: %w", dir, err)
	}

	s := &Store{
		dir:        dir,
		configPath: filepath.Join(dir, configFileName),
		dbPath:     filepath.Join(dir, dbFileName),
	}

	db, err := openDB(s.dbPath)
	if err != nil {
		return nil, err
	}
	s.db = db

	if err := s.loadOrCreateSettings(); err != nil {
		_ = db.Close()
		return nil, err
	}

	return s, nil
}

// Close releases the SQLite connection. It is safe to call more than once.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Settings returns a deep copy of the current settings. The password hash is
// cleared: config.json is the only place where it is stored, and it must never
// reach an API response.
func (s *Store) Settings() Settings {
	s.settingsMu.RLock()
	defer s.settingsMu.RUnlock()
	out := cloneSettings(s.settings)
	out.AuthHash = ""
	return out
}

// AuthHash returns the bcrypt hash of the login password, or "" when no
// password has been set. It is deliberately separate from Settings so that the
// hash cannot leak through the settings API by accident.
func (s *Store) AuthHash() string {
	s.settingsMu.RLock()
	defer s.settingsMu.RUnlock()
	return s.settings.AuthHash
}

// SaveSettings replaces the settings and persists them atomically to
// config.json. The in-memory value is only updated after the file write
// succeeded, so a failed save leaves the previous settings in place.
//
// AuthHash is never taken from the caller: Settings() strips it, so a settings
// round trip would otherwise erase the stored password. Use SetPassword to
// change it and ResetAuth to clear it.
func (s *Store) SaveSettings(settings Settings) error {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()

	settings.AuthHash = s.settings.AuthHash
	normalizeSettings(&settings)

	if err := s.writeConfig(settings); err != nil {
		return err
	}
	s.settings = cloneSettings(settings)
	return nil
}

// SetPassword stores the bcrypt hash of the login password under user and
// persists it. An empty user falls back to DefaultAuthUser. Enabling or
// disabling login protection is a separate, explicit decision.
func (s *Store) SetPassword(user, hash string) error {
	user = strings.TrimSpace(user)
	if user == "" {
		user = DefaultAuthUser
	}
	if strings.TrimSpace(hash) == "" {
		return errors.New("store: password hash must not be empty")
	}

	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()

	next := cloneSettings(s.settings)
	next.AuthUser = user
	next.AuthHash = hash
	if err := s.writeConfig(next); err != nil {
		return err
	}
	s.settings = next
	return nil
}

// ResetAuth disables login protection and clears the password hash, writing
// the result back to config.json. It is the recovery path behind -reset-auth.
func (s *Store) ResetAuth() error {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()

	next := cloneSettings(s.settings)
	next.AuthEnabled = false
	next.AuthHash = ""
	if err := s.writeConfig(next); err != nil {
		return err
	}
	s.settings = next
	return nil
}

// Segments returns the segments matching f, sorted by start time, channel and
// end time, together with the total number of matches before pagination.
// A non-positive Limit is capped at 5000; Offset is applied after sorting.
func (s *Store) Segments(f SegFilter) ([]Segment, int) {
	out := []Segment{}
	if f.Limit <= 0 {
		f.Limit = defaultSegmentLimit
	}
	if f.Offset < 0 {
		f.Offset = 0
	}
	// Preserve the historical behaviour of an explicitly inverted range:
	// there is no overlap to report.
	if f.From > 0 && f.To > 0 && f.From > f.To {
		return out, 0
	}

	where, args := segmentWhere(f)

	// The count and the page come from one read transaction so a concurrent
	// writer cannot make the total disagree with the returned rows.
	tx, err := s.db.Begin()
	if err != nil {
		return out, 0
	}
	defer tx.Rollback() //nolint:errcheck // read-only transaction

	var total int
	if err := tx.QueryRow("SELECT COUNT(*) FROM segments"+where, args...).Scan(&total); err != nil {
		return out, 0
	}
	if f.Offset >= total {
		return out, total
	}

	query := "SELECT " + segmentColumns + " FROM segments" + where +
		" ORDER BY start_ts ASC, ch ASC, end_ts ASC LIMIT ? OFFSET ?"
	queryArgs := make([]any, 0, len(args)+2)
	queryArgs = append(queryArgs, args...)
	queryArgs = append(queryArgs, f.Limit, f.Offset)

	rows, err := tx.Query(query, queryArgs...)
	if err != nil {
		return out, total
	}

	for rows.Next() {
		seg, err := scanSegment(rows)
		if err != nil {
			rows.Close()
			return out, total
		}
		out = append(out, seg)
	}
	rows.Close()
	return out, total
}

// UpsertSegment inserts a segment or merges it into the existing one with the
// same (Ch, Start, End) key. On merge, non-zero incoming fields replace the
// stored values; a non-empty Status also replaces Err so that a successful
// retry clears a previous error.
func (s *Store) UpsertSegment(seg Segment) error {
	_, err := s.db.Exec(upsertSegmentSQL,
		seg.Ch, seg.Start, seg.End, seg.Size, seg.EventType, seg.Status,
		seg.SyncedAt, seg.Path, seg.Bytes, seg.Err,
	)
	if err != nil {
		return fmt.Errorf("store: upsert segment: %w", err)
	}
	return nil
}

// DeleteSegment removes the segment identified by (ch, start, end). Deleting a
// missing segment is not an error.
func (s *Store) DeleteSegment(ch int, start, end int64) error {
	_, err := s.db.Exec(
		"DELETE FROM segments WHERE ch = ? AND start_ts = ? AND end_ts = ?",
		ch, start, end,
	)
	if err != nil {
		return fmt.Errorf("store: delete segment: %w", err)
	}
	return nil
}

// Log appends a log entry, keeps at most maxLogs entries in the database and
// prints the entry to standard output with a timestamp. Logging failures are
// reported to standard error but never returned, because logging must not
// break a sync.
func (s *Store) Log(level, kind, msg string) {
	s.LogCh(0, level, kind, msg)
}

// LogCh appends a log entry tagged with the channel it belongs to (0 = the
// process as a whole).
func (s *Store) LogCh(ch int, level, kind, msg string) {
	now := time.Now()
	entry := LogEntry{TS: now.Unix(), Ch: ch, Level: level, Kind: kind, Msg: msg}
	if err := s.appendLog(entry); err != nil {
		fmt.Fprintf(os.Stderr, "store: append log: %v\n", err)
	}
	fmt.Fprintf(os.Stdout, "%s [%s] %s: %s\n",
		now.Format("2006-01-02 15:04:05"), level, kind, msg)
}

// Logs returns up to limit log entries, newest first. A non-positive limit
// means 1000 entries and an empty level means all levels.
func (s *Store) Logs(limit int, level string, ch int) []LogEntry {
	if limit <= 0 {
		limit = defaultLogLimit
	}

	query := "SELECT ts, ch, level, kind, msg FROM logs"
	args := []any{}
	var wheres []string
	if level != "" {
		wheres = append(wheres, "level = ?")
		args = append(args, level)
	}
	if ch > 0 {
		wheres = append(wheres, "ch = ?")
		args = append(args, ch)
	}
	if len(wheres) > 0 {
		query += " WHERE " + strings.Join(wheres, " AND ")
	}
	query += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)

	out := []LogEntry{}
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return out
	}
	defer rows.Close()

	for rows.Next() {
		var e LogEntry
		if err := rows.Scan(&e.TS, &e.Ch, &e.Level, &e.Kind, &e.Msg); err != nil {
			return out
		}
		out = append(out, e)
	}
	return out
}

// Days aggregates successfully synced segments by local calendar day, newest
// day first. A non-zero ch restricts the result to that channel.
//
// Seconds are the union of the playable intervals per channel, not the sum of
// every row: a motion clip and the continuous recording containing it must not
// make a day look twice as long. Bytes still sum every file because overlapping
// segments really do occupy disk.
func (s *Store) Days(ch int) []DayInfo {
	type aggregate struct {
		chs      map[int]struct{}
		segments int
		seconds  int64
		bytes    int64
		lastEnd  map[int]int64
	}
	query := "SELECT ch, start_ts, end_ts, bytes FROM segments " +
		"WHERE status IN ('done', 'local_only')"
	args := []any{}
	if ch != 0 {
		query += " AND ch = ?"
		args = append(args, ch)
	}
	// A single ordered scan lets us merge overlapping intervals in O(n).
	query += " ORDER BY start_ts ASC, ch ASC, end_ts ASC"

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return []DayInfo{}
	}
	defer rows.Close()

	days := make(map[string]*aggregate)
	for rows.Next() {
		var (
			segCh int
			start int64
			end   int64
			bytes int64
		)
		if err := rows.Scan(&segCh, &start, &end, &bytes); err != nil {
			break
		}
		date := time.Unix(start, 0).In(time.Local).Format("2006-01-02")
		agg := days[date]
		if agg == nil {
			agg = &aggregate{
				chs:     make(map[int]struct{}),
				lastEnd: make(map[int]int64),
			}
			days[date] = agg
		}
		agg.chs[segCh] = struct{}{}
		agg.segments++
		agg.bytes += bytes
		if end <= start {
			continue
		}
		prevEnd := agg.lastEnd[segCh]
		if start < prevEnd {
			start = prevEnd
		}
		if end > start {
			agg.seconds += end - start
		}
		if end > prevEnd {
			agg.lastEnd[segCh] = end
		}
	}

	out := make([]DayInfo, 0, len(days))
	for date, agg := range days {
		out = append(out, DayInfo{
			Date:     date,
			Chs:      len(agg.chs),
			Segments: agg.segments,
			Seconds:  agg.seconds,
			Bytes:    agg.bytes,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date > out[j].Date })
	return out
}

// Ping reports whether the SQLite handle is usable. It is used by /api/ready.
func (s *Store) Ping() error {
	if s == nil || s.db == nil {
		return errors.New("store: not open")
	}
	return s.db.Ping()
}

// SegmentsAll returns every segment matching f without a COUNT or LIMIT. It is
// used by internal scanners (catalog, retention, queue) that need the whole
// set; callers should not expose it directly to paginated HTTP clients.
func (s *Store) SegmentsAll(f SegFilter) []Segment {
	out := []Segment{}
	if f.From > 0 && f.To > 0 && f.From > f.To {
		return out
	}
	where, args := segmentWhere(f)
	query := "SELECT " + segmentColumns + " FROM segments" + where +
		" ORDER BY start_ts ASC, ch ASC, end_ts ASC"
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		seg, err := scanSegment(rows)
		if err != nil {
			break
		}
		out = append(out, seg)
	}
	return out
}

// QueueSegments returns the pending/failed/syncing rows for the given channels
// in one query. collectQueue used to issue one paginated query per channel and
// status, which grew quadratically with the number of rows.
func (s *Store) QueueSegments(channels []int, statuses []string) []Segment {
	out := []Segment{}
	channels = normalizeChannels(channels)
	statuses = uniqueStrings(statuses)
	if len(channels) == 0 || len(statuses) == 0 {
		return out
	}
	chPlaceholders := strings.TrimRight(strings.Repeat("?,", len(channels)), ",")
	stPlaceholders := strings.TrimRight(strings.Repeat("?,", len(statuses)), ",")
	query := "SELECT " + segmentColumns + " FROM segments WHERE ch IN (" + chPlaceholders +
		") AND status IN (" + stPlaceholders + ") ORDER BY start_ts ASC, ch ASC, end_ts ASC"
	args := make([]any, 0, len(channels)+len(statuses))
	for _, ch := range channels {
		args = append(args, ch)
	}
	for _, status := range statuses {
		args = append(args, status)
	}
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		seg, err := scanSegment(rows)
		if err != nil {
			break
		}
		out = append(out, seg)
	}
	return out
}

func uniqueStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

// SyncState returns the current sync progress.
func (s *Store) SyncState() SyncState {
	var data string
	if err := s.db.QueryRow("SELECT data FROM sync_state WHERE id = 1").Scan(&data); err != nil {
		return SyncState{}
	}
	var state SyncState
	if err := json.Unmarshal([]byte(data), &state); err != nil {
		return SyncState{}
	}
	return state
}

// SetSyncState replaces the sync progress and persists it. Persistence
// failures are ignored because the syncer only reports progress through this
// method.
func (s *Store) SetSyncState(state SyncState) {
	raw, err := json.Marshal(state)
	if err != nil {
		return
	}
	_, _ = s.db.Exec(
		"INSERT INTO sync_state (id, data) VALUES (1, ?) "+
			"ON CONFLICT(id) DO UPDATE SET data = excluded.data",
		string(raw),
	)
}

// appendLog writes one entry and trims the table to maxLogs rows. The insert
// and the trim share a transaction so the log never exceeds the cap on disk.
func (s *Store) appendLog(entry LogEntry) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is a no-op

	if _, err := tx.Exec(
		"INSERT INTO logs (ts, ch, level, kind, msg) VALUES (?, ?, ?, ?, ?)",
		entry.TS, entry.Ch, entry.Level, entry.Kind, entry.Msg,
	); err != nil {
		return err
	}
	// "OFFSET maxLogs" selects the oldest row that is still allowed to
	// stay; everything at or below it is dropped. Keeping the trim in SQL
	// avoids loading the whole log into memory on every write.
	if _, err := tx.Exec(
		"DELETE FROM logs WHERE id <= "+
			"(SELECT id FROM logs ORDER BY id DESC LIMIT 1 OFFSET ?)",
		maxLogs,
	); err != nil {
		return err
	}
	return tx.Commit()
}

// loadOrCreateSettings reads config.json, creating it with default settings
// when it does not exist yet.
func (s *Store) loadOrCreateSettings() error {
	raw, err := os.ReadFile(s.configPath)
	switch {
	case err == nil:
		var settings Settings
		if err := json.Unmarshal(raw, &settings); err != nil {
			return fmt.Errorf("store: decode %s: %w", s.configPath, err)
		}
		normalizeSettings(&settings)
		s.settings = settings
		return nil

	case errors.Is(err, os.ErrNotExist):
		settings := Settings{Channels: []int{}}
		if err := s.writeConfig(settings); err != nil {
			return err
		}
		s.settings = settings
		return nil

	default:
		return fmt.Errorf("store: read %s: %w", s.configPath, err)
	}
}

// writeConfig atomically writes settings to config.json: a temporary file in
// the same directory is written, fsynced and then renamed over the target.
func (s *Store) writeConfig(settings Settings) error {
	raw, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("store: encode settings: %w", err)
	}
	raw = append(raw, '\n')

	tmp := filepath.Join(s.dir, configTmpName)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("store: create %s: %w", tmp, err)
	}

	_, werr := f.Write(raw)
	if werr == nil {
		werr = f.Sync()
	}
	cerr := f.Close()
	if werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("store: write %s: %w", tmp, werr)
	}

	if err := os.Rename(tmp, s.configPath); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("store: replace %s: %w", s.configPath, err)
	}

	// Best-effort directory sync so the rename survives a power loss.
	if df, err := os.Open(s.dir); err == nil {
		_ = df.Sync()
		_ = df.Close()
	}
	return nil
}

// segmentWhere builds the WHERE clause shared by the count and page queries.
func segmentWhere(f SegFilter) (string, []any) {
	conds := make([]string, 0, 4)
	args := make([]any, 0, 4)
	if f.Ch > 0 {
		conds = append(conds, "ch = ?")
		args = append(args, f.Ch)
	}
	if f.Status != "" {
		conds = append(conds, "status = ?")
		args = append(args, f.Status)
	}
	if f.From > 0 {
		conds = append(conds, "end_ts > ?")
		args = append(args, f.From)
	}
	if f.To > 0 {
		conds = append(conds, "start_ts < ?")
		args = append(args, f.To)
	}
	if len(conds) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

func cloneSettings(in Settings) Settings {
	out := in
	if in.Channels != nil {
		out.Channels = make([]int, len(in.Channels))
		copy(out.Channels, in.Channels)
	}
	if in.CatalogChannels != nil {
		out.CatalogChannels = make([]int, len(in.CatalogChannels))
		copy(out.CatalogChannels, in.CatalogChannels)
	}
	return out
}

// normalizeSettings keeps the persisted JSON shape stable and removes
// duplicated/negative channel numbers. Older builds allowed the settings API
// to store duplicates (for example [1,1,2,2,...]), which made the UI and some
// loops do the same work twice.
func normalizeSettings(settings *Settings) {
	if settings.Channels == nil {
		settings.Channels = []int{}
	}
	settings.Channels = normalizeChannels(settings.Channels)
	// CatalogChannels keeps its nil shape: nil means "follow Channels".
	if settings.CatalogChannels != nil {
		settings.CatalogChannels = normalizeChannels(settings.CatalogChannels)
	}
}

// normalizeChannels returns a sorted, de-duplicated copy of in, dropping
// non-positive values. A nil input stays nil so callers can distinguish "not
// configured" from an explicitly empty list.
func normalizeChannels(in []int) []int {
	if in == nil {
		return nil
	}
	seen := make(map[int]struct{}, len(in))
	out := make([]int, 0, len(in))
	for _, ch := range in {
		if ch <= 0 {
			continue
		}
		if _, ok := seen[ch]; ok {
			continue
		}
		seen[ch] = struct{}{}
		out = append(out, ch)
	}
	sort.Ints(out)
	return out
}
