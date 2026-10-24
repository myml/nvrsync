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
	"sync/atomic"
	"syscall"
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
	// busyRetryBase is used for the NVR's -40401 ("no session budget") reply.
	// It is transient; hammering the device with 2s retries makes it worse.
	busyRetryBase = 10 * time.Second
	// maxBusyStreak stops one run after this many consecutive busy failures,
	// so a saturated NVR does not turn the whole queue into retries.
	maxBusyStreak = 8
	// maxFailedPerRun bounds automatic retries of the permanent-looking
	// backlog. Pending segments are always processed first.
	maxFailedPerRun = 200
	// minFreeBytes keeps the last gigabyte of the volume free for SQLite,
	// sidecars and the OS; a 1 GB NVR segment can otherwise fill the disk.
	minFreeBytes = 1 << 30
	// autoPoll is how often AutoLoop re-reads settings, so changing
	// AutoMinutes takes effect without restarting the process.
	autoPoll = 15 * time.Second
	// liveGuard leaves the newest recordings alone for a couple of minutes:
	// a segment that ends too close to "now" may still be recording, and
	// downloading it now would persist a truncated file whose tail the
	// watermark then skips forever.
	liveGuard = 120 // seconds
)

// ErrAlreadyRunning is returned by Start when a sync is already in flight.
var ErrAlreadyRunning = errors.New("syncer: sync already running")

// ErrCatalogBusy is returned when a catalogue refresh is already running.
var ErrCatalogBusy = errors.New("syncer: catalogue refresh already running")

// catalogCooldown coalesces bursts of refresh requests (page loads, multiple
// tabs, the background loop) into at most one NVR scan per interval.
const catalogCooldown = 60 * time.Second

// SyncRequest selects which channels and which time range a sync drains from
// the local index. Zero From/To means "everything pending".
//
// Channels empty means Settings.Channels. From/To are unix seconds filtering
// which segments start inside the range; zero means "no bound".
type SyncRequest struct {
	Channels []int `json:"channels"`
	From     int64 `json:"from"`
	To       int64 `json:"to"`
}

