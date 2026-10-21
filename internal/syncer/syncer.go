// Package syncer is the NVR -> local disk sync engine.
//
// A run is deliberately simple and restartable:
//
//  1. scan: for each selected channel ask the NVR for indexed segments since
//     the last successfully synced position (or the backfill window) and
//     upsert them as "pending";
//  2. queue: take every pending/failed segment of the selected channels;
//  3. download: two workers pull segments concurrently (the NVR supports 8
//     sessions, so 2 downloads plus occasional scans stay well below the
//     limit), each into rec/camN/YYYY-MM-DD/HH-MM-SS_HH-MM-SS.ts;
//  4. verify and record: non-empty file, 188-byte TS sync headers, sidecar
//     JSON next to the file, then UpsertSegment{Status:"done"};
//  5. retention: when RetentionD > 0, remove expired files that are both
//     "done" and inside StorageDir.
//
// Failures are retried twice with exponential backoff and then recorded as
// "failed" with the error message. Progress is published through
// store.SetSyncState; every scan and download is also written to the store
// log.
package syncer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/myml/nvrsync/internal/nvr"
	"github.com/myml/nvrsync/internal/store"
)

const (
	// downloadWorkers stays below the device's 8-session limit while still
	// hiding per-request latency.
	downloadWorkers = 2
	// maxAttempts includes the first try: two retries after a failure.
	maxAttempts = 3
	retryBase   = 2 * time.Second
	// autoPoll is how often AutoLoop re-reads settings, so changing
	// AutoMinutes takes effect without restarting the process.
	autoPoll = 15 * time.Second
	// defaultBackfillH is used when Settings.BackfillH is not positive, so
	// the first sync is not a no-op.
	defaultBackfillH = 24
	pageSize         = 5000
	// liveGuard leaves the newest recordings alone for a couple of minutes:
	// a segment that ends too close to "now" may still be recording, and
	// downloading it now would persist a truncated file whose tail the
	// watermark then skips forever.
	liveGuard = 120 // seconds
)

// ErrAlreadyRunning is returned by Start when a sync is already in flight.
var ErrAlreadyRunning = errors.New("syncer: sync already running")

// SyncRequest selects what to sync.
//
// Channels empty means Settings.Channels. From/To are unix seconds; From=0
// means "from the per-channel synced watermark" (or now-BackfillH on the
// first run) and To=0 means "now". Force re-downloads segments the NVR
// returns even if they are already done.
type SyncRequest struct {
	Channels []int `json:"channels"`
	From     int64 `json:"from"`
	To       int64 `json:"to"`
	Force    bool  `json:"force"`
}

// Syncer owns the sync lifecycle. It is safe for concurrent use.
type Syncer struct {
	st *store.Store

	mu      sync.Mutex
	running bool
	cancel  context.CancelFunc
	done    chan struct{}
}

// New returns a Syncer backed by st.
func New(st *store.Store) *Syncer { return &Syncer{st: st} }

// Start validates the request, marks the sync as running and returns
// immediately; the work happens in a background goroutine. Progress is
// available through store.SyncState.
func (s *Syncer) Start(req SyncRequest) error {
	if s == nil || s.st == nil {
		return errors.New("syncer: nil store")
	}
	settings := s.st.Settings()
	channels := req.Channels
	if len(channels) == 0 {
		channels = settings.Channels
	}
	channels = cleanChannels(channels)
	if len(channels) == 0 {
		return errors.New("syncer: no channels configured")
	}
	if strings.TrimSpace(settings.NVRHost) == "" {
		return errors.New("syncer: NVR host is not configured")
	}
	if strings.TrimSpace(settings.StorageDir) == "" {
		return errors.New("syncer: storage directory is not configured")
	}
	if req.From > 0 && req.To > 0 && req.To <= req.From {
		return fmt.Errorf("syncer: invalid range %d..%d", req.From, req.To)
	}

	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return ErrAlreadyRunning
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s.running = true
	s.cancel = cancel
	s.done = done
	s.mu.Unlock()

	// Publish "running" before returning so /api/sync/status never shows a
	// stale idle state after a successful start.
	s.st.SetSyncState(store.SyncState{Running: true, StartedAt: time.Now().Unix()})

	req.Channels = channels
	go s.run(ctx, req, done)
	return nil
}

