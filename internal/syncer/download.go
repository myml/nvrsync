package syncer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/myml/nvrsync/internal/metrics"
	"github.com/myml/nvrsync/internal/nvr"
	"github.com/myml/nvrsync/internal/store"
)

// ---------------------------------------------------------------------------
// Download
// ---------------------------------------------------------------------------

func (s *Syncer) downloadAll(ctx context.Context, client *nvr.Client, settings store.Settings, queue []store.Segment, workers int, p *progress) {
	if workers < 1 {
		workers = 1
	}
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
	for i := 0; i < workers; i++ {
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
			_ = s.st.UpdateSegmentIfPresent(store.Segment{
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
	// resets it to pending below. From here on the run only updates its own
	// row: if retention removed it (the file it describes is gone), the
	// download is abandoned instead of writing a file nobody indexed again.
	if err := s.st.UpdateSegmentIfPresent(store.Segment{Ch: seg.Ch, Start: seg.Start, End: seg.End, Status: "syncing"}); err != nil {
		metrics.Default().IncSegment(metrics.StatusSkipped)
		s.st.LogCh(seg.Ch, "warn", "download", fmt.Sprintf("%s 索引已被移除，跳过下载: %v", label, err))
		return
	}

	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			delay := retryBase << (attempt - 1)
			if nvr.IsBusy(lastErr) {
				delay = busyRetryBase << (attempt - 1)
			} else if nvr.IsTransient(lastErr) {
				delay = transientRetryBase << (attempt - 1)
			}
			s.st.LogCh(seg.Ch, "warn", "download", fmt.Sprintf("%s 第 %d 次重试（等待 %s）", label, attempt, delay))
			if !retrySleep(ctx, delay) {
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
			metrics.Default().IncSegment(metrics.StatusDone)
			metrics.Default().AddBytes(stats.Bytes)
			metrics.Default().ObserveDownload(took)
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
		if nvr.IsBusy(err) {
			metrics.Default().IncNVRBusy()
		}
		p.setError(err)
		s.st.LogCh(seg.Ch, "warn", "download", fmt.Sprintf("%s 下载失败: %v", label, err))
	}

	if ctx.Err() != nil {
		// Leave the segment queued for the next run.
		_ = s.st.UpdateSegmentIfPresent(store.Segment{Ch: seg.Ch, Start: seg.Start, End: seg.End, Status: "pending"})
		metrics.Default().IncSegment(metrics.StatusPending)
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
		_ = s.st.UpdateSegmentIfPresent(store.Segment{
			Ch: seg.Ch, Start: seg.Start, End: seg.End,
			Status: "pending", Err: lastErr.Error(),
		})
		metrics.Default().IncSegment(metrics.StatusPending)
		s.st.LogCh(seg.Ch, "warn", "download", label+" NVR 繁忙（-40401），保留为 pending")
		if busyStreak.Add(1) >= maxBusyStreak {
			trip()
		}
		p.setCur("")
		s.publish(p, true)
		return
	}
	busyStreak.Store(0)
	if errors.Is(lastErr, store.ErrSegmentGone) {
		// Retention removed the row while this download was running; the
		// partial file was already cleaned up by attemptDownload.
		metrics.Default().IncSegment(metrics.StatusSkipped)
		s.st.LogCh(seg.Ch, "warn", "download", label+" 索引已被移除，放弃该段")
		p.setCur("")
		s.publish(p, true)
		return
	}
	if errors.Is(lastErr, nvr.ErrNoMedia) {
		_ = s.st.UpdateSegmentIfPresent(store.Segment{
			Ch: seg.Ch, Start: seg.Start, End: seg.End,
			Status: "skipped", Err: "NVR 上已无此段",
		})
		metrics.Default().IncSegment(metrics.StatusSkipped)
		s.st.LogCh(seg.Ch, "warn", "download", label+" NVR 上已无此段，标记 skipped")
		p.setCur("")
		s.publish(p, true)
		return
	}
	if nvr.IsTransient(lastErr) {
		// A truncated stream, an idle timeout or a broken handshake is a
		// device/network hiccup, not a verdict on the segment. Keep it
		// pending so the next run picks it up, instead of filling the index
		// with permanent-looking failures that would also be counted as
		// "failed" in the UI.
		_ = s.st.UpdateSegmentIfPresent(store.Segment{
			Ch: seg.Ch, Start: seg.Start, End: seg.End,
			Status: "pending", Err: lastErr.Error(),
		})
		metrics.Default().IncSegment(metrics.StatusPending)
		s.st.LogCh(seg.Ch, "warn", "download", fmt.Sprintf("%s 暂时失败，保留为 pending: %v", label, lastErr))
		p.setCur("")
		s.publish(p, true)
		return
	}
	s.markFailed(seg, lastErr)
	p.setCur("")
	s.publish(p, true)
}

func (s *Syncer) markFailed(seg store.Segment, err error) {
	metrics.Default().IncSegment(metrics.StatusFailed)
	_ = s.st.UpdateSegmentIfPresent(store.Segment{
		Ch:       seg.Ch,
		Start:    seg.Start,
		End:      seg.End,
		Status:   "failed",
		SyncedAt: time.Now().Unix(),
		Err:      failureRecord{failures: failedAttempts(seg.Err) + 1, msg: err.Error()}.String(),
	})
}

// failureRecord is the shape stored in Segment.Err for a "failed" row: a
// machine-readable attempt counter followed by the human-readable reason, e.g.
//
//	"failures=3: nvr: download: ..."
//
// The counter is what drives the per-segment retry backoff; wrapping it in the
// message keeps the error column self-describing for the UI and for anyone
// reading the SQLite file by hand.
type failureRecord struct {
	failures int
	msg      string
}

func (f failureRecord) String() string {
	return fmt.Sprintf("%s%d: %s", failurePrefix, f.failures, f.msg)
}

// failurePrefix prefixes the attempt counter in a failed row's Err.
const failurePrefix = "failures="

// failedAttempts extracts the attempt counter from a stored failure message.
// Rows written before the counter existed parse as 0.
func failedAttempts(errMsg string) int {
	if !strings.HasPrefix(errMsg, failurePrefix) {
		return 0
	}
	rest := errMsg[len(failurePrefix):]
	colon := strings.IndexByte(rest, ':')
	if colon <= 0 {
		return 0
	}
	n, err := strconv.Atoi(rest[:colon])
	if err != nil || n < 0 {
		return 0
	}
	return n
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
	if err := s.st.UpdateSegmentIfPresent(sc.Segment); err != nil {
		// The file is valid; the next scan can only reuse the bytes through a
		// fresh download. Surface the metadata error so the segment is retried
		// and the store is eventually consistent. A row retention removed is
		// not retried: the file it describes was just deleted.
		if errors.Is(err, store.ErrSegmentGone) {
			_ = os.Remove(abs)
			_ = os.Remove(abs + ".json")
		}
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
