package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

func mustUpsert(t *testing.T, s *Store, seg Segment) {
	t.Helper()
	if err := s.UpsertSegment(seg); err != nil {
		t.Fatalf("UpsertSegment(%+v): %v", seg, err)
	}
}

func readStoreJSON(t *testing.T, dir string) storeData {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, fileName))
	if err != nil {
		t.Fatalf("read store.json: %v", err)
	}
	var d storeData
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("store.json is not valid JSON: %v\n%s", err, raw)
	}
	return d
}

func TestOpenInitializes(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, fileName)); err != nil {
		t.Fatalf("store.json not created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, tmpFileName)); !os.IsNotExist(err) {
		t.Fatalf("temporary file left behind: %v", err)
	}

	d := readStoreJSON(t, dir)
	if d.Version != formatVersion {
		t.Errorf("version = %d, want %d", d.Version, formatVersion)
	}
	if d.Segments == nil || d.Logs == nil || d.Settings.Channels == nil {
		t.Errorf("slices should be initialized, got %+v", d)
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
	if days := s.Days(0); len(days) != 0 {
		t.Errorf("Days() = %v, want empty", days)
	}
	if got := s.MaxSyncedEnd(0); got != 0 {
		t.Errorf("MaxSyncedEnd() = %d, want 0", got)
	}

	// Reopening an existing store must not lose anything and must not rewrite
	// it unnecessarily.
	before, err := os.Stat(filepath.Join(dir, fileName))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	after, err := os.Stat(filepath.Join(dir, fileName))
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("reopen rewrote store.json")
	}
}