// Stop requests cancellation and waits for the worker to exit. In-flight
// downloads are cancelled through DownloadContext, so this normally returns
// quickly.
func (s *Syncer) Stop() {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return
	}
	cancel, done := s.cancel, s.done
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

// Running reports whether a sync goroutine is active.
func (s *Syncer) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// AutoLoop runs incremental syncs every Settings().AutoMinutes until ctx is
// done. It starts the first sync immediately when AutoMinutes > 0, re-reads
// settings periodically (so enabling/disabling takes effect without a
// restart), and never starts a second run while one is in flight.
func (s *Syncer) AutoLoop(ctx context.Context) {
	var last time.Time
	for {
		settings := s.st.Settings()
		if settings.AutoMinutes <= 0 {
			last = time.Time{}
		} else {
			interval := time.Duration(settings.AutoMinutes) * time.Minute
			if last.IsZero() || time.Since(last) >= interval {
				if !s.Running() {
					err := s.Start(SyncRequest{})
					switch {
					case err == nil:
						last = time.Now()
						last = last.Truncate(time.Second)
						s.st.Log("info", "scan", fmt.Sprintf("自动同步已启动（间隔 %d 分钟）", settings.AutoMinutes))
					case errors.Is(err, ErrAlreadyRunning):
						// another run is finishing; retry on the next poll
					default:
						s.st.Log("error", "scan", "自动同步启动失败: "+err.Error())
					}
				}
			}
		}

		wait := autoPoll
		if settings.AutoMinutes > 0 && !last.IsZero() {
			remain := time.Duration(settings.AutoMinutes)*time.Minute - time.Since(last)
			if remain < time.Second {
				remain = time.Second
			}
			if remain < wait {
				wait = remain
			}
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// ---------------------------------------------------------------------------
// Run
// ---------------------------------------------------------------------------

type progress struct {
	mu      sync.Mutex
	started int64
	cur     string
	done    int
	total   int
	bytes   int64
	lastErr string
}

func (p *progress) state(running bool) store.SyncState {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := store.SyncState{
		Running:   running,
		Cur:       p.cur,
		Done:      p.done,
		Total:     p.total,
		Bytes:     p.bytes,
		StartedAt: p.started,
		LastError: p.lastErr,
	}
	if elapsed := time.Since(time.Unix(p.started, 0)).Seconds(); elapsed > 0 && p.bytes > 0 {
		st.Speed = float64(p.bytes) / elapsed / 1e6 // MB/s
	}
	return st
}

func (p *progress) setCur(cur string) {
	p.mu.Lock()
	p.cur = cur
	p.mu.Unlock()
}

func (p *progress) setTotal(total int) {
	p.mu.Lock()
	p.total = total
	p.mu.Unlock()
}

func (p *progress) setError(err error) {
	if err == nil {
		return
	}
	p.mu.Lock()
	p.lastErr = err.Error()
	p.mu.Unlock()
}

func (p *progress) addDone(bytes int64) {
	p.mu.Lock()
	p.done++
	p.bytes += bytes
	p.mu.Unlock()
}

func (s *Syncer) run(ctx context.Context, req SyncRequest, done chan struct{}) {
	started := time.Now()
	settings := s.st.Settings()
	p := &progress{started: started.Unix()}
	p.total = 0

	defer func() {
		final := p.state(false)
		s.st.SetSyncState(final)
		s.st.Log("info", "scan", fmt.Sprintf("同步结束：完成 %d/%d 段，共 %.1f MiB，用时 %s",
			final.Done, final.Total, float64(final.Bytes)/(1<<20),
			time.Since(started).Round(time.Second)))
		s.mu.Lock()
		s.running = false
		s.cancel = nil
		s.done = nil
		s.mu.Unlock()
		// Close last: Stop() waits for every store write to finish.
		close(done)
	}()

	s.publish(p, true)
	scope := channelList(req.Channels)
	s.st.Log("info", "scan", fmt.Sprintf("开始同步：通道 %v，范围 %s", scope, describeRange(req)))

	client := nvr.New(settings.NVRHost, settings.NVRUser, settings.NVRPass, settings.AdminPort)
	if err := client.Login(); err != nil {
		p.setError(err)
		s.st.Log("error", "scan", "NVR 登录失败: "+err.Error())
		return
	}
	s.st.Log("info", "scan", "NVR 登录成功")

	s.scan(ctx, client, settings, req, p)
	if ctx.Err() != nil {
		s.st.Log("warn", "scan", "同步已取消")
		return
	}

	queue := s.collectQueue(req.Channels)
	p.setTotal(len(queue))
	s.publish(p, true)
	if len(queue) == 0 {
		s.st.Log("info", "scan", "没有需要下载的录像段")
	} else {
		s.st.Log("info", "download", fmt.Sprintf("待下载 %d 段，并发 %d", len(queue), downloadWorkers))
		s.downloadAll(ctx, client, settings, queue, p)
	}

	if settings.RetentionD > 0 {
		s.cleanupRetention(settings)
	}
}

func channelList(chs []int) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, ch := range chs {
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "cam%d", ch)
	}
	b.WriteByte(']')
	return b.String()
}

func describeRange(req SyncRequest) string {
	from, to := "自动水位", "现在"
	if req.From > 0 {
		from = fmtTS(req.From)
	}
	if req.To > 0 {
		to = fmtTS(req.To)
	}
	if req.Force {
		return from + " ~ " + to + "（force）"
	}
	return from + " ~ " + to
}

func (s *Syncer) publish(p *progress, running bool) {
	s.st.SetSyncState(p.state(running))
}

// scan lists NVR segments for every channel and upserts them as pending.
func (s *Syncer) scan(ctx context.Context, client *nvr.Client, settings store.Settings, req SyncRequest, p *progress) {
	now := time.Now().Unix()
	to := req.To
	if to <= 0 {
		to = syncCutoff(settings, now)
	}
	backfillH := settings.BackfillH
	if backfillH <= 0 {
		backfillH = defaultBackfillH
	}
	backfill := int64(backfillH) * 3600

	for _, ch := range req.Channels {
		from := req.From
		if from <= 0 {
			if req.Force {
				from = to - backfill
			} else {
				from = s.st.MaxSyncedEnd(ch)
				if from <= 0 {
					from = to - backfill
				}
			}
		}
		if from < 0 {
			from = 0
		}
		if from >= to {
			s.st.Log("info", "scan", fmt.Sprintf("cam%d 已是最新（水位 %s）", ch, fmtTS(from)))
			continue
		}
		s.st.Log("info", "scan", fmt.Sprintf("扫描 cam%d：%s ~ %s", ch, fmtTS(from), fmtTS(to)))

		segs, err := mediaListRange(ctx, client, ch, from, to)
		if err != nil {
			p.setError(err)
			s.st.Log("error", "scan", fmt.Sprintf("cam%d 列出录像失败: %v", ch, err))
			continue
		}
		if len(segs) == 0 {
			s.st.Log("info", "scan", fmt.Sprintf("cam%d 该时段无录像", ch))
			continue
		}
		segs = dropCoveredClips(segs)

		doneKeys := map[segKey]bool{}
		if !req.Force {
			for _, old := range s.listChannel(ch, "done") {
				doneKeys[segKey{old.Ch, old.Start, old.End}] = true
			}
		}

		added := 0
		for _, ms := range segs {
			seg := store.Segment{
				Ch:        ch,
				Start:     ms.Start,
				End:       ms.End,
				Size:      ms.Size,
				EventType: ms.EventType,
				Status:    "pending",
			}
			if seg.End <= seg.Start {
				seg.Status = "skipped"
				seg.Err = "empty time range"
				_ = s.st.UpsertSegment(seg)
				continue
			}
			if !req.Force && doneKeys[segKey{seg.Ch, seg.Start, seg.End}] {
				continue
			}
			if err := s.st.UpsertSegment(seg); err != nil {
				s.st.Log("warn", "scan", fmt.Sprintf("cam%d 记录段 %s 失败: %v", ch, fmtTS(seg.Start), err))
				continue
			}
			added++
		}
		s.st.Log("info", "scan", fmt.Sprintf("cam%d 扫描完成：%d 段，新增/重置 %d 段", ch, len(segs), added))
	}
}

type segKey struct {
	ch    int
	start int64
	end   int64
}

// collectQueue returns every pending/failed segment for the selected
// channels, sorted by start time. Stale "syncing" entries (left behind by a
// crash) are reset to pending and included.
func (s *Syncer) collectQueue(channels []int) []store.Segment {
	seen := map[segKey]bool{}
	var queue []store.Segment
	// Segments left pending from earlier runs must respect the cutoff too,
	// otherwise a manual run could still pull a segment that Settings.Cutoff
	// forbids (and that may still be growing).
	cutoff := syncCutoff(s.st.Settings(), time.Now().Unix())
	for _, ch := range channels {
		for _, status := range []string{"pending", "failed", "syncing"} {
			for _, seg := range s.listChannel(ch, status) {
				k := segKey{seg.Ch, seg.Start, seg.End}
				if seen[k] {
					continue
				}
				seen[k] = true
				if status == "syncing" {
					seg.Status = "pending"
					_ = s.st.UpsertSegment(seg)
				}
				if seg.End <= seg.Start {
					seg.Status = "skipped"
					seg.Err = "empty time range"
					_ = s.st.UpsertSegment(seg)
					continue
				}
				if seg.End > cutoff {
					continue
				}
				if isLive(seg, time.Now().Unix()) {
					// Still recording: keep it pending and retry next round
					// instead of persisting a truncated file.
					s.st.Log("info", "download", fmt.Sprintf("cam%d %s 仍在写入，稍后再同步", seg.Ch, segTime(seg)))
					continue
				}
				queue = append(queue, seg)
			}
		}
	}
	sort.Slice(queue, func(i, j int) bool {
		if queue[i].Start != queue[j].Start {
			return queue[i].Start < queue[j].Start
		}
		if queue[i].Ch != queue[j].Ch {
			return queue[i].Ch < queue[j].Ch
		}
		return queue[i].End < queue[j].End
	})
	return queue
}

func (s *Syncer) listChannel(ch int, status string) []store.Segment {
	var out []store.Segment
	offset := 0
	for {
		batch, total := s.st.Segments(store.SegFilter{Ch: ch, Status: status, Limit: pageSize, Offset: offset})
		out = append(out, batch...)
		offset += len(batch)
		if len(batch) == 0 || offset >= total {
			break
		}
	}
	return out
}

func (s *Syncer) listStatus(status string) []store.Segment {
	var out []store.Segment
	offset := 0
	for {
		batch, total := s.st.Segments(store.SegFilter{Status: status, Limit: pageSize, Offset: offset})
		out = append(out, batch...)
		offset += len(batch)
		if len(batch) == 0 || offset >= total {
			break
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Download
// ---------------------------------------------------------------------------

func (s *Syncer) downloadAll(ctx context.Context, client *nvr.Client, settings store.Settings, queue []store.Segment, p *progress) {
	jobs := make(chan store.Segment)
	var wg sync.WaitGroup
	for i := 0; i < downloadWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for seg := range jobs {
				s.downloadOne(ctx, client, settings.StorageDir, seg, p)
			}
		}()
	}
feed:
	for _, seg := range queue {
		select {
		case <-ctx.Done():
			break feed
		case jobs <- seg:
		}
	}
	close(jobs)
	wg.Wait()
}

func (s *Syncer) downloadOne(ctx context.Context, client *nvr.Client, storageDir string, seg store.Segment, p *progress) {
	rel, abs, err := segmentPaths(storageDir, seg)
	if err != nil {
		s.markFailed(seg, err)
		p.setError(err)
		s.st.Log("error", "download", fmt.Sprintf("cam%d %s: %v", seg.Ch, segTime(seg), err))
		return
	}
	label := fmt.Sprintf("cam%d %s", seg.Ch, segTime(seg))
	p.setCur(label)
	s.publish(p, true)
	s.st.Log("info", "download", fmt.Sprintf("%s 开始（NVR %s，本地 %s）", label, humanBytes(seg.Size), rel))

	// Mark syncing so the UI shows the current work item. A cancelled run
	// resets it to pending below.
	_ = s.st.UpsertSegment(store.Segment{Ch: seg.Ch, Start: seg.Start, End: seg.End, Status: "syncing"})

	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			delay := retryBase << (attempt - 1)
			s.st.Log("warn", "download", fmt.Sprintf("%s 第 %d 次重试（等待 %s）", label, attempt, delay))
			if !sleepCtx(ctx, delay) {
				break
			}
		}
		if ctx.Err() != nil {
			break
		}
		start := time.Now()
		stats, err := s.attemptDownload(ctx, client, storageDir, seg, rel, abs)
		if err == nil {
			took := time.Since(start)
			p.addDone(stats.Bytes)
			p.setCur("")
			s.publish(p, true)
			speed := 0.0
			if secs := took.Seconds(); secs > 0 {
				speed = float64(stats.Bytes) / secs / 1e6
			}
			s.st.Log("info", "download", fmt.Sprintf("%s 完成：%.1f MiB，%s，%.1f MB/s",
				label, float64(stats.Bytes)/(1<<20), took.Round(time.Millisecond), speed))
			return
		}
		if ctx.Err() != nil {
			break
		}
		lastErr = err
		p.setError(err)
		s.st.Log("warn", "download", fmt.Sprintf("%s 下载失败: %v", label, err))
	}

	if ctx.Err() != nil {
		// Leave the segment queued for the next run.
		_ = s.st.UpsertSegment(store.Segment{Ch: seg.Ch, Start: seg.Start, End: seg.End, Status: "pending"})
		p.setCur("")
		s.publish(p, true)
		s.st.Log("warn", "download", label+" 已取消，保留为 pending")
		return
	}

	if lastErr == nil {
		lastErr = errors.New("download failed")
	}
	s.markFailed(seg, lastErr)
	p.setCur("")
	s.publish(p, true)
}

