package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// legacyTestFileName is the pre-SQLite store file; it must be ignored.
const legacyTestFileName = "store.json"

func openStoreAt(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return s
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	return openStoreAt(t, t.TempDir())
}

func mustUpsert(t *testing.T, s *Store, seg Segment) {
	t.Helper()
	if err := s.UpsertSegment(seg); err != nil {
		t.Fatalf("UpsertSegment(%+v): %v", seg, err)
	}
}

func findSegment(t *testing.T, s *Store, ch int, start, end int64) Segment {
	t.Helper()
	segs, _ := s.Segments(SegFilter{Ch: ch, Limit: defaultSegmentLimit})
	for _, seg := range segs {
		if seg.Start == start && seg.End == end {
			return seg
		}
	}
	t.Fatalf("segment ch=%d %d-%d not found in %+v", ch, start, end, segs)
	return Segment{}
}

func readConfigFile(t *testing.T, dir string) Settings {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, configFileName))
	if err != nil {
		t.Fatalf("read config.json: %v", err)
	}
	var settings Settings
	if err := json.Unmarshal(raw, &settings); err != nil {
		t.Fatalf("config.json is not valid JSON: %v\n%s", err, raw)
	}
	return settings
}

// silenceStdout hides the Log() echo while a test emits many entries.
func silenceStdout(t *testing.T) {
	t.Helper()
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = devnull
	t.Cleanup(func() {
		os.Stdout = old
		_ = devnull.Close()
	})
}

