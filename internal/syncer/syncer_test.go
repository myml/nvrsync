package syncer

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/myml/nvrsync/internal/nvr"
	"github.com/myml/nvrsync/internal/store"
)

// fakeNVR serves the /ds management API over HTTP and downloads over a raw
// TCP listener. Each download returns two TS packets and a finished notice.
type fakeNVR struct {
	srv      *httptest.Server
	ln       net.Listener
	segs     [][2]int64
	mu       sync.Mutex
	channels []string
	listings int
}

func newFakeNVR(t *testing.T, segs [][2]int64) *fakeNVR {
	f := newFakeNVRBase(t, segs)
	go f.acceptStreams()
	return f
}

// newFakeNVRBase builds the /ds server and a stream listener without an
// accept loop, so a test can install its own behavior.
func newFakeNVRBase(t *testing.T, segs [][2]int64) *fakeNVR {
	t.Helper()
	f := &fakeNVR{segs: segs}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handleDS))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.ln = ln
	t.Cleanup(func() {
		f.srv.Close()
		ln.Close()
	})
	return f
}

func (f *fakeNVR) handleDS(w http.ResponseWriter, r *http.Request) {
	var req struct {
		System json.RawMessage `json:"system"`
		Media  struct {
			GetMediaList struct {
				Channel   []string `json:"channel"`
				StartTime string   `json:"start_time"`
			} `json:"get_media_list"`
		} `json:"media"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/":
		io.WriteString(w, `{"error_code":0,"stok":"tok"}`)
	case len(req.System) > 0:
		io.WriteString(w, `{"error_code":0,"user_id":9}`)
	default:
		q := req.Media.GetMediaList
		f.mu.Lock()
		f.channels = append(f.channels, q.Channel...)
		f.listings++
		f.mu.Unlock()
		var st, et, sz, ev []string
		for _, s := range f.segs {
			st = append(st, strconv.FormatInt(s[0], 10))
			et = append(et, strconv.FormatInt(s[1], 10))
			sz = append(sz, "376")
			ev = append(ev, "1")
		}
		json.NewEncoder(w).Encode(map[string]any{
			"error_code": 0,
			"media": map[string]any{
				"start_time": st, "end_time": et, "size": sz,
				"event_type": ev, "media_type": []string{"0"}, "channel": q.Channel,
			},
		})
	}
}

func (f *fakeNVR) acceptStreams() {
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.serveStream(conn)
	}
}

func (f *fakeNVR) serveStream(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(conn)
	if _, err := readHead(br); err != nil {
		return
	}
	fmt.Fprint(conn, "HTTP/1.1 401 Unauthorized\r\nWWW-Authenticate: Digest realm=\"TP-Link IP-Camera\", nonce=\"n\", opaque=\"o\"\r\nContent-Length: 0\r\n\r\n")
	if _, err := readHead(br); err != nil {
		return
	}
	fmt.Fprint(conn, "HTTP/1.0 200 OK\r\nContent-Type: multipart/mixed;boundary=--device--\r\n\r\n")
	writePart := func(ct string, body []byte) {
		fmt.Fprintf(conn, "----device--\r\nContent-Type: %s\r\nContent-Length: %d\r\n\r\n", ct, len(body))
		conn.Write(body)
		io.WriteString(conn, "\r\n")
	}
	writePart("application/json", []byte(`{"params":{"session_id":"s1"}}`))
	writePart("video/mp2t", append(tsPacketNull(), tsPacketNull()...))
	writePart("application/json", []byte(`{"params":{"stream_status":"finished"}}`))
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	io.Copy(io.Discard, conn)
}

func readHead(br *bufio.Reader) ([]string, error) {
	var lines []string
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			return lines, nil
		}
		lines = append(lines, line)
	}
}

func tsPacketNull() []byte {
	p := make([]byte, 188)
	p[0] = 0x47
	p[1] = 0x1f
	p[2] = 0xff
	p[3] = 0x10
	return p
}

func (f *fakeNVR) host() string { return f.ln.Addr().String() }

func (f *fakeNVR) adminPort() int {
	_, p, _ := net.SplitHostPort(f.srv.Listener.Addr().String())
	n, _ := strconv.Atoi(p)
	return n
}

func TestSyncDownloadsSegments(t *testing.T) {
	now := time.Now().Unix()
	f := newFakeNVR(t, [][2]int64{{now - 900, now - 780}, {now - 780, now - 660}})
	dataDir := t.TempDir()
	st, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	storage := filepath.Join(dataDir, "storage")
	err = st.SaveSettings(store.Settings{
		NVRHost: f.host(), NVRUser: "admin", NVRPass: "secret",
		AdminPort: f.adminPort(), StorageDir: storage,
		Channels: []int{1}, BackfillH: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	s := New(st)
	if err := s.Start(SyncRequest{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitDone(t, s, st, 2)

	segs, total := st.Segments(store.SegFilter{Ch: 1})
	if total != 2 {
		t.Fatalf("segments = %d, want 2", total)
	}
	for _, seg := range segs {
		if seg.Status != "done" {
			t.Errorf("segment %s status = %q, want done (err %q)", segKeyString(seg), seg.Status, seg.Err)
			continue
		}
		if seg.Bytes != 376 || seg.SyncedAt == 0 {
			t.Errorf("segment %s bytes=%d synced=%d, want 376 / non-zero", segKeyString(seg), seg.Bytes, seg.SyncedAt)
		}
		if !strings.HasPrefix(seg.Path, "rec/cam1/") || !strings.HasSuffix(seg.Path, ".ts") {
			t.Errorf("path = %q, want rec/cam1/.../*.ts", seg.Path)
		}
		p := filepath.Join(storage, filepath.FromSlash(seg.Path))
		fi, err := os.Stat(p)
		if err != nil {
			t.Errorf("recording missing: %v", err)
			continue
		}
		if fi.Size() != 376 {
			t.Errorf("file size = %d, want 376", fi.Size())
		}
		raw, err := os.ReadFile(p + ".json")
		if err != nil {
			t.Errorf("sidecar missing: %v", err)
			continue
		}
		var sc struct {
			Ch    int   `json:"ch"`
			Start int64 `json:"start"`
			Stats struct {
				Bytes   int64 `json:"bytes"`
				Packets int64 `json:"packets"`
			} `json:"stats"`
		}
		if err := json.Unmarshal(raw, &sc); err != nil {
			t.Errorf("sidecar invalid: %v", err)
			continue
		}
		if sc.Ch != 1 || sc.Start != seg.Start || sc.Stats.Bytes != 376 || sc.Stats.Packets != 2 {
			t.Errorf("sidecar = %+v, want ch1 start %d bytes 376 packets 2", sc, seg.Start)
		}
	}

	state := st.SyncState()
	if state.Running || state.Done != 2 || state.Total != 2 || state.Bytes != 752 || state.Speed <= 0 {
		t.Errorf("sync state = %+v, want finished 2/2 752 bytes", state)
	}
	if logs := st.Logs(0, ""); len(logs) == 0 {
		t.Error("expected sync logs")
	}
	f.mu.Lock()
	listings, channels := f.listings, append([]string(nil), f.channels...)
	f.mu.Unlock()
	if listings == 0 {
		t.Error("media list was never called")
	}
	if len(channels) == 0 || channels[0] != "0" {
		t.Errorf("channels sent = %v, want [0] (physical 1)", channels)
	}
}

func TestRetentionOnlyDeletesDoneInsideStorage(t *testing.T) {
	dataDir := t.TempDir()
	st, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	storage := filepath.Join(dataDir, "storage")
	old := time.Now().Add(-10 * 24 * time.Hour).Unix()
	recent := time.Now().Add(-time.Hour).Unix()

	mk := func(rel string) string {
		p := filepath.Join(storage, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, tsPacketNull(), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	expiredRel := "rec/cam1/2000-01-01/00-00-00_00-01-00.ts"
	expired := mk(expiredRel)
	mk(expiredRel + ".json")
	pendingRel := "rec/cam2/2000-01-01/00-00-00_00-01-00.ts"
	pending := mk(pendingRel)
	recentRel := "rec/cam3/2026-01-01/00-00-00_00-01-00.ts"
	recentFile := mk(recentRel)
	outside := filepath.Join(dataDir, "outside.ts")
	if err := os.WriteFile(outside, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}

	mustUpsert := func(seg store.Segment) {
		t.Helper()
		if err := st.UpsertSegment(seg); err != nil {
			t.Fatal(err)
		}
	}
	mustUpsert(store.Segment{Ch: 1, Start: old, End: old + 60, Status: "done", Path: expiredRel, Bytes: 188})
	mustUpsert(store.Segment{Ch: 2, Start: old, End: old + 60, Status: "pending", Path: pendingRel})
	mustUpsert(store.Segment{Ch: 3, Start: recent, End: recent + 60, Status: "done", Path: recentRel, Bytes: 188})
	mustUpsert(store.Segment{Ch: 4, Start: old, End: old + 60, Status: "done", Path: "../outside.ts", Bytes: 4})

	New(st).cleanupRetention(store.Settings{StorageDir: storage, RetentionD: 7})

	if _, err := os.Stat(expired); !os.IsNotExist(err) {
		t.Errorf("expired recording still exists: %v", err)
	}
	if _, err := os.Stat(expired + ".json"); !os.IsNotExist(err) {
		t.Errorf("expired sidecar still exists: %v", err)
	}
	if _, err := os.Stat(pending); err != nil {
		t.Errorf("pending recording was deleted: %v", err)
	}
	if _, err := os.Stat(recentFile); err != nil {
		t.Errorf("recent recording was deleted: %v", err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Errorf("file outside storage was deleted: %v", err)
	}
	if segs, _ := st.Segments(store.SegFilter{Ch: 1}); len(segs) != 1 || segs[0].Status != "skipped" {
		t.Errorf("expired segment = %+v, want status skipped", segs)
	}
	if segs, _ := st.Segments(store.SegFilter{Ch: 2}); len(segs) != 1 || segs[0].Status != "pending" {
		t.Errorf("pending segment = %+v, want pending", segs)
	}
}

func TestStopCancelsStalledDownload(t *testing.T) {
	now := time.Now().Unix()
	f := newFakeNVRBase(t, [][2]int64{{now - 600, now - 300}})
	// Replace the stream handler behavior: accept and then never answer the
	// authenticated request. Stop must not wait for the 15s handshake
	// deadline.
	stall := make(chan struct{})
	t.Cleanup(func() { close(stall) })
	go func() {
		for {
			conn, err := f.ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				_ = c.SetDeadline(time.Now().Add(10 * time.Second))
				if _, err := readHead(br); err != nil {
					return
				}
				fmt.Fprint(c, "HTTP/1.1 401 Unauthorized\r\nWWW-Authenticate: Digest nonce=\"n\"\r\nContent-Length: 0\r\n\r\n")
				if _, err := readHead(br); err != nil {
					return
				}
				<-stall // hold the response open until the test ends
			}(conn)
		}
	}()

	dataDir := t.TempDir()
	st, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveSettings(store.Settings{
		NVRHost: f.host(), NVRUser: "admin", NVRPass: "secret",
		AdminPort: f.adminPort(), StorageDir: filepath.Join(dataDir, "storage"),
		Channels: []int{1}, BackfillH: 1,
	}); err != nil {
		t.Fatal(err)
	}
	s := New(st)
	if err := s.Start(SyncRequest{}); err != nil {
		t.Fatal(err)
	}
	// Wait until the download worker is in the stalled handshake.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(st.SyncState().Cur, "cam1") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	began := time.Now()
	s.Stop()
	if d := time.Since(began); d > 3*time.Second {
		t.Fatalf("Stop took %s, want quick cancellation", d)
	}
	if s.Running() {
		t.Fatal("Running() = true after Stop")
	}
	segs, _ := st.Segments(store.SegFilter{Ch: 1})
	if len(segs) != 1 || segs[0].Status != "pending" {
		t.Fatalf("cancelled segment = %+v, want pending", segs)
	}
}

func TestStartValidation(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := New(st)
	if err := s.Start(SyncRequest{}); err == nil {
		t.Fatal("Start with empty settings should fail")
	}
	if s.Running() {
		t.Fatal("failed Start left Running() true")
	}
}

func waitDone(t *testing.T, s *Syncer, st *store.Store, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		state := st.SyncState()
		if !state.Running && state.Done >= want && state.Total >= want {
			return
		}
		if !s.Running() && state.Total >= want && state.Done < want {
			// A finished run with failures is a test failure, but report the
			// state instead of looping until the deadline.
			t.Fatalf("sync finished without downloading all segments: %+v", state)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for sync: %+v", st.SyncState())
}

func segKeyString(seg store.Segment) string {
	return fmt.Sprintf("cam%d@%d-%d", seg.Ch, seg.Start, seg.End)
}

func TestLiveSegmentsStayPending(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	now := time.Now().Unix()
	segs := []store.Segment{
		{Ch: 1, Start: now - 3600, End: now - 10, Status: "pending"},  // may still be recording
		{Ch: 1, Start: now - 7200, End: now - 600, Status: "pending"}, // safely closed
	}
	for _, seg := range segs {
		if err := st.UpsertSegment(seg); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}

	s := New(st)
	queue := s.collectQueue([]int{1}, 0, 0)
	if len(queue) != 1 {
		t.Fatalf("expected only the closed segment in queue, got %d: %+v", len(queue), queue)
	}
	if queue[0].End != now-600 {
		t.Errorf("wrong segment queued: %+v", queue[0])
	}
	if !isLive(segs[0], now) || isLive(segs[1], now) {
		t.Errorf("isLive classified segments incorrectly")
	}
}

func TestSyncCutoff(t *testing.T) {
	now := time.Date(2026, 10, 21, 14, 30, 0, 0, time.Local).Unix()
	midnight := time.Date(2026, 10, 21, 0, 0, 0, 0, time.Local).Unix()

	cases := []struct {
		name string
		set  store.Settings
		want int64
	}{
		{"latest", store.Settings{}, now - liveGuard},
		{"yesterday", store.Settings{Cutoff: "yesterday"}, midnight},
		{"two days", store.Settings{Cutoff: "days", CutoffDays: 2}, midnight - 2*86400},
		{"days fallback", store.Settings{Cutoff: "days", CutoffDays: 0}, midnight - 86400},
		{"unknown", store.Settings{Cutoff: "bogus"}, now - liveGuard},
	}
	for _, tc := range cases {
		if got := syncCutoff(tc.set, now); got != tc.want {
			t.Errorf("%s: syncCutoff = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestCutoffExcludesPendingSegments(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := st.SaveSettings(store.Settings{Cutoff: "yesterday"}); err != nil {
		t.Fatalf("save settings: %v", err)
	}
	now := time.Now().In(time.Local)
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local).Unix()

	today := store.Segment{Ch: 1, Start: midnight + 3600, End: midnight + 3660, Status: "pending"}
	yesterday := store.Segment{Ch: 1, Start: midnight - 7200, End: midnight - 3600, Status: "pending"}
	for _, seg := range []store.Segment{today, yesterday} {
		if err := st.UpsertSegment(seg); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}

	queue := New(st).collectQueue([]int{1}, 0, 0)
	if len(queue) != 1 {
		t.Fatalf("queue = %d segments, want 1: %+v", len(queue), queue)
	}
	if queue[0].End != yesterday.End {
		t.Errorf("queued %+v, want only yesterday's segment", queue[0])
	}
}

func TestDropCoveredClips(t *testing.T) {
	segs := []nvr.MediaSeg{
		{Start: 0, End: 3600, EventType: 1},
		{Start: 100, End: 110, EventType: 2},   // inside continuous
		{Start: 4000, End: 4010, EventType: 2}, // outside continuous
		{Start: 0, End: 3600, EventType: 21},   // composite event is kept
	}
	got := dropCoveredClips(segs)
	if len(got) != 3 {
		t.Fatalf("got %d segments, want 3: %+v", len(got), got)
	}
	for _, seg := range got {
		if seg.EventType == 2 && seg.Start == 100 {
			t.Errorf("covered motion clip was not dropped")
		}
	}
}

func TestRefreshCatalogMarksLocalOnly(t *testing.T) {
	now := time.Now().Unix()
	f := newFakeNVR(t, [][2]int64{{now - 3600, now - 3000}})
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := st.SaveSettings(store.Settings{
		NVRHost: f.host(), NVRUser: "admin", NVRPass: "secret", AdminPort: f.adminPort(),
		StorageDir: t.TempDir(), Channels: []int{1}, CatalogDays: 1,
	}); err != nil {
		t.Fatalf("save settings: %v", err)
	}
	gone := store.Segment{Ch: 1, Start: now - 7200, End: now - 7000, Status: "done", Path: "rec/cam1/gone.ts", Bytes: 123}
	if err := st.UpsertSegment(gone); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	res, err := New(st).RefreshCatalog(context.Background())
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if res.Added != 1 || res.BecameLocal != 1 {
		t.Fatalf("result = %+v, want added=1 became_local=1", res)
	}
	segs, _ := st.Segments(store.SegFilter{Ch: 1, Status: "local_only"})
	if len(segs) != 1 || segs[0].End != gone.End {
		t.Fatalf("local_only segments = %+v", segs)
	}
	if segs[0].Path != gone.Path {
		t.Errorf("path = %q, want %q", segs[0].Path, gone.Path)
	}
}

func TestCollectQueueHonoursRange(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	now := time.Now().Unix()
	inRange := store.Segment{Ch: 1, Start: now - 3600, End: now - 3400, Status: "pending"}
	outside := store.Segment{Ch: 1, Start: now - 7200, End: now - 7000, Status: "pending"}
	for _, seg := range []store.Segment{inRange, outside} {
		if err := st.UpsertSegment(seg); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}
	queue := New(st).collectQueue([]int{1}, now-4000, now-3000)
	if len(queue) != 1 || queue[0].Start != inRange.Start {
		t.Fatalf("queue = %+v, want only the in-range segment", queue)
	}
}