func (s *Syncer) markFailed(seg store.Segment, err error) {
	_ = s.st.UpsertSegment(store.Segment{
		Ch:     seg.Ch,
		Start:  seg.Start,
		End:    seg.End,
		Status: "failed",
		Err:    err.Error(),
	})
}

// attemptDownload performs one download into <path>.part, validates it,
// renames it into place, writes the sidecar and marks the segment done.
func (s *Syncer) attemptDownload(ctx context.Context, client *nvr.Client, storageDir string, seg store.Segment, rel, abs string) (nvr.Stats, error) {
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return nvr.Stats{}, fmt.Errorf("create directory: %w", err)
	}
	tmp := abs + ".part"
	_ = os.Remove(tmp)
	f, err := os.Create(tmp)
	if err != nil {
		return nvr.Stats{}, fmt.Errorf("create temp file: %w", err)
	}
	stats, dlErr := client.DownloadContext(ctx, seg.Ch, seg.Start, seg.End, f, nvr.DownloadOpt{})
	closeErr := f.Close()
	if dlErr == nil && closeErr != nil {
		dlErr = fmt.Errorf("close file: %w", closeErr)
	}
	if dlErr != nil {
		_ = os.Remove(tmp)
		return stats, dlErr
	}
	size, err := validateTSFile(tmp)
	if err != nil {
		_ = os.Remove(tmp)
		return stats, err
	}
	if err := os.Rename(tmp, abs); err != nil {
		_ = os.Remove(tmp)
		return stats, fmt.Errorf("rename into place: %w", err)
	}

	now := time.Now().Unix()
	sc := sidecar{
		Segment: store.Segment{
			Ch:        seg.Ch,
			Start:     seg.Start,
			End:       seg.End,
			Size:      seg.Size,
			EventType: seg.EventType,
			Status:    "done",
			SyncedAt:  now,
			Path:      rel,
			Bytes:     size,
		},
		Stats:   stats,
		SavedAt: now,
	}
	if err := writeSidecar(abs+".json", sc); err != nil {
		_ = os.Remove(abs)
		_ = os.Remove(abs + ".json")
		return stats, err
	}
	if err := s.st.UpsertSegment(sc.Segment); err != nil {
		// The file is valid; keeping it lets the next scan reuse the bytes
		// only through a fresh download. Surface the metadata error so the
		// segment is retried and the store is eventually consistent.
		return stats, fmt.Errorf("record segment: %w", err)
	}
	return stats, nil
}