// seedSegmentsDirect inserts n done segments without paying for one fsync per
// row; used by the pagination-cap test.
func seedSegmentsDirect(t *testing.T, s *Store, n int) {
	t.Helper()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(
		"INSERT INTO segments (ch, start_ts, end_ts, status) VALUES (?, ?, ?, ?)")
	if err != nil {
		t.Fatal(err)
	}
	defer stmt.Close()
	for i := 0; i < n; i++ {
		if _, err := stmt.Exec(1, int64(i*10), int64(i*10+10), "done"); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenInitializes(t *testing.T) {
	dir := t.TempDir()
	s := openStoreAt(t, dir)

	if _, err := os.Stat(filepath.Join(dir, configFileName)); err != nil {
		t.Fatalf("config.json not created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, dbFileName)); err != nil {
		t.Fatalf("nvrsync.db not created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, configTmpName)); !os.IsNotExist(err) {
		t.Fatalf("temporary config file left behind: %v", err)
	}
	// A fresh store must not create a legacy JSON file.
	if _, err := os.Stat(filepath.Join(dir, legacyTestFileName)); !os.IsNotExist(err) {
		t.Fatalf("store.json should not be created: %v", err)
	}

	fi, err := os.Stat(filepath.Join(dir, configFileName))
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("config.json mode = %o, want 600", perm)
	}

	// config.json holds Settings directly, not a wrapper document.
	raw, err := os.ReadFile(filepath.Join(dir, configFileName))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("config.json is not valid JSON: %v", err)
	}
	for _, forbidden := range []string{"version", "settings", "segments", "logs", "sync_state"} {
		if _, ok := doc[forbidden]; ok {
			t.Errorf("config.json unexpectedly contains %q: %s", forbidden, raw)
		}
	}
	if _, ok := doc["nvr_host"]; !ok {
		t.Errorf("config.json does not look like Settings: %s", raw)
	}

	if got := s.Settings(); !reflect.DeepEqual(got, Settings{Channels: []int{}}) {
		t.Errorf("Settings() = %+v, want empty settings", got)
	}
	if segs, total := s.Segments(SegFilter{}); total != 0 || len(segs) != 0 || segs == nil {
		t.Errorf("Segments() = %v, %d; want empty, 0", segs, total)
	}
	if logs := s.Logs(0, ""); len(logs) != 0 || logs == nil {
		t.Errorf("Logs() = %v, want empty non-nil", logs)
	}
	if days := s.Days(0); len(days) != 0 || days == nil {
		t.Errorf("Days() = %v, want empty non-nil", days)
	}
	if got := s.MaxSyncedEnd(0); got != 0 {
		t.Errorf("MaxSyncedEnd() = %d, want 0", got)
	}
	if got := s.SyncState(); got != (SyncState{}) {
		t.Errorf("SyncState() = %+v, want zero", got)
	}

	// Connection settings required by the deployment.
	var journal string
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&journal); err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(journal, "wal") {
		t.Errorf("journal_mode = %q, want wal", journal)
	}
	var busyTimeout int
	if err := s.db.QueryRow("PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
		t.Fatal(err)
	}
	if busyTimeout != 5000 {
		t.Errorf("busy_timeout = %d, want 5000", busyTimeout)
	}
	var foreignKeys int
	if err := s.db.QueryRow("PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		t.Fatal(err)
	}
	if foreignKeys != 1 {
		t.Errorf("foreign_keys = %d, want 1", foreignKeys)
	}
	if got := s.db.Stats().MaxOpenConnections; got != 1 {
		t.Errorf("MaxOpenConnections = %d, want 1", got)
	}

	// Reopening an existing store must not rewrite config.json.
	before, err := os.Stat(filepath.Join(dir, configFileName))
	if err != nil {
		t.Fatal(err)
	}
	reopened := openStoreAt(t, dir)
	if got := reopened.Settings(); !reflect.DeepEqual(got, Settings{Channels: []int{}}) {
		t.Errorf("reopened Settings() = %+v", got)
	}
	after, err := os.Stat(filepath.Join(dir, configFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("reopen rewrote config.json")
	}
}

func TestOpenErrors(t *testing.T) {
	if _, err := Open("   "); err == nil {
		t.Error(`Open("   ") should fail`)
	}

	dir := t.TempDir()
	blocker := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(filepath.Join(blocker, "sub")); err == nil {
		t.Error("Open under a regular file should fail")
	}

	badConfig := t.TempDir()
	if err := os.WriteFile(filepath.Join(badConfig, configFileName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(badConfig); err == nil {
		t.Error("Open with corrupt config.json should fail")
	}

	badDB := t.TempDir()
	if err := os.Mkdir(filepath.Join(badDB, dbFileName), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(badDB); err == nil {
		t.Error("Open with a directory at nvrsync.db should fail")
	}
}

// The new format deliberately does not read or rename a legacy store.json.
func TestLegacyStoreJSONIgnored(t *testing.T) {
	dir := t.TempDir()
	legacy := []byte(`{
		"version": 1,
		"settings": {"nvr_host": "legacy", "channels": [7]},
		"segments": [{"ch": 7, "start": 1, "end": 2, "status": "done"}],
		"logs": [{"ts": 1, "level": "info", "kind": "old", "msg": "old"}],
		"sync_state": {"running": true}
	}`)
	legacyPath := filepath.Join(dir, legacyTestFileName)
	if err := os.WriteFile(legacyPath, legacy, 0o644); err != nil {
		t.Fatal(err)
	}

	s := openStoreAt(t, dir)
	if got := s.Settings(); !reflect.DeepEqual(got, Settings{Channels: []int{}}) {
		t.Errorf("Settings() = %+v, want defaults (legacy file must be ignored)", got)
	}
	if _, total := s.Segments(SegFilter{}); total != 0 {
		t.Errorf("legacy segments imported: total = %d", total)
	}
	if logs := s.Logs(0, ""); len(logs) != 0 {
		t.Errorf("legacy logs imported: %+v", logs)
	}
	if got := s.SyncState(); got != (SyncState{}) {
		t.Errorf("legacy sync state imported: %+v", got)
	}

	kept, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatalf("legacy store.json was removed: %v", err)
	}
	if string(kept) != string(legacy) {
		t.Errorf("legacy store.json was modified")
	}
	if _, err := os.Stat(legacyPath + ".migrated"); !os.IsNotExist(err) {
		t.Errorf("legacy store.json was renamed: %v", err)
	}
}

func TestSettingsPersistAndCopy(t *testing.T) {
	dir := t.TempDir()
	s := openStoreAt(t, dir)

	channels := []int{1, 2}
	catalog := []int{3, 4}
	want := Settings{
		NVRHost:         "192.0.2.10",
		NVRUser:         "admin",
		NVRPass:         "secret",
		AdminPort:       8099,
		StorageDir:      "/srv/rec",
		Channels:        []int{1, 2},
		AutoMinutes:     15,
		BackfillH:       24,
		RetentionD:      7,
		Transcode:       true,
		FFmpeg:          "/usr/bin/ffmpeg",
		Cutoff:          "days",
		CutoffDays:      2,
		CatalogDays:     3,
		CatalogMinutes:  60,
		CatalogChannels: []int{3, 4},
	}
	if err := s.SaveSettings(want); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}

	// Mutating the caller's slices must not change the stored settings.
	channels[0] = 99
	catalog[0] = 99

	got := s.Settings()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Settings() = %+v, want %+v", got, want)
	}
	// Mutating the returned slices must not change the stored settings either.
	got.Channels[1] = 98
	got.CatalogChannels[1] = 97
	if again := s.Settings(); !reflect.DeepEqual(again, want) {
		t.Errorf("Settings() after caller mutation = %+v, want %+v", again, want)
	}

	// The file is written atomically, directly as Settings and mode 0600.
	if _, err := os.Stat(filepath.Join(dir, configTmpName)); !os.IsNotExist(err) {
		t.Errorf("temporary config file left behind: %v", err)
	}
	if onDisk := readConfigFile(t, dir); !reflect.DeepEqual(onDisk, want) {
		t.Errorf("config.json = %+v, want %+v", onDisk, want)
	}
	fi, err := os.Stat(filepath.Join(dir, configFileName))
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("config.json mode = %o, want 600", perm)
	}

	reopened := openStoreAt(t, dir)
	if reloaded := reopened.Settings(); !reflect.DeepEqual(reloaded, want) {
		t.Errorf("reopened Settings() = %+v, want %+v", reloaded, want)
	}
}

func TestSegmentsFilterSortAndPaginate(t *testing.T) {
	s := openTestStore(t)
	for _, seg := range []Segment{
		{Ch: 1, Start: 0, End: 100, Status: "done"},
		{Ch: 1, Start: 100, End: 200, Status: "done"},
		{Ch: 2, Start: 100, End: 200, Status: "pending"},
		{Ch: 1, Start: 100, End: 150, Status: "done"},
		{Ch: 1, Start: 100, End: 120, Status: "done"},
		{Ch: 1, Start: 200, End: 300, Status: "failed"},
	} {
		mustUpsert(t, s, seg)
	}

	all, total := s.Segments(SegFilter{})
	if total != 6 || len(all) != 6 {
		t.Fatalf("all = %d/%d, want 6/6", len(all), total)
	}
	gotOrder := make([][3]int64, 0, len(all))
	for _, seg := range all {
		gotOrder = append(gotOrder, [3]int64{int64(seg.Ch), seg.Start, seg.End})
	}
	wantOrder := [][3]int64{
		{1, 0, 100},
		{1, 100, 120},
		{1, 100, 150},
		{1, 100, 200},
		{2, 100, 200},
		{1, 200, 300},
	}
	if !reflect.DeepEqual(gotOrder, wantOrder) {
		t.Errorf("sort order = %v, want %v", gotOrder, wantOrder)
	}

	cases := []struct {
		name  string
		f     SegFilter
		first [3]int64
		last  [3]int64
		total int
	}{
		{"channel", SegFilter{Ch: 1}, [3]int64{1, 0, 100}, [3]int64{1, 200, 300}, 5},
		{"channel+status", SegFilter{Ch: 2, Status: "pending"}, [3]int64{2, 100, 200}, [3]int64{2, 100, 200}, 1},
		{"status", SegFilter{Status: "done"}, [3]int64{1, 0, 100}, [3]int64{1, 100, 200}, 4},
		{"overlap", SegFilter{From: 100, To: 200}, [3]int64{1, 100, 120}, [3]int64{2, 100, 200}, 4},
		{"end-boundary", SegFilter{To: 100}, [3]int64{1, 0, 100}, [3]int64{1, 0, 100}, 1},
		{"start-boundary", SegFilter{From: 200}, [3]int64{1, 200, 300}, [3]int64{1, 200, 300}, 1},
		{"touching", SegFilter{From: 100, To: 100}, [3]int64{}, [3]int64{}, 0},
		{"inverted", SegFilter{From: 500, To: 200}, [3]int64{}, [3]int64{}, 0},
		{"missing", SegFilter{Ch: 9}, [3]int64{}, [3]int64{}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			segs, total := s.Segments(tc.f)
			if total != tc.total {
				t.Fatalf("total = %d, want %d", total, tc.total)
			}
			if len(segs) != tc.total {
				t.Fatalf("len = %d, want %d", len(segs), tc.total)
			}
			if tc.total == 0 {
				if segs == nil {
					t.Errorf("empty result is nil, want non-nil")
				}
				return
			}
			if first := segs[0]; [3]int64{int64(first.Ch), first.Start, first.End} != tc.first {
				t.Errorf("first = %+v, want %v", first, tc.first)
			}
			if last := segs[len(segs)-1]; [3]int64{int64(last.Ch), last.Start, last.End} != tc.last {
				t.Errorf("last = %+v, want %v", last, tc.last)
			}
		})
	}

	// Limit and Offset apply after sorting.
	page, total := s.Segments(SegFilter{Limit: 2, Offset: 1})
	if total != 6 || len(page) != 2 {
		t.Fatalf("page = %d/%d, want 2/6", len(page), total)
	}
	if page[0].Start != 100 || page[0].End != 120 || page[1].Start != 100 || page[1].End != 150 {
		t.Errorf("page = %+v", page)
	}
	// A negative Offset means zero.
	firstPage, _ := s.Segments(SegFilter{Limit: 1, Offset: -5})
	if len(firstPage) != 1 || firstPage[0].Start != 0 {
		t.Errorf("negative offset page = %+v", firstPage)
	}
	// An Offset past the end returns an empty, non-nil slice but keeps total.
	empty, total := s.Segments(SegFilter{Offset: 100})
	if total != 6 || len(empty) != 0 || empty == nil {
		t.Errorf("offset past end = %v, total %d", empty, total)
	}
}

