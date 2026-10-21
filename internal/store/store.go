// Package store provides the JSON-backed persistence layer for nvrsync.
//
// All metadata lives in a single file, $DATA/store.json. Every mutation is
// serialized with a mutex, applied to the in-memory snapshot and then written
// to a temporary file which is renamed over store.json. This makes each write
// atomic: a reader either sees the complete old file or the complete new one.
package store

import (
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
	fileName    = "store.json"
	tmpFileName = "store.json.tmp"

	// maxLogs is the number of log entries kept in memory (and on disk).
	maxLogs = 5000

	// Log persistence is batched once the buffer grows: small stores are
	// flushed on every entry, larger ones at most every logPersistEvery
	// entries (or every logPersistAge, whichever comes first).
	logPersistEvery = 128
	logPersistAge   = 2 * time.Second

	// defaultSegmentLimit caps a Segments query with a non-positive Limit.
	defaultSegmentLimit = 5000

	formatVersion = 1
)

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
	BackfillH   int    `json:"backfill_hours"`
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

// storeData is the on-disk representation of the whole store.
type storeData struct {
	Version   int        `json:"version"`
	Settings  Settings   `json:"settings"`
	Segments  []Segment  `json:"segments"`
	Logs      []LogEntry `json:"logs"`
	SyncState SyncState  `json:"sync_state"`
}

// Store is a concurrency-safe JSON-backed metadata store.
type Store struct {
	dir  string
	path string

	// mu guards data. It is held for reads while copying values out and for
	// writes while mutating the in-memory state.
	mu   sync.RWMutex
	data storeData

	// writeMu serializes writers and, in particular, file writes. It is always
	// acquired before mu so the lock order is consistent.
	writeMu sync.Mutex

	// Log persistence bookkeeping, guarded by writeMu.
	logSeq      uint64
	savedLogSeq uint64
	lastLogSave time.Time
}

// Open opens (or creates) the store rooted at dir and reads $dir/store.json.
// When the file does not exist yet, an empty store is written immediately.
func Open(dir string) (*Store, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("store: data directory must not be empty")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("store: create data directory %q: %w", dir, err)
	}

	s := &Store{
		dir:  dir,
		path: filepath.Join(dir, fileName),
	}

	raw, err := os.ReadFile(s.path)
	switch {
	case err == nil:
		var d storeData
		if err := json.Unmarshal(raw, &d); err != nil {
			return nil, fmt.Errorf("store: decode %s: %w", s.path, err)
		}
		d.normalize()
		s.data = d
		s.logSeq = uint64(len(d.Logs))
		s.savedLogSeq = s.logSeq

	case errors.Is(err, os.ErrNotExist):
		s.data = storeData{
			Version:  formatVersion,
			Settings: Settings{Channels: []int{}},
			Segments: []Segment{},
			Logs:     []LogEntry{},
		}
		s.writeMu.Lock()
		err = s.writeFile(s.data)
		s.writeMu.Unlock()
		if err != nil {
			return nil, err
		}

	default:
		return nil, fmt.Errorf("store: read %s: %w", s.path, err)
	}

	return s, nil
}

// Settings returns a copy of the current settings.
func (s *Store) Settings() Settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneSettings(s.data.Settings)
}

// SaveSettings replaces the settings and persists them atomically.
func (s *Store) SaveSettings(settings Settings) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	s.mu.Lock()
	old := s.data.Settings
	s.data.Settings = cloneSettings(settings)
	snap := s.snapshotLocked()
	s.mu.Unlock()

	if err := s.writeFile(snap); err != nil {
		s.mu.Lock()
		s.data.Settings = old
		s.mu.Unlock()
		return err
	}
	s.noteSaved()
	return nil
}