// Syncer owns the sync lifecycle. It is safe for concurrent use.
type Syncer struct {
	st *store.Store

	mu      sync.Mutex
	running bool
	cancel  context.CancelFunc
	done    chan struct{}

	// Catalogue refreshes are expensive (one NVR login + many media list
	// queries), so concurrent triggers are coalesced and repeats inside
	// catalogCooldown reuse the previous result.
	catalogMu   sync.Mutex
	catalogAt   time.Time
	catalogLast CatalogResult

	// Stale .part files are removed once per process, not on every run.
	cleanupOnce sync.Once
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
	var retryAt time.Time
	var failures int
	for {
		settings := s.st.Settings()
		if settings.AutoMinutes <= 0 {
			last = time.Time{}
			retryAt = time.Time{}
			failures = 0
		} else {
			interval := time.Duration(settings.AutoMinutes) * time.Minute
			if (last.IsZero() || time.Since(last) >= interval) &&
				!time.Now().Before(retryAt) && !s.Running() {
				err := s.Start(SyncRequest{})
				switch {
				case err == nil:
					failures = 0
					retryAt = time.Time{}
					last = time.Now().Truncate(time.Second)
					s.st.Log("info", "scan", fmt.Sprintf("自动同步已启动（间隔 %d 分钟）", settings.AutoMinutes))
				case errors.Is(err, ErrAlreadyRunning):
					// another run is finishing; retry on the next poll
				default:
					// A misconfigured NVR must not produce a log line every
					// 15 seconds forever. Back off up to 10 minutes.
					failures++
					backoff := 15 * time.Second
					for i := 1; i < failures && i < 6; i++ {
						backoff *= 2
					}
					if backoff > 10*time.Minute {
						backoff = 10 * time.Minute
					}
					retryAt = time.Now().Add(backoff)
					if failures == 1 || failures%10 == 0 {
						s.st.Log("error", "scan", fmt.Sprintf("自动同步启动失败（第 %d 次，%s 后重试）: %v",
							failures, backoff.Round(time.Second), err))
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
		if !retryAt.IsZero() {
			remain := time.Until(retryAt)
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
	s.cleanupOnce.Do(func() { s.cleanupStaleParts(settings.StorageDir) })
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

	// Discovery belongs to the catalogue loop; a sync only drains what the
	// local index already knows about.
	s.st.Log("info", "scan", "使用本地索引（对账由后台按设置执行）")

	queue := s.collectQueue(req.Channels, req.From, req.To)
	p.setTotal(len(queue))
	s.publish(p, true)
	if len(queue) == 0 {
		s.st.Log("info", "scan", "没有需要下载的录像段")
	} else if free, ok := freeBytes(settings.StorageDir); ok && free < minFreeBytes {
		err := fmt.Errorf("存储空间不足：仅剩 %s，至少保留 %s", humanBytes(free), humanBytes(minFreeBytes))
		p.setError(err)
		s.st.Log("error", "scan", err.Error())
	} else {
		s.st.Log("info", "download", fmt.Sprintf("待下载 %d 段，并发 %d", len(queue), downloadWorkers))
		s.downloadAll(ctx, client, settings, queue, p)
	}

	if settings.RetentionD > 0 {
		s.cleanupRetention(settings)
	}
}

func statusRank(status string) int {
	switch status {
	case "pending", "syncing":
		return 0
	case "failed":
		return 1
	default:
		return 2
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
	return from + " ~ " + to
}

func (s *Syncer) publish(p *progress, running bool) {
	s.st.SetSyncState(p.state(running))
}

// segKey identifies a segment row in the index.
type segKey struct {
	Ch         int
	Start, End int64
}

func (s *Syncer) collectQueue(channels []int, from, to int64) []store.Segment {
	seen := map[segKey]bool{}
	var queue []store.Segment
	failedCount := 0
	// Segments left pending from earlier runs must respect the cutoff too,
	// otherwise a manual run could still pull a segment that Settings.Cutoff
	// forbids (and that may still be growing).
	cutoff := syncCutoff(s.st.Settings(), time.Now().Unix())
	for _, seg := range s.st.QueueSegments(channels, []string{"pending", "failed", "syncing"}) {
		status := seg.Status
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
		// A run started with an explicit range only touches segments
		// that *start* inside it. Overlap matching would make a retry
		// of a 10s event clip drag in the whole hour-long continuous
		// recording that contains it.
		if from > 0 && seg.Start < from {
			continue
		}
		if to > 0 && seg.Start >= to {
			continue
		}
		if isLive(seg, time.Now().Unix()) {
			// Still recording: keep it pending and retry next round
			// instead of persisting a truncated file.
			s.st.LogCh(seg.Ch, "info", "download", fmt.Sprintf("%s 仍在写入，稍后再同步", segTime(seg)))
			continue
		}
		if status == "failed" {
			if failedCount >= maxFailedPerRun {
				continue
			}
			failedCount++
		}
		queue = append(queue, seg)
	}
	// Drain fresh pending work before retrying the failed backlog.
	sort.Slice(queue, func(i, j int) bool {
		ri, rj := statusRank(queue[i].Status), statusRank(queue[j].Status)
		if ri != rj {
			return ri < rj
		}
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
	return s.st.SegmentsAll(store.SegFilter{Ch: ch, Status: status})
}

func (s *Syncer) listStatus(status string) []store.Segment {
	return s.st.SegmentsAll(store.SegFilter{Status: status})
}

// ---------------------------------------------------------------------------
// Download
// ---------------------------------------------------------------------------

func (s *Syncer) downloadAll(ctx context.Context, client *nvr.Client, settings store.Settings, queue []store.Segment, p *progress) {
	jobs := make(chan store.Segment)
	var wg sync.WaitGroup
	var busyStreak atomic.Int32
	trip := make(chan struct{})
	var tripOnce sync.Once
	tripFn := func() {
		tripOnce.Do(func() {
			close(trip)
			s.st.Log("warn", "download", "NVR 连续繁忙，本轮提前收尾，剩余段保留到下一轮")
		})
	}
	for i := 0; i < downloadWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for seg := range jobs {
				s.downloadOne(ctx, client, settings.StorageDir, seg, p, &busyStreak, tripFn)
			}
		}()
	}
feed:
	for _, seg := range queue {
		select {
		case <-ctx.Done():
			break feed
		case <-trip:
			break feed
		case jobs <- seg:
		}
	}
	close(jobs)
	wg.Wait()
}

func (s *Syncer) downloadOne(ctx context.Context, client *nvr.Client, storageDir string, seg store.Segment, p *progress, busyStreak *atomic.Int32, trip func()) {
	rel, abs, err := segmentPaths(storageDir, seg)
	if err != nil {
		s.markFailed(seg, err)
		p.setError(err)
		s.st.LogCh(seg.Ch, "error", "download", fmt.Sprintf("%s: %v", segTime(seg), err))
		return
	}
	label := fmt.Sprintf("cam%d %s", seg.Ch, segTime(seg))
	if free, ok := freeBytes(storageDir); ok {
		need := seg.Size
		if need <= 0 {
			need = 256 << 20
		}
		if free < need+minFreeBytes {
			s.st.LogCh(seg.Ch, "error", "download", fmt.Sprintf("%s 磁盘余量不足（剩 %s，需约 %s），保留为 pending",
				label, humanBytes(free), humanBytes(need)))
			_ = s.st.UpsertSegment(store.Segment{
				Ch: seg.Ch, Start: seg.Start, End: seg.End,
				Status: "pending", Err: "disk full",
			})
			return
		}
	}

	p.setCur(label)
	s.publish(p, true)
	s.st.LogCh(seg.Ch, "info", "download", fmt.Sprintf("%s 开始（NVR %s，本地 %s）", label, humanBytes(seg.Size), rel))

	// Mark syncing so the UI shows the current work item. A cancelled run
	// resets it to pending below.
	_ = s.st.UpsertSegment(store.Segment{Ch: seg.Ch, Start: seg.Start, End: seg.End, Status: "syncing"})

	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			delay := retryBase << (attempt - 1)
			if nvr.IsBusy(lastErr) {
				delay = busyRetryBase << (attempt - 1)
			}
			s.st.LogCh(seg.Ch, "warn", "download", fmt.Sprintf("%s 第 %d 次重试（等待 %s）", label, attempt, delay))
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
			busyStreak.Store(0)
			took := time.Since(start)
			p.addDone(stats.Bytes)
			p.setCur("")
			s.publish(p, true)
			speed := 0.0
			if secs := took.Seconds(); secs > 0 {
				speed = float64(stats.Bytes) / secs / 1e6
			}
			s.st.LogCh(seg.Ch, "info", "download", fmt.Sprintf("%s 完成：%.1f MiB，%s，%.1f MB/s",
				label, float64(stats.Bytes)/(1<<20), took.Round(time.Millisecond), speed))
			return
		}
		if ctx.Err() != nil {
			break
		}
		lastErr = err
		p.setError(err)
		s.st.LogCh(seg.Ch, "warn", "download", fmt.Sprintf("%s 下载失败: %v", label, err))
	}

	if ctx.Err() != nil {
		// Leave the segment queued for the next run.
		_ = s.st.UpsertSegment(store.Segment{Ch: seg.Ch, Start: seg.Start, End: seg.End, Status: "pending"})
		p.setCur("")
		s.publish(p, true)
		s.st.LogCh(seg.Ch, "warn", "download", label+" 已取消，保留为 pending")
		return
	}

	if lastErr == nil {
		lastErr = errors.New("download failed")
	}
	if nvr.IsBusy(lastErr) {
		// Leave the segment queued: marking thousands of segments failed just
		// because the NVR ran out of sessions is what filled the index with
		// 3,700 permanent-looking errors.
		_ = s.st.UpsertSegment(store.Segment{
			Ch: seg.Ch, Start: seg.Start, End: seg.End,
			Status: "pending", Err: lastErr.Error(),
		})
		s.st.LogCh(seg.Ch, "warn", "download", label+" NVR 繁忙（-40401），保留为 pending")
		if busyStreak.Add(1) >= maxBusyStreak {
			trip()
		}
		p.setCur("")
		s.publish(p, true)
		return
	}
	busyStreak.Store(0)
	if errors.Is(lastErr, nvr.ErrNoMedia) {
		_ = s.st.UpsertSegment(store.Segment{
			Ch: seg.Ch, Start: seg.Start, End: seg.End,
			Status: "skipped", Err: "NVR 上已无此段",
		})
		s.st.LogCh(seg.Ch, "warn", "download", label+" NVR 上已无此段，标记 skipped")
		p.setCur("")
		s.publish(p, true)
		return
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
	// local_only rows have a local file too; omitting them leaked every file
	// whose segment the NVR had already overwritten.
	for _, status := range []string{"done", "local_only"} {
		for _, seg := range s.listStatus(status) {
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
	}
	if removed > 0 {
		s.st.Log("info", "scan", fmt.Sprintf("保留清理：删除 %d 个过期文件（%d 天）", removed, settings.RetentionD))
	} else {
		s.st.Log("info", "scan", fmt.Sprintf("保留清理：无过期文件（%d 天）", settings.RetentionD))
	}
}

// freeBytes reports the space available to an unprivileged writer.
func freeBytes(dir string) (int64, bool) {
	if strings.TrimSpace(dir) == "" {
		return 0, false
	}
	var fs syscall.Statfs_t
	if err := syscall.Statfs(dir, &fs); err != nil {
		return 0, false
	}
	return int64(fs.Bavail) * int64(fs.Bsize), true
}

// cleanupStaleParts removes interrupted downloads left behind by a crash or a
// container restart. Only files older than an hour are touched, so an active
// download from a concurrent process is never removed.
func (s *Syncer) cleanupStaleParts(storageDir string) {
	root := filepath.Join(strings.TrimSpace(storageDir), "rec")
	if strings.TrimSpace(storageDir) == "" {
		return
	}
	removed := 0
	_ = filepath.Walk(root, func(name string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(name, ".part") && !strings.HasSuffix(name, ".json.tmp") {
			return nil
		}
		if time.Since(info.ModTime()) < time.Hour {
			return nil
		}
		if err := os.Remove(name); err == nil {
			removed++
		}
		return nil
	})
	if removed > 0 {
		s.st.Log("info", "scan", fmt.Sprintf("清理了 %d 个残留的未完成下载文件", removed))
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
		// Use calendar days rather than 86400 seconds so a DST transition
		// does not shift the cutoff by an hour.
		return time.Date(local.Year(), local.Month(), local.Day()-days, 0, 0, 0, 0, time.Local).Unix()
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
//
// Days are walked from newest to oldest and the walk stops after
// mediaListEmptyStop consecutive empty days, so a "look back 365 days" setting
// costs about as many queries as the device actually has footage for instead of
// 365 (× channels) requests.
func mediaListRange(ctx context.Context, client *nvr.Client, ch int, from, to int64) ([]nvr.MediaSeg, error) {
	var out []nvr.MediaSeg
	emptyStreak := 0
	for end := to; end > from; {
		day := time.Unix(end-1, 0).In(time.Local)
		start := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.Local).Unix()
		if start < from {
			start = from
		}
		segs, err := client.MediaListContext(ctx, ch, start, end)
		switch {
		case errors.Is(err, nvr.ErrNoMedia):
			// no recording in this slice
		case err != nil:
			return out, err
		default:
			emptyStreak = 0
			out = append(out, segs...)
		}
		if len(segs) == 0 {
			emptyStreak++
			if emptyStreak >= mediaListEmptyStop {
				return out, nil
			}
		}
		end = start
		// Be gentle with the NVR: a 30-day, 8-channel refresh is ~240
		// requests, and firing them back-to-back makes the device answer
		// -40401 (busy).
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		case <-time.After(mediaListPace):
		}
	}
	return out, nil
}

// mediaListEmptyStop ends the backwards day walk after this many consecutive
// days without recordings.
const mediaListEmptyStop = 7

// mediaListPace is the pause between day-sized media list queries.
const mediaListPace = 150 * time.Millisecond

// catalogPoll is the fallback reconcile interval when the setting is missing.
const catalogPoll = 5 * time.Minute

// defaultCatalogDays is how far back the catalogue refresh looks when the
// setting is not positive.
const defaultCatalogDays = 7

// CatalogLoop periodically re-reads the NVR recording list so the local index
// reflects what the device still holds: new segments appear as pending and
// segments the NVR has already overwritten become "local_only" (file kept) or
// "skipped" (file never downloaded).
func (s *Syncer) CatalogLoop(ctx context.Context) {
	// Small delay so startup is not competing with the first sync.
	select {
	case <-ctx.Done():
		return
	case <-time.After(20 * time.Second):
	}
	for {
		settings := s.st.Settings()
		if settings.CatalogDays > 0 && !s.Running() {
			if _, err := s.RefreshCatalog(ctx); err != nil {
				s.st.Log("error", "catalog", "自动对账失败: "+err.Error())
			}
		}
		poll := catalogPoll
		if settings.CatalogMinutes > 0 {
			poll = time.Duration(settings.CatalogMinutes) * time.Minute
		}
		timer := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// RefreshCatalog lists the NVR recordings for the configured horizon and
// reconciles the local index with them. It never downloads footage.
func (s *Syncer) RefreshCatalog(ctx context.Context) (CatalogResult, error) {
	return s.RefreshCatalogForce(ctx, false)
}

// RefreshCatalogForce is RefreshCatalog with an option to bypass the cooldown.
// It is used by the UI refresh button, which must always hit the NVR.
func (s *Syncer) RefreshCatalogForce(ctx context.Context, force bool) (CatalogResult, error) {
	return s.refreshCatalogDays(ctx, force, false, 0)
}

// refreshCatalogDays is the shared implementation. allowRunning lets a sync use
// it while it is itself marked as running; daysOverride>0 limits the look-back
// window (the sync only refreshes the most recent days instead of the full
// catalogue window).
func (s *Syncer) refreshCatalogDays(ctx context.Context, force, allowRunning bool, daysOverride int) (CatalogResult, error) {
	if !s.catalogMu.TryLock() {
		return CatalogResult{}, ErrCatalogBusy
	}
	defer s.catalogMu.Unlock()
	if !force && time.Since(s.catalogAt) < catalogCooldown {
		return s.catalogLast, nil
	}
	// Record the attempt even if it fails, so a failing NVR is not hammered
	// by page loads either.
	s.catalogAt = time.Now()
	var res CatalogResult
	settings := s.st.Settings()
	if strings.TrimSpace(settings.NVRHost) == "" {
		return res, errors.New("nvr_host 未配置")
	}
	if !allowRunning && s.Running() {
		return res, ErrAlreadyRunning
	}
	channels := settings.CatalogChannels
	if len(channels) == 0 {
		channels = settings.Channels
	}
	if len(channels) == 0 {
		channels = []int{1}
	}
	days := settings.CatalogDays
	if daysOverride > 0 {
		days = daysOverride
	}
	if days <= 0 {
		days = defaultCatalogDays
	}
	now := time.Now()
	from := now.AddDate(0, 0, -days).Unix()
	to := now.Unix()
	cutoff := syncCutoff(settings, now.Unix())

	client := nvr.New(settings.NVRHost, settings.NVRUser, settings.NVRPass, settings.AdminPort)
	if err := client.Login(); err != nil {
		s.st.Log("error", "catalog", "对账登录 NVR 失败: "+err.Error())
		return res, fmt.Errorf("登录 NVR 失败: %w", err)
	}
	s.st.Log("info", "catalog", fmt.Sprintf("对账开始：通道 %v，范围 %s ~ %s（最近 %d 天）",
		channels, fmtTS(from), fmtTS(to), days))

	for _, ch := range channels {
		segs, err := mediaListRange(ctx, client, ch, from, to)
		if err != nil {
			if nvr.IsBusy(err) {
				s.st.LogCh(ch, "warn", "catalog", "NVR 繁忙（-40401），本轮跳过")
				continue
			}
			s.st.LogCh(ch, "error", "catalog", fmt.Sprintf("列出录像失败: %v", err))
			continue
		}
		segs = dropCoveredClips(segs)

		// Keep the status of known rows: a skipped row may become eligible
		// once the cutoff moves, but it must not be counted as "new" on every
		// hourly refresh (which also caused thousands of pointless upserts).
		known := map[segKey]store.Segment{}
		for _, status := range []string{"pending", "syncing", "done", "failed", "skipped"} {
			for _, old := range s.listChannel(ch, status) {
				known[segKey{old.Ch, old.Start, old.End}] = old
			}
		}

		present := make([]nvr.MediaSeg, 0, len(segs))
		for _, ms := range segs {
			present = append(present, ms)
			key := segKey{ch, ms.Start, ms.End}
			if old, ok := known[key]; ok {
				if old.Status == "skipped" && shouldPromoteSkipped(old, ms, cutoff) {
					seg := store.Segment{
						Ch: ch, Start: ms.Start, End: ms.End,
						Size: ms.Size, EventType: ms.EventType, Status: "pending",
					}
					if err := s.st.UpsertSegment(seg); err == nil {
						known[key] = seg
						res.Promoted++
					}
				}
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
			known[key] = seg
			res.Added++
		}

		// Anything in the refreshed window that the NVR no longer returns has
		// either been kept locally (local_only) or was never downloaded.
		for _, status := range []string{"done", "pending", "failed", "syncing", "local_only"} {
			for _, old := range s.listChannel(ch, status) {
				if old.Start < from || old.Start >= to {
					continue // outside the window we actually queried
				}
				if overlapsAny(old, present) {
					continue
				}
				switch {
				case status == "done" || status == "local_only":
					if old.Path == "" {
						continue
					}
					if status == "done" {
						res.BecameLocal++
					}
					_ = s.st.UpsertSegment(store.Segment{
						Ch: old.Ch, Start: old.Start, End: old.End,
						Status: "local_only", Path: old.Path, Bytes: old.Bytes, SyncedAt: old.SyncedAt,
					})
				default:
					_ = s.st.UpsertSegment(store.Segment{
						Ch: old.Ch, Start: old.Start, End: old.End,
						Status: "skipped", Err: "NVR 上已无此段",
					})
					res.MarkedGone++
				}
			}
		}
		res.Listed += len(present)
	}
	if settings.RetentionD > 0 {
		// Retention used to run only after a successful sync, so a deployment
		// with auto-sync disabled never deleted anything.
		s.cleanupRetention(settings)
	}
	s.st.Log("info", "catalog", fmt.Sprintf("对账完成：NVR 上 %d 段，新增 %d，恢复 %d，转为仅本地 %d，标记丢失 %d",
		res.Listed, res.Added, res.Promoted, res.BecameLocal, res.MarkedGone))
	s.catalogLast = res
	return res, nil
}

// shouldPromoteSkipped reports whether an existing skipped row should be
// queued again for download because the NVR still has it and the current
// cutoff now allows it. Rows skipped by retention or with an empty time range
// stay skipped.
func shouldPromoteSkipped(old store.Segment, ms nvr.MediaSeg, cutoff int64) bool {
	if ms.End <= ms.Start || ms.End > cutoff {
		return false
	}
	if strings.HasPrefix(old.Err, "retention:") || old.Err == "empty time range" {
		return false
	}
	return true
}

// CatalogResult summarises one catalogue refresh.
type CatalogResult struct {
	Listed      int `json:"listed"`
	Added       int `json:"added"`
	Promoted    int `json:"promoted"`
	BecameLocal int `json:"became_local"`
	MarkedGone  int `json:"marked_gone"`
}

// overlapsAny reports whether seg is still represented on the NVR list. Exact
// key equality is too strict because the device can re-index a segment with a
// slightly different end once it is closed, so a large overlap also counts.
func overlapsAny(seg store.Segment, list []nvr.MediaSeg) bool {
	length := seg.End - seg.Start
	if length <= 0 {
		return false
	}
	for _, other := range list {
		start := seg.Start
		if other.Start > start {
			start = other.Start
		}
		end := seg.End
		if other.End < end {
			end = other.End
		}
		if end-start >= length*9/10 {
			return true
		}
	}
	return false
}