func TestSegmentsDefaultLimit(t *testing.T) {
	s := openTestStore(t)
	seedSegmentsDirect(t, s, defaultSegmentLimit+37)

	got, total := s.Segments(SegFilter{})
	if total != defaultSegmentLimit+37 {
		t.Errorf("total = %d, want %d", total, defaultSegmentLimit+37)
	}
	if len(got) != defaultSegmentLimit {
		t.Errorf("len = %d, want cap %d", len(got), defaultSegmentLimit)
	}
	if got[0].Start != 0 || got[len(got)-1].Start != int64((defaultSegmentLimit-1)*10) {
		t.Errorf("unexpected window: first %+v last %+v", got[0], got[len(got)-1])
	}
}

func TestUpsertSegmentMergesNonZeroFields(t *testing.T) {
	s := openTestStore(t)
	base := Segment{
		Ch: 1, Start: 100, End: 200, Size: 1000, EventType: 2,
		Status: "pending", SyncedAt: 10, Path: "a.ts", Bytes: 100, Err: "old",
	}
	mustUpsert(t, s, base)

	// A status-only update keeps the other fields and clears the old error,
	// even though the incoming Err is empty.
	mustUpsert(t, s, Segment{Ch: 1, Start: 100, End: 200, Status: "done"})
	got := findSegment(t, s, 1, 100, 200)
	want := Segment{
		Ch: 1, Start: 100, End: 200, Size: 1000, EventType: 2,
		Status: "done", SyncedAt: 10, Path: "a.ts", Bytes: 100, Err: "",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("after status update = %+v, want %+v", got, want)
	}

	// A non-empty Err without a status updates only the error.
	mustUpsert(t, s, Segment{Ch: 1, Start: 100, End: 200, Err: "boom"})
	want.Err = "boom"
	if got := findSegment(t, s, 1, 100, 200); !reflect.DeepEqual(got, want) {
		t.Errorf("after error update = %+v, want %+v", got, want)
	}

	// Zero values do not erase stored data.
	mustUpsert(t, s, Segment{Ch: 1, Start: 100, End: 200})
	if got := findSegment(t, s, 1, 100, 200); !reflect.DeepEqual(got, want) {
		t.Errorf("after zero update = %+v, want %+v", got, want)
	}

	// Every non-zero field replaces the stored value.
	full := Segment{
		Ch: 1, Start: 100, End: 200, Size: 2000, EventType: 3,
		Status: "failed", SyncedAt: 20, Path: "b.ts", Bytes: 200, Err: "bad",
	}
	mustUpsert(t, s, full)
	if got := findSegment(t, s, 1, 100, 200); !reflect.DeepEqual(got, full) {
		t.Errorf("after full update = %+v, want %+v", got, full)
	}
	if _, total := s.Segments(SegFilter{}); total != 1 {
		t.Errorf("total after upserts = %d, want 1", total)
	}

	// The same key in another channel or with another range is a new row.
	mustUpsert(t, s, Segment{Ch: 2, Start: 100, End: 200, Status: "done"})
	mustUpsert(t, s, Segment{Ch: 1, Start: 100, End: 201, Status: "done"})
	if _, total := s.Segments(SegFilter{}); total != 3 {
		t.Errorf("total after distinct keys = %d, want 3", total)
	}
}