// Segments returns the segments matching f, sorted by start time and then
// channel, together with the total number of matches before pagination.
// A non-positive Limit is capped at 5000; Offset is applied after sorting.
func (s *Store) Segments(f SegFilter) ([]Segment, int) {
	if f.Limit <= 0 {
		f.Limit = defaultSegmentLimit
	}
	if f.Offset < 0 {
		f.Offset = 0
	}

	s.mu.RLock()
	matches := make([]Segment, 0)
	if !(f.From > 0 && f.To > 0 && f.From > f.To) {
		for _, seg := range s.data.Segments {
			if !segmentMatches(seg, f) {
				continue
			}
			matches = append(matches, seg)
		}
	}
	s.mu.RUnlock()

	sort.Slice(matches, func(i, j int) bool {
		a, b := matches[i], matches[j]
		if a.Start != b.Start {
			return a.Start < b.Start
		}
		if a.Ch != b.Ch {
			return a.Ch < b.Ch
		}
		return a.End < b.End
	})

	total := len(matches)
	if f.Offset >= total {
		return []Segment{}, total
	}
	matches = matches[f.Offset:]
	if len(matches) > f.Limit {
		matches = matches[:f.Limit]
	}
	if matches == nil {
		matches = []Segment{}
	}
	return matches, total
}

// UpsertSegment inserts a segment or merges it into the existing one with the
// same (Ch, Start, End) key. On merge, non-zero incoming fields replace the
// stored values; a non-empty Status also replaces Err so that a successful
// retry clears a previous error.
func (s *Store) UpsertSegment(seg Segment) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	s.mu.Lock()
	idx := -1
	for i := range s.data.Segments {
		if sameKey(s.data.Segments[i], seg) {
			idx = i
			break
		}
	}
	var old Segment
	if idx >= 0 {
		old = s.data.Segments[idx]
		s.data.Segments[idx] = mergeSegment(old, seg)
	} else {
		s.data.Segments = append(s.data.Segments, seg)
	}
	snap := s.snapshotLocked()
	s.mu.Unlock()

	if err := s.writeFile(snap); err != nil {
		s.mu.Lock()
		if idx >= 0 {
			if idx < len(s.data.Segments) {
				s.data.Segments[idx] = old
			}
		} else {
			for i := len(s.data.Segments) - 1; i >= 0; i-- {
				if sameKey(s.data.Segments[i], seg) {
					s.data.Segments = append(s.data.Segments[:i], s.data.Segments[i+1:]...)
					break
				}
			}
		}
		s.mu.Unlock()
		return err
	}
	s.noteSaved()
	return nil
}

// DeleteSegment removes the segment identified by (ch, start, end). Deleting a
// missing segment is not an error.
func (s *Store) DeleteSegment(ch int, start, end int64) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	key := Segment{Ch: ch, Start: start, End: end}

	s.mu.Lock()
	idx := -1
	for i := range s.data.Segments {
		if sameKey(s.data.Segments[i], key) {
			idx = i
			break
		}
	}
	if idx < 0 {
		s.mu.Unlock()
		return nil
	}
	old := s.data.Segments[idx]
	s.data.Segments = append(s.data.Segments[:idx], s.data.Segments[idx+1:]...)
	snap := s.snapshotLocked()
	s.mu.Unlock()

	if err := s.writeFile(snap); err != nil {
		s.mu.Lock()
		restored := make([]Segment, 0, len(s.data.Segments)+1)
		restored = append(restored, s.data.Segments[:idx]...)
		restored = append(restored, old)
		restored = append(restored, s.data.Segments[idx:]...)
		s.data.Segments = restored
		s.mu.Unlock()
		return err
	}
	s.noteSaved()
	return nil
}

// MaxSyncedEnd returns the largest End among successfully synced segments for
// ch. A non-positive ch considers all channels. It returns 0 when there is no
// such segment.
func (s *Store) MaxSyncedEnd(ch int) int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var max int64
	for _, seg := range s.data.Segments {
		if seg.Status != "done" && seg.Status != "local_only" {
			continue
		}
		if ch > 0 && seg.Ch != ch {
			continue
		}
		if seg.End > max {
			max = seg.End
		}
	}
	return max
}

