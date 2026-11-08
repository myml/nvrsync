package syncer

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/myml/nvrsync/internal/nvr"
	"github.com/myml/nvrsync/internal/store"
)

// ---------------------------------------------------------------------------
// Run
// ---------------------------------------------------------------------------

type progress struct {
	mu      sync.Mutex
	started int64
	cur     string
	phase   string
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
		Phase:     p.phase,
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

// setPhase records what the run is doing before its queue exists, so the UI
// can distinguish "reading the index" from "waiting for the NVR session".
func (p *progress) setPhase(phase string) {
	p.mu.Lock()
	p.phase = phase
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

	p.setPhase(store.PhaseQueue)
	s.publish(p, true)
	scope := channelList(req.Channels)
	s.st.Log("info", "scan", fmt.Sprintf("开始同步：通道 %v，范围 %s", scope, describeRange(req)))

	// Build the queue before touching the NVR: it only reads the local index,
	// and an empty queue means there is no reason to take the session at all.
	queue := s.collectQueue(req.Channels, req.From, req.To)
	p.setTotal(len(queue))
	p.setPhase("")
	s.publish(p, true)
	if len(queue) == 0 {
		s.st.Log("info", "scan", "没有需要下载的录像段")
		if settings.RetentionD > 0 {
			s.cleanupRetention(settings)
		}
		return
	}

	p.setPhase(store.PhaseWaitingNVR)
	s.publish(p, true)
	release, err := s.acquireNVR(ctx, "sync")
	if err != nil {
		p.setError(err)
		s.st.Log("warn", "scan", "同步在等待 NVR 会话时被取消")
		return
	}
	defer release()
	p.setPhase("")
	s.publish(p, true)

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

	if free, ok := freeBytes(settings.StorageDir); ok && free < minFreeBytes {
		err := fmt.Errorf("存储空间不足：仅剩 %s，至少保留 %s", humanBytes(free), humanBytes(minFreeBytes))
		p.setError(err)
		s.st.Log("error", "scan", err.Error())
	} else {
		workers := downloadWorkers(settings)
		s.st.Log("info", "download", fmt.Sprintf("待下载 %d 段，并发 %d", len(queue), workers))
		s.downloadAll(ctx, client, settings, queue, workers, p)
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
	now := time.Now().Unix()
	cutoff := syncCutoff(s.st.Settings(), now)
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
		if isLive(seg, now) {
			// Still recording: keep it pending and retry next round
			// instead of persisting a truncated file.
			s.st.LogCh(seg.Ch, "info", "download", fmt.Sprintf("%s 仍在写入，稍后再同步", segTime(seg)))
			continue
		}
		if status == "failed" {
			// A repeated failure backs off for a growing cooldown, so a
			// large failed backlog no longer crowds out fresh pending work
			// on every single run.
			if !failedRetryDue(seg, now) {
				continue
			}
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