func TestDeleteAndMaxSyncedEnd(t *testing.T) {
	dir := t.TempDir()
	s := openStoreAt(t, dir)
	for _, seg := range []Segment{
		{Ch: 1, Start: 100, End: 200, Status: "done"},
		{Ch: 1, Start: 300, End: 400, Status: "done"},
		{Ch: 1, Start: 500, End: 900, Status: "pending"},
		{Ch: 1, Start: 900, End: 1000, Status: "local_only"},
		{Ch: 2, Start: 700, End: 800, Status: "done"},
	} {
		mustUpsert(t, s, seg)
	}

	// local_only footage still exists on disk and must hold the watermark.
	if got := s.MaxSyncedEnd(1); got != 1000 {
		t.Errorf("MaxSyncedEnd(1) = %d, want 1000", got)
	}
	if got := s.MaxSyncedEnd(2); got != 800 {
		t.Errorf("MaxSyncedEnd(2) = %d, want 800", got)
	}
	if got := s.MaxSyncedEnd(0); got != 1000 {
		t.Errorf("MaxSyncedEnd(0) = %d, want 1000", got)
	}
	if got := s.MaxSyncedEnd(9); got != 0 {
		t.Errorf("MaxSyncedEnd(9) = %d, want 0", got)
	}

	if err := s.DeleteSegment(9, 1, 2); err != nil {
		t.Errorf("deleting a missing segment: %v", err)
	}
	if err := s.DeleteSegment(1, 100, 200); err != nil {
		t.Fatalf("DeleteSegment: %v", err)
	}
	if got := s.MaxSyncedEnd(1); got != 1000 {
		t.Errorf("MaxSyncedEnd(1) after delete = %d, want 1000", got)
	}
	if _, total := s.Segments(SegFilter{}); total != 4 {
		t.Errorf("total after delete = %d, want 4", total)
	}

	reopened := openStoreAt(t, dir)
	if _, total := reopened.Segments(SegFilter{}); total != 4 {
		t.Errorf("reopened total after delete = %d, want 4", total)
	}
	if got := reopened.MaxSyncedEnd(1); got != 1000 {
		t.Errorf("reopened MaxSyncedEnd(1) = %d, want 1000", got)
	}
}