// sidecar is written next to each finished .ts file.
type sidecar struct {
	store.Segment
	Stats   nvr.Stats `json:"stats"`
	SavedAt int64     `json:"saved_at"`
}

func writeSidecar(path string, sc sidecar) error {
	raw, err := json.MarshalIndent(sc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode sidecar: %w", err)
	}
	raw = append(raw, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return fmt.Errorf("write sidecar: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename sidecar: %w", err)
	}
	return nil
}

// validateTSFile checks the local file: non-empty, a whole number of
// 188-byte packets and 0x47 at every sampled packet start.
func validateTSFile(path string) (int64, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, fmt.Errorf("stat download: %w", err)
	}
	size := fi.Size()
	if size <= 0 {
		return 0, errors.New("downloaded file is empty")
	}
	if size%188 != 0 {
		return 0, fmt.Errorf("downloaded file is not TS-aligned (%d bytes)", size)
	}
	const sample = 188 * 64
	buf := make([]byte, min(int64(sample), size))
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return 0, fmt.Errorf("read download: %w", err)
	}
	for off := 0; off+188 <= n; off += 188 {
		if buf[off] != 0x47 {
			return 0, fmt.Errorf("missing TS sync byte at offset %d", off)
		}
	}
	return size, nil
}

// segmentPaths returns the relative (slash-separated) and absolute paths for
// a segment: rec/camN/YYYY-MM-DD/HH-MM-SS_HH-MM-SS.ts in local time.
func segmentPaths(storageDir string, seg store.Segment) (rel, abs string, err error) {
	root, err := filepath.Abs(storageDir)
	if err != nil {
		return "", "", fmt.Errorf("resolve storage dir: %w", err)
	}
	start := time.Unix(seg.Start, 0).In(time.Local)
	end := time.Unix(seg.End, 0).In(time.Local)
	name := fmt.Sprintf("%s_%s.ts", start.Format("15-04-05"), end.Format("15-04-05"))
	rel = filepath.ToSlash(filepath.Join("rec", fmt.Sprintf("cam%d", seg.Ch), start.Format("2006-01-02"), name))
	abs = filepath.Join(root, filepath.FromSlash(rel))
	return rel, abs, nil
}