// Log appends a log entry, keeps at most maxLogs entries in memory/on disk and
// prints the entry to standard output with a timestamp.
func (s *Store) Log(level, kind, msg string) {
	now := time.Now()
	entry := LogEntry{TS: now.Unix(), Level: level, Kind: kind, Msg: msg}

	s.writeMu.Lock()
	s.mu.Lock()
	s.data.Logs = append(s.data.Logs, entry)
	if n := len(s.data.Logs); n > maxLogs {
		kept := make([]LogEntry, maxLogs)
		copy(kept, s.data.Logs[n-maxLogs:])
		s.data.Logs = kept
	}
	s.logSeq++
	seq := s.logSeq

	persist := s.shouldPersistLogLocked(now, seq)
	var snap storeData
	if persist {
		snap = s.snapshotLocked()
	}
	s.mu.Unlock()

	if persist {
		if err := s.writeFile(snap); err != nil {
			// Logging must not fail, so a failed flush only means the entry
			// will be written with the next snapshot. Throttle retries so a
			// broken disk does not turn every log line into a failed write.
			s.lastLogSave = now
			fmt.Fprintf(os.Stderr, "store: persist logs: %v\n", err)
		} else {
			s.savedLogSeq = seq
			s.lastLogSave = now
		}
	}
	s.writeMu.Unlock()

	fmt.Fprintf(os.Stdout, "%s [%s] %s: %s\n",
		now.Format("2006-01-02 15:04:05"), level, kind, msg)
}

// Logs returns up to limit log entries, newest first. A non-positive limit
// means all entries and an empty level means all levels.
func (s *Store) Logs(limit int, level string) []LogEntry {
	if limit < 0 {
		limit = 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]LogEntry, 0)
	for i := len(s.data.Logs) - 1; i >= 0; i-- {
		e := s.data.Logs[i]
		if level != "" && e.Level != level {
			continue
		}
		out = append(out, e)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

// Days aggregates successfully synced segments by local calendar day,
// newest day first. A non-zero ch restricts the result to that channel.
func (s *Store) Days(ch int) []DayInfo {
	type aggregate struct {
		chs      map[int]struct{}
		segments int
		seconds  int64
		bytes    int64
	}

	s.mu.RLock()
	days := make(map[string]*aggregate)
	for _, seg := range s.data.Segments {
		if seg.Status != "done" && seg.Status != "local_only" {
			continue
		}
		if ch != 0 && seg.Ch != ch {
			continue
		}
		date := time.Unix(seg.Start, 0).In(time.Local).Format("2006-01-02")
		agg := days[date]
		if agg == nil {
			agg = &aggregate{chs: make(map[int]struct{})}
			days[date] = agg
		}
		agg.chs[seg.Ch] = struct{}{}
		agg.segments++
		if d := seg.End - seg.Start; d > 0 {
			agg.seconds += d
		}
		agg.bytes += seg.Bytes
	}
	s.mu.RUnlock()

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

// SyncState returns the current sync progress.
func (s *Store) SyncState() SyncState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data.SyncState
}

// SetSyncState replaces the sync progress and persists it. Persistence
// failures are ignored because the syncer only reports progress through this
// method; the value is still updated in memory.
func (s *Store) SetSyncState(state SyncState) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	s.mu.Lock()
	old := s.data.SyncState
	s.data.SyncState = state
	snap := s.snapshotLocked()
	s.mu.Unlock()

	if err := s.writeFile(snap); err != nil {
		s.mu.Lock()
		s.data.SyncState = old
		s.mu.Unlock()
		return
	}
	s.noteSaved()
}