func TestLogLimitAndOrder(t *testing.T) {
	dir := t.TempDir()
	s := openStoreAt(t, dir)
	silenceStdout(t)

	const total = maxLogs + 100
	for i := 0; i < total; i++ {
		level := "info"
		if i%3 == 0 {
			level = "error"
		}
		s.Log(level, "test", fmt.Sprintf("msg %d", i))
	}

	// A non-positive limit means the newest 1000 entries.
	recent := s.Logs(0, "")
	if len(recent) != defaultLogLimit {
		t.Fatalf("len(Logs(0)) = %d, want %d", len(recent), defaultLogLimit)
	}
	if recent[0].Msg != fmt.Sprintf("msg %d", total-1) {
		t.Errorf("newest log = %q, want msg %d", recent[0].Msg, total-1)
	}

	// At most maxLogs entries are kept.
	logs := s.Logs(maxLogs, "")
	if len(logs) != maxLogs {
		t.Fatalf("len(Logs) = %d, want %d", len(logs), maxLogs)
	}
	if logs[0].Msg != fmt.Sprintf("msg %d", total-1) {
		t.Errorf("newest log = %q, want msg %d", logs[0].Msg, total-1)
	}
	if logs[len(logs)-1].Msg != fmt.Sprintf("msg %d", total-maxLogs) {
		t.Errorf("oldest kept log = %q, want msg %d", logs[len(logs)-1].Msg, total-maxLogs)
	}
	for i := 1; i < len(logs); i++ {
		if logs[i-1].TS < logs[i].TS {
			t.Fatalf("logs not newest-first at %d", i)
		}
	}

	errorsOnly := s.Logs(5, "error")
	if len(errorsOnly) != 5 {
		t.Fatalf("len(error logs) = %d, want 5", len(errorsOnly))
	}
	for _, e := range errorsOnly {
		if e.Level != "error" {
			t.Errorf("level filter leaked %q", e.Level)
		}
	}
	if got := s.Logs(0, "warn"); len(got) != 0 || got == nil {
		t.Errorf("warn logs = %v, want empty non-nil", got)
	}

	reopened := openStoreAt(t, dir)
	if got := reopened.Logs(maxLogs, ""); len(got) != maxLogs {
		t.Errorf("reopened logs = %d, want %d", len(got), maxLogs)
	}
	if got := reopened.Logs(1, ""); len(got) != 1 || got[0].Msg != fmt.Sprintf("msg %d", total-1) {
		t.Errorf("reopened newest log = %+v", got)
	}
}