// ---------------------------------------------------------------------------
// Retention
// ---------------------------------------------------------------------------

func (s *Syncer) cleanupRetention(settings store.Settings) {
	if settings.RetentionD <= 0 {
		return
	}
	cutoff := time.Now().Unix() - int64(settings.RetentionD)*86400
	removed := 0
	for _, seg := range s.listStatus("done") {
		if seg.Start >= cutoff || seg.Path == "" {
			continue
		}
		abs, ok := pathUnderStorage(settings.StorageDir, seg.Path)
		if !ok {
			s.st.Log("warn", "scan", fmt.Sprintf("保留清理跳过越界路径: %q", seg.Path))
			continue
		}
		if err := os.Remove(abs); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.st.Log("warn", "scan", fmt.Sprintf("删除过期文件 %s 失败: %v", seg.Path, err))
			continue
		}
		if err := os.Remove(abs + ".json"); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.st.Log("warn", "scan", fmt.Sprintf("删除 sidecar %s.json 失败: %v", seg.Path, err))
		}
		if err := s.st.UpsertSegment(store.Segment{
			Ch: seg.Ch, Start: seg.Start, End: seg.End,
			Status: "skipped", Err: "retention: expired",
		}); err != nil {
			s.st.Log("warn", "scan", fmt.Sprintf("标记过期段失败: %v", err))
		}
		removed++
	}
	if removed > 0 {
		s.st.Log("info", "scan", fmt.Sprintf("保留清理：删除 %d 个过期文件（%d 天）", removed, settings.RetentionD))
	} else {
		s.st.Log("info", "scan", fmt.Sprintf("保留清理：无过期文件（%d 天）", settings.RetentionD))
	}
}