func TestOpenErrors(t *testing.T) {
	if _, err := Open("   "); err == nil {
		t.Error("Open(\"\") should fail")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(filepath.Join(path, "sub")); err == nil {
		t.Error("Open under a regular file should fail")
	}

	bad := t.TempDir()
	if err := os.WriteFile(filepath.Join(bad, fileName), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(bad); err == nil {
		t.Error("Open with corrupt JSON should fail")
	}
}

func TestSaveSettingsPersistsAndCopies(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	in := Settings{
		NVRHost:     "192.168.0.49:8000",
		NVRUser:     "admin",
		NVRPass:     "secret",
		AdminPort:   8081,
		StorageDir:  "/rec",
		Channels:    []int{1, 2, 3},
		AutoMinutes: 15,
		BackfillH:   24,
		RetentionD:  7,
		Transcode:   true,
		FFmpeg:      "/usr/bin/ffmpeg",
	}
	if err := s.SaveSettings(in); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}

	got := s.Settings()
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("Settings() = %+v, want %+v", got, in)
	}
	// The returned Channels slice must be a copy.
	got.Channels[0] = 99
	if again := s.Settings(); again.Channels[0] != 1 {
		t.Errorf("Settings() exposed its internal slice: %+v", again.Channels)
	}

	if _, err := os.Stat(filepath.Join(dir, tmpFileName)); !os.IsNotExist(err) {
		t.Errorf("temporary file left behind: %v", err)
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := reopened.Settings(); !reflect.DeepEqual(got, in) {
		t.Errorf("reopened settings = %+v, want %+v", got, in)
	}
}

func TestAtomicWriteFailurePreservesOldFile(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	old := Settings{NVRHost: "old", Channels: []int{1}}
	if err := s.SaveSettings(old); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}

	// A directory at the temp-file path makes creation of the temp file fail,
	// even for root. The existing store.json must stay untouched.
	tmp := filepath.Join(dir, tmpFileName)
	if err := os.Mkdir(tmp, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSettings(Settings{NVRHost: "new", Channels: []int{2}}); err == nil {
		t.Fatal("SaveSettings succeeded despite unwritable temp path")
	}

	if got := s.Settings(); got.NVRHost != "old" {
		t.Errorf("in-memory settings not rolled back: %+v", got)
	}
	if d := readStoreJSON(t, dir); d.Settings.NVRHost != "old" {
		t.Errorf("store.json was modified by failed write: %+v", d.Settings)
	}

	if err := os.Remove(tmp); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSettings(Settings{NVRHost: "new", Channels: []int{2}}); err != nil {
		t.Fatalf("SaveSettings after fix: %v", err)
	}
	if d := readStoreJSON(t, dir); d.Settings.NVRHost != "new" {
		t.Errorf("store.json = %+v, want new settings", d.Settings)
	}
}

func TestUpsertMergeAndUniqueKey(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	mustUpsert(t, s, Segment{
		Ch: 1, Start: 100, End: 200, Size: 1000, EventType: 1,
		Status: "pending",
	})
	// A progress update merges into the existing row instead of duplicating it.
	mustUpsert(t, s, Segment{Ch: 1, Start: 100, End: 200, Status: "syncing", Path: "cam1/a.ts"})
	mustUpsert(t, s, Segment{
		Ch: 1, Start: 100, End: 200, Status: "done",
		SyncedAt: 1234, Bytes: 900, Err: "",
	})
	// Same channel/start but a different end is a distinct segment.
	mustUpsert(t, s, Segment{Ch: 1, Start: 100, End: 300, Status: "pending"})

	segs, total := s.Segments(SegFilter{Ch: 1})
	if total != 2 || len(segs) != 2 {
		t.Fatalf("Segments() = %+v, total %d; want 2 rows", segs, total)
	}
	got := segs[0]
	want := Segment{
		Ch: 1, Start: 100, End: 200, Size: 1000, EventType: 1,
		Status: "done", SyncedAt: 1234, Path: "cam1/a.ts", Bytes: 900,
	}
	if got != want {
		t.Errorf("merged segment = %+v, want %+v", got, want)
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if segs, total := reopened.Segments(SegFilter{}); total != 2 || len(segs) != 2 {
		t.Errorf("reopened Segments() = %+v, total %d; want 2", segs, total)
	}
}

func TestSegmentsFilterSortAndPaginate(t *testing.T) {
	s := openTestStore(t)
	for _, seg := range []Segment{
		{Ch: 1, Start: 300, End: 400, Status: "done"},
		{Ch: 1, Start: 100, End: 200, Status: "pending"},
		{Ch: 1, Start: 500, End: 600, Status: "failed"},
		{Ch: 2, Start: 150, End: 250, Status: "done"},
		{Ch: 2, Start: 350, End: 450, Status: "done"},
	} {
		mustUpsert(t, s, seg)
	}

	starts := func(segs []Segment) []int64 {
		out := make([]int64, len(segs))
		for i, seg := range segs {
			out[i] = seg.Start
		}
		return out
	}

	all, total := s.Segments(SegFilter{})
	if total != 5 || !reflect.DeepEqual(starts(all), []int64{100, 150, 300, 350, 500}) {
		t.Errorf("all: starts %v total %d; want [100 150 300 350 500] 5", starts(all), total)
	}

	ch1, total := s.Segments(SegFilter{Ch: 1})
	if total != 3 || !reflect.DeepEqual(starts(ch1), []int64{100, 300, 500}) {
		t.Errorf("ch1: starts %v total %d", starts(ch1), total)
	}

	done, total := s.Segments(SegFilter{Status: "done"})
	if total != 3 || !reflect.DeepEqual(starts(done), []int64{150, 300, 350}) {
		t.Errorf("done: starts %v total %d", starts(done), total)
	}

	// Half-open overlap: [200,500) excludes [100,200) (ends at From) and
	// [500,600) (starts at To).
	ranged, total := s.Segments(SegFilter{From: 200, To: 500})
	if total != 3 || !reflect.DeepEqual(starts(ranged), []int64{150, 300, 350}) {
		t.Errorf("range: starts %v total %d", starts(ranged), total)
	}

	page, total := s.Segments(SegFilter{Limit: 2, Offset: 1})
	if total != 5 || !reflect.DeepEqual(starts(page), []int64{150, 300}) {
		t.Errorf("page: starts %v total %d", starts(page), total)
	}

	page, total = s.Segments(SegFilter{Limit: 2, Offset: 99})
	if total != 5 || len(page) != 0 {
		t.Errorf("offset past end: %v total %d", page, total)
	}

	empty, total := s.Segments(SegFilter{From: 500, To: 200})
	if total != 0 || len(empty) != 0 {
		t.Errorf("from > to: %v total %d", empty, total)
	}
}

func TestSegmentsDefaultLimit(t *testing.T) {
	s := openTestStore(t)
	s.mu.Lock()
	for i := 0; i < defaultSegmentLimit+37; i++ {
		s.data.Segments = append(s.data.Segments, Segment{
			Ch: 1, Start: int64(i * 10), End: int64(i*10 + 10), Status: "done",
		})
	}
	s.mu.Unlock()

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

func TestDeleteAndMaxSyncedEnd(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, seg := range []Segment{
		{Ch: 1, Start: 100, End: 200, Status: "done"},
		{Ch: 1, Start: 300, End: 400, Status: "done"},
		{Ch: 1, Start: 500, End: 900, Status: "pending"},
		{Ch: 2, Start: 700, End: 800, Status: "done"},
	} {
		mustUpsert(t, s, seg)
	}

	if got := s.MaxSyncedEnd(1); got != 400 {
		t.Errorf("MaxSyncedEnd(1) = %d, want 400", got)
	}
	if got := s.MaxSyncedEnd(2); got != 800 {
		t.Errorf("MaxSyncedEnd(2) = %d, want 800", got)
	}
	if got := s.MaxSyncedEnd(0); got != 800 {
		t.Errorf("MaxSyncedEnd(0) = %d, want 800", got)
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
	if got := s.MaxSyncedEnd(1); got != 400 {
		t.Errorf("MaxSyncedEnd(1) after delete = %d, want 400", got)
	}
	if _, total := s.Segments(SegFilter{}); total != 3 {
		t.Errorf("total after delete = %d, want 3", total)
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, total := reopened.Segments(SegFilter{}); total != 3 {
		t.Errorf("reopened total after delete = %d, want 3", total)
	}
}

func TestConcurrentUpsert(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	const workers, perWorker = 8, 25
	var wg sync.WaitGroup
	errs := make(chan error, workers*perWorker)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				seg := Segment{
					Ch:     w + 1,
					Start:  int64(i * 60),
					End:    int64(i*60 + 60),
					Status: "done",
					Bytes:  int64(1000 + i),
				}
				if err := s.UpsertSegment(seg); err != nil {
					errs <- err
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent upsert: %v", err)
	}

	segs, total := s.Segments(SegFilter{Limit: 10000})
	if total != workers*perWorker || len(segs) != workers*perWorker {
		t.Fatalf("total = %d, len = %d; want %d", total, len(segs), workers*perWorker)
	}
	seen := make(map[string]bool, len(segs))
	for _, seg := range segs {
		key := fmt.Sprintf("%d/%d/%d", seg.Ch, seg.Start, seg.End)
		if seen[key] {
			t.Fatalf("duplicate key %s", key)
		}
		seen[key] = true
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, total := reopened.Segments(SegFilter{Limit: 10000}); total != workers*perWorker {
		t.Errorf("reopened total = %d, want %d", total, workers*perWorker)
	}
}

func TestConcurrentMixedAccess(t *testing.T) {
	s := openTestStore(t)
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
}

func TestLogLimitAndOrder(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	// Keep the test output readable.
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	oldStdout := os.Stdout
	os.Stdout = devnull
	defer func() {
		os.Stdout = oldStdout
		_ = devnull.Close()
	}()

	const total = maxLogs + 100
	for i := 0; i < total; i++ {
		level := "info"
		if i%3 == 0 {
			level = "error"
		}
		s.Log(level, "test", fmt.Sprintf("msg %d", i))
	}

	logs := s.Logs(0, "")
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

	errors := s.Logs(5, "error")
	if len(errors) != 5 {
		t.Fatalf("len(error logs) = %d, want 5", len(errors))
	}
	for _, e := range errors {
		if e.Level != "error" {
			t.Errorf("level filter leaked %q", e.Level)
		}
	}
	if got := s.Logs(0, "warn"); len(got) != 0 {
		t.Errorf("warn logs = %v, want none", got)
	}

	// Any subsequent snapshot includes the current logs.
	if err := s.SaveSettings(Settings{Channels: []int{1}}); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Logs(0, ""); len(got) != maxLogs {
		t.Errorf("reopened logs = %d, want %d", len(got), maxLogs)
	}

	d := readStoreJSON(t, dir)
	if len(d.Logs) != maxLogs {
		t.Errorf("store.json logs = %d, want %d", len(d.Logs), maxLogs)
	}
}

func TestDays(t *testing.T) {
	s := openTestStore(t)
	at := func(day, hour int) int64 {
		return time.Date(2026, 10, day, hour, 0, 0, 0, time.Local).Unix()
	}
	day20 := "2026-10-20"
	day21 := "2026-10-21"

	for _, seg := range []Segment{
		// 2026-10-20: two channels, two done segments, one failed.
		{Ch: 1, Start: at(20, 23), End: at(21, 0), Status: "done", Bytes: 100},
		{Ch: 2, Start: at(20, 5), End: at(20, 6), Status: "done", Bytes: 50},
		{Ch: 1, Start: at(20, 7), End: at(20, 8), Status: "failed", Bytes: 9999},
		// 2026-10-21: one done segment on channel 1.
		{Ch: 1, Start: at(21, 7), End: at(21, 8), Status: "done", Bytes: 30},
		// Wrong-case status is ignored.
		{Ch: 1, Start: at(22, 7), End: at(22, 8), Status: "DONE", Bytes: 1},
	} {
		mustUpsert(t, s, seg)
	}

	days := s.Days(0)
	if len(days) != 2 {
		t.Fatalf("Days() = %+v, want 2 days", days)
	}
	if days[0].Date != day21 || days[1].Date != day20 {
		t.Fatalf("day order = %q, %q; want newest first", days[0].Date, days[1].Date)
	}
	want20 := DayInfo{Date: day20, Chs: 2, Segments: 2, Seconds: 3600 + 3600, Bytes: 150}
	if days[1] != want20 {
		t.Errorf("day 20 = %+v, want %+v", days[1], want20)
	}
	want21 := DayInfo{Date: day21, Chs: 1, Segments: 1, Seconds: 3600, Bytes: 30}
	if days[0] != want21 {
		t.Errorf("day 21 = %+v, want %+v", days[0], want21)
	}

	ch1 := s.Days(1)
	if len(ch1) != 2 || ch1[1].Chs != 1 || ch1[1].Segments != 1 || ch1[1].Bytes != 100 {
		t.Errorf("Days(1) = %+v", ch1)
	}
	if ch2 := s.Days(2); len(ch2) != 1 || ch2[0].Date != day20 || ch2[0].Bytes != 50 {
		t.Errorf("Days(2) = %+v", ch2)
	}
	if none := s.Days(9); len(none) != 0 {
		t.Errorf("Days(9) = %+v, want empty", none)
	}
}

func TestSyncStateJSONAndPersistence(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
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

	raw, err := os.ReadFile(filepath.Join(dir, fileName))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	var syncDoc map[string]json.RawMessage
	if err := json.Unmarshal(doc["sync_state"], &syncDoc); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"running", "cur", "done", "total", "bytes", "speed", "started_at", "last_error",
	} {
		if _, ok := syncDoc[key]; !ok {
			t.Errorf("sync_state JSON is missing %q: %s", key, doc["sync_state"])
		}
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.SyncState(); got != state {
		t.Errorf("reopened SyncState() = %+v, want %+v", got, state)
	}
}

func TestLogPersistsImmediatelyForSmallStore(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.Log("warn", "settings", "hello")

	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	logs := reopened.Logs(0, "")
	if len(logs) != 1 {
		t.Fatalf("reopened Logs() = %+v, want 1 entry", logs)
	}
	got := logs[0]
	if got.Level != "warn" || got.Kind != "settings" || got.Msg != "hello" || got.TS <= 0 {
		t.Errorf("reopened log = %+v", got)
	}
}