// shouldPersistLogLocked reports whether the just-appended log entry should be
// flushed now. s.mu and s.writeMu must be held.
func (s *Store) shouldPersistLogLocked(now time.Time, seq uint64) bool {
	// Small stores are cheap to flush and should be durable right away.
	if seq <= 512 {
		return true
	}
	if seq-s.savedLogSeq >= logPersistEvery {
		return true
	}
	// A store loaded from disk has no lastLogSave yet; flush the first new
	// entry immediately so recent history survives a quick restart.
	return s.lastLogSave.IsZero() || now.Sub(s.lastLogSave) >= logPersistAge
}

// noteSaved records that a snapshot containing the current logs was persisted.
// s.writeMu must be held.
func (s *Store) noteSaved() {
	s.savedLogSeq = s.logSeq
	s.lastLogSave = time.Now()
}

// snapshotLocked copies the in-memory state so it can be marshaled without
// holding the lock. s.mu must be held (read or write).
func (s *Store) snapshotLocked() storeData {
	d := s.data
	if s.data.Segments != nil {
		d.Segments = make([]Segment, len(s.data.Segments))
		copy(d.Segments, s.data.Segments)
	}
	if s.data.Logs != nil {
		d.Logs = make([]LogEntry, len(s.data.Logs))
		copy(d.Logs, s.data.Logs)
	}
	d.Settings = cloneSettings(s.data.Settings)
	return d
}

// writeFile atomically writes the snapshot to store.json via a temporary file
// and rename. s.writeMu must be held.
func (s *Store) writeFile(d storeData) error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("store: create data directory %q: %w", s.dir, err)
	}
	if d.Version == 0 {
		d.Version = formatVersion
	}
	raw, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return fmt.Errorf("store: encode: %w", err)
	}
	raw = append(raw, '\n')

	tmp := filepath.Join(s.dir, tmpFileName)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("store: create temp file: %w", err)
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
		return fmt.Errorf("store: write temp file: %w", werr)
	}

	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("store: replace %s: %w", s.path, err)
	}

	// Best-effort directory sync so the rename survives a power loss.
	if df, err := os.Open(s.dir); err == nil {
		_ = df.Sync()
		_ = df.Close()
	}
	return nil
}

// normalize fills in nil slices so the JSON shape stays stable.
func (d *storeData) normalize() {
	if d.Version == 0 {
		d.Version = formatVersion
	}
	if d.Segments == nil {
		d.Segments = []Segment{}
	}
	if d.Logs == nil {
		d.Logs = []LogEntry{}
	}
	if d.Settings.Channels == nil {
		d.Settings.Channels = []int{}
	}
	if len(d.Logs) > maxLogs {
		d.Logs = append([]LogEntry(nil), d.Logs[len(d.Logs)-maxLogs:]...)
	}
}

func cloneSettings(in Settings) Settings {
	out := in
	if in.Channels != nil {
		out.Channels = make([]int, len(in.Channels))
		copy(out.Channels, in.Channels)
	}
	return out
}

func sameKey(a, b Segment) bool {
	return a.Ch == b.Ch && a.Start == b.Start && a.End == b.End
}

// mergeSegment overlays the non-zero fields of in onto old.
func mergeSegment(old, in Segment) Segment {
	out := old
	if in.Size != 0 {
		out.Size = in.Size
	}
	if in.EventType != 0 {
		out.EventType = in.EventType
	}
	if in.Status != "" {
		out.Status = in.Status
		out.Err = in.Err
	}
	if in.SyncedAt != 0 {
		out.SyncedAt = in.SyncedAt
	}
	if in.Path != "" {
		out.Path = in.Path
	}
	if in.Bytes != 0 {
		out.Bytes = in.Bytes
	}
	if in.Err != "" {
		out.Err = in.Err
	}
	return out
}

func segmentMatches(seg Segment, f SegFilter) bool {
	if f.Ch > 0 && seg.Ch != f.Ch {
		return false
	}
	if f.Status != "" && seg.Status != f.Status {
		return false
	}
	if f.From > 0 && seg.End <= f.From {
		return false
	}
	if f.To > 0 && seg.Start >= f.To {
		return false
	}
	return true
}