// pathUnderStorage resolves a relative segment path and confirms it stays
// inside the storage root. Retention only ever deletes such paths.
func pathUnderStorage(storageDir, rel string) (string, bool) {
	root, err := filepath.Abs(storageDir)
	if err != nil {
		return "", false
	}
	p := filepath.Join(root, filepath.FromSlash(rel))
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", false
	}
	r, err := filepath.Rel(root, abs)
	if err != nil || r == "." || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) || filepath.IsAbs(r) {
		return "", false
	}
	return abs, true
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func cleanChannels(in []int) []int {
	seen := make(map[int]bool, len(in))
	out := make([]int, 0, len(in))
	for _, ch := range in {
		if ch <= 0 || seen[ch] {
			continue
		}
		seen[ch] = true
		out = append(out, ch)
	}
	return out
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func fmtTS(unix int64) string {
	if unix <= 0 {
		return "-"
	}
	return time.Unix(unix, 0).In(time.Local).Format("2006-01-02 15:04:05")
}

func segTime(seg store.Segment) string {
	return time.Unix(seg.Start, 0).In(time.Local).Format("01-02 15:04:05") + " - " +
		time.Unix(seg.End, 0).In(time.Local).Format("15:04:05")
}

// syncCutoff returns the newest wall clock time that may be synced according
// to Settings.Cutoff. "latest" keeps the small liveGuard safety window,
// "yesterday" stops at 00:00 of today (so only closed days are fetched) and
// "days" stops N days back at midnight.
func syncCutoff(settings store.Settings, now int64) int64 {
	latest := now - liveGuard
	local := time.Unix(now, 0).In(time.Local)
	midnight := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.Local).Unix()
	switch strings.TrimSpace(settings.Cutoff) {
	case "yesterday":
		return midnight
	case "days":
		days := settings.CutoffDays
		if days < 1 {
			days = 1
		}
		return midnight - int64(days)*86400
	default:
		return latest
	}
}