func TestDaysAggregatesLocalDates(t *testing.T) {
	oldLocal := time.Local
	time.Local = time.FixedZone("test-utc+8", 8*3600)
	t.Cleanup(func() { time.Local = oldLocal })

	s := openTestStore(t)
	at := func(day, hour int) int64 {
		return time.Date(2026, 10, day, hour, 0, 0, 0, time.Local).Unix()
	}
	const (
		day20 = "2026-10-20"
		day21 = "2026-10-21"
	)
	for _, seg := range []Segment{
		// 2026-10-20: two channels, done/local_only, one failed.
		{Ch: 1, Start: at(20, 23), End: at(21, 0), Status: "done", Bytes: 100},
		{Ch: 2, Start: at(20, 5), End: at(20, 6), Status: "done", Bytes: 50},
		{Ch: 1, Start: at(20, 7), End: at(20, 8), Status: "local_only", Bytes: 25},
		{Ch: 1, Start: at(20, 9), End: at(20, 10), Status: "failed", Bytes: 9999},
		// 2026-10-21: one done segment on channel 1.
		{Ch: 1, Start: at(21, 7), End: at(21, 8), Status: "done", Bytes: 30},
		// Wrong-case status is ignored.
		{Ch: 1, Start: at(22, 7), End: at(22, 8), Status: "DONE", Bytes: 1},
	} {
		mustUpsert(t, s, seg)
	}

	days := s.Days(0)
	want := []DayInfo{
		{Date: day21, Chs: 1, Segments: 1, Seconds: 3600, Bytes: 30},
		{Date: day20, Chs: 2, Segments: 3, Seconds: 3 * 3600, Bytes: 175},
	}
	if !reflect.DeepEqual(days, want) {
		t.Fatalf("Days() = %+v, want %+v", days, want)
	}

	ch1 := s.Days(1)
	wantCh1 := []DayInfo{
		{Date: day21, Chs: 1, Segments: 1, Seconds: 3600, Bytes: 30},
		{Date: day20, Chs: 1, Segments: 2, Seconds: 2 * 3600, Bytes: 125},
	}
	if !reflect.DeepEqual(ch1, wantCh1) {
		t.Errorf("Days(1) = %+v, want %+v", ch1, wantCh1)
	}
	if ch2 := s.Days(2); !reflect.DeepEqual(ch2, []DayInfo{
		{Date: day20, Chs: 1, Segments: 1, Seconds: 3600, Bytes: 50},
	}) {
		t.Errorf("Days(2) = %+v", ch2)
	}
	if none := s.Days(9); len(none) != 0 || none == nil {
		t.Errorf("Days(9) = %+v, want empty non-nil", none)
	}

	reopened := openStoreAt(t, dirFromStore(t, s))
	if got := reopened.Days(0); !reflect.DeepEqual(got, want) {
		t.Errorf("reopened Days() = %+v, want %+v", got, want)
	}
}