// isLive reports whether a segment may still be recording, i.e. its end is
// within liveGuard seconds of now. Such segments are left for a later round so
// a partially written file is never stored as complete.
func isLive(seg store.Segment, now int64) bool {
	return seg.End > now-liveGuard
}

func humanBytes(n int64) string {
	if n <= 0 {
		return "0 B"
	}
	if n < 1<<20 {
		return fmt.Sprintf("%.0f KiB", float64(n)/(1<<10))
	}
	if n < 1<<30 {
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%.2f GiB", float64(n)/(1<<30))
}

// ScanOnly lists NVR recordings for the given range and records them in the
// store without downloading anything. Segments newer than the configured
// cutoff are stored as "skipped" so they neither queue nor appear as pending;
// they can be listed again once the cutoff has moved past them.
func (s *Syncer) ScanOnly(ctx context.Context, channels []int, from, to int64) (listed, added int, err error) {
	settings := s.st.Settings()
	if strings.TrimSpace(settings.NVRHost) == "" {
		return 0, 0, errors.New("nvr_host 未配置")
	}
	if len(channels) == 0 {
		channels = settings.Channels
	}
	if len(channels) == 0 {
		channels = []int{1}
	}
	now := time.Now()
	if to <= 0 {
		to = now.Unix()
	}
	if from <= 0 {
		from = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local).Unix()
	}
	if from >= to {
		return 0, 0, errors.New("from 必须早于 to")
	}
	cutoff := syncCutoff(settings, now.Unix())

	client := nvr.New(settings.NVRHost, settings.NVRUser, settings.NVRPass, settings.AdminPort)
	if err := client.Login(); err != nil {
		return 0, 0, fmt.Errorf("登录 NVR 失败: %w", err)
	}

	for _, ch := range channels {
		segs, err := mediaListRange(ctx, client, ch, from, to)
		if err != nil {
			return listed, added, fmt.Errorf("cam%d 列出录像失败: %w", ch, err)
		}
		segs = dropCoveredClips(segs)
		known := map[segKey]bool{}
		for _, status := range []string{"pending", "syncing", "done", "failed", "skipped"} {
			for _, old := range s.listChannel(ch, status) {
				known[segKey{old.Ch, old.Start, old.End}] = true
			}
		}
		for _, ms := range segs {
			listed++
			key := segKey{ch, ms.Start, ms.End}
			if known[key] {
				continue
			}
			seg := store.Segment{Ch: ch, Start: ms.Start, End: ms.End, Size: ms.Size, EventType: ms.EventType}
			switch {
			case seg.End <= seg.Start:
				seg.Status = "skipped"
				seg.Err = "empty time range"
			case seg.End > cutoff:
				seg.Status = "skipped"
				seg.Err = "晚于同步截止时间，暂不同步"
			default:
				seg.Status = "pending"
			}
			if err := s.st.UpsertSegment(seg); err != nil {
				continue
			}
			known[key] = true
			added++
		}
	}
	s.st.Log("info", "scan", fmt.Sprintf("列出 NVR 录像 %s ~ %s：%d 段，新增 %d 段",
		fmtTS(from), fmtTS(to), listed, added))
	return listed, added, nil
}