// dirFromStore exposes the directory of an open store for reopen tests.
func dirFromStore(t *testing.T, s *Store) string {
	t.Helper()
	return s.dir
}

func TestSyncStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := openStoreAt(t, dir)

	state := SyncState{
		Running:   true,
		Cur:       "cam1/2026-10-21/07-00-00_08-00-00.ts",
		Done:      3,
		Total:     10,
		Bytes:     123456,
		Speed:     2048.5,
		StartedAt: 1700000000,
		LastError: "boom",
	}
	s.SetSyncState(state)
	if got := s.SyncState(); got != state {
		t.Fatalf("SyncState() = %+v, want %+v", got, state)
	}

	// The state is persisted as a single JSON row.
	var data string
	if err := s.db.QueryRow("SELECT data FROM sync_state WHERE id = 1").Scan(&data); err != nil {
		t.Fatal(err)
	}
	var decoded SyncState
	if err := json.Unmarshal([]byte(data), &decoded); err != nil {
		t.Fatalf("sync_state.data is not valid JSON: %v", err)
	}
	if decoded != state {
		t.Errorf("stored sync state = %+v, want %+v", decoded, state)
	}
	var rows int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM sync_state").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Errorf("sync_state rows = %d, want 1", rows)
	}

	state.Running = false
	state.Done = 10
	s.SetSyncState(state)
	reopened := openStoreAt(t, dir)
	if got := reopened.SyncState(); got != state {
		t.Errorf("reopened SyncState() = %+v, want %+v", got, state)
	}
	var rowsAfter int
	if err := reopened.db.QueryRow("SELECT COUNT(*) FROM sync_state").Scan(&rowsAfter); err != nil {
		t.Fatal(err)
	}
	if rowsAfter != 1 {
		t.Errorf("sync_state rows after update = %d, want 1", rowsAfter)
	}
}

func TestConcurrentAccess(t *testing.T) {
	s := openTestStore(t)
	silenceStdout(t)

	for i := 0; i < 10; i++ {
		mustUpsert(t, s, Segment{Ch: 1, Start: int64(i * 60), End: int64(i*60 + 60), Status: "done"})
	}

	var wg sync.WaitGroup
	errs := make(chan error, 256)
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				if err := s.UpsertSegment(Segment{
					Ch: w + 2, Start: int64(i * 60), End: int64(i*60 + 60), Status: "done",
				}); err != nil {
					errs <- err
					return
				}
				s.Log("info", "sync", fmt.Sprintf("w%d-%d", w, i))
				s.SetSyncState(SyncState{Running: true, Cur: "x", Done: i, Total: 20})
			}
		}(w)
	}
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				if err := s.SaveSettings(Settings{Channels: []int{1, w + 2}}); err != nil {
					errs <- err
					return
				}
				got := s.Settings()
				if len(got.Channels) > 0 {
					got.Channels[0] = 99 // must not race with the store
				}
				s.Segments(SegFilter{Ch: 1})
				s.Logs(10, "info")
				s.Days(1)
				s.MaxSyncedEnd(1)
				_ = s.SyncState()
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("mixed access: %v", err)
	}

	if _, total := s.Segments(SegFilter{Limit: 10000}); total != 10+4*20 {
		t.Errorf("final total = %d, want %d", total, 10+4*20)
	}
	settings := s.Settings()
	if len(settings.Channels) != 2 || settings.Channels[0] != 1 {
		t.Errorf("final settings = %+v", settings)
	}
	if logs := s.Logs(defaultLogLimit, ""); len(logs) != 80 {
		t.Errorf("final logs = %d, want 80", len(logs))
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}