// dropCoveredClips removes motion clips (event type 2) that lie entirely inside
// a continuous recording (event type 1) returned for the same range. Keeping
// them would double both the download volume and the timeline entries.
func dropCoveredClips(segs []nvr.MediaSeg) []nvr.MediaSeg {
	continuous := make([]nvr.MediaSeg, 0, len(segs))
	for _, s := range segs {
		if s.EventType == 1 {
			continuous = append(continuous, s)
		}
	}
	if len(continuous) == 0 {
		return segs
	}
	out := segs[:0]
	for _, s := range segs {
		if s.EventType == 2 {
			covered := false
			for _, c := range continuous {
				if c.Start <= s.Start && c.End >= s.End {
					covered = true
					break
				}
			}
			if covered {
				continue
			}
		}
		out = append(out, s)
	}
	return out
}

// mediaListRange lists recordings for an arbitrary range by querying the NVR
// one local day at a time: a single wide query silently under-reports on this
// firmware once it spans several days.
func mediaListRange(ctx context.Context, client *nvr.Client, ch int, from, to int64) ([]nvr.MediaSeg, error) {
	var out []nvr.MediaSeg
	for start := from; start < to; {
		day := time.Unix(start, 0).In(time.Local)
		next := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.Local).AddDate(0, 0, 1).Unix()
		if next <= start {
			next = start + 86400
		}
		if next > to {
			next = to
		}
		segs, err := client.MediaListContext(ctx, ch, start, next)
		switch {
		case errors.Is(err, nvr.ErrNoMedia):
			// no recording in this slice
		case err != nil:
			return out, err
		default:
			out = append(out, segs...)
		}
		start = next
	}
	return out, nil
}
