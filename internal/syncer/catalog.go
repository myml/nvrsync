package syncer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/myml/nvrsync/internal/metrics"
	"github.com/myml/nvrsync/internal/nvr"
	"github.com/myml/nvrsync/internal/store"
)

// ---------------------------------------------------------------------------
// Catalogue
// ---------------------------------------------------------------------------

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

	// A scan logs in and page-s through the media list; it must not overlap a
	// sync or a catalogue refresh on the same device.
	release, err := s.acquireNVR(ctx, "scan")
	if err != nil {
		return 0, 0, err
	}
	defer release()

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
			// Keep what earlier (newer) days already returned instead of
			// throwing the whole listing away: losing a multi-day listing to
			// one bad day makes the reconcile loop look like it never
			// succeeds. The day walk stops here and the caller reports the
			// error alongside the partial result.
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
		if settings.CatalogDays > 0 {
			// Do not even try while a sync owns the device: RefreshCatalog
			// would only queue up behind it and then log a "failed" line.
			if !s.Running() && s.SessionHolder() == "" {
				if _, err := s.RefreshCatalog(ctx); err != nil {
					s.st.Log("error", "catalog", "自动对账失败: "+err.Error())
				}
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

	// The catalogue reads the device; it must not run on top of a sync (or of
	// another scan). Wait for the session instead of hammering the NVR.
	release, err := s.acquireNVR(ctx, "catalog")
	if err != nil {
		return res, err
	}
	defer release()

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
				metrics.Default().IncNVRBusy()
				s.st.LogCh(ch, "warn", "catalog", "NVR 繁忙（-40401），本轮跳过")
				continue
			}
			// mediaListRange may return the days it already listed before
			// the failing one; use them rather than discarding the channel.
			s.st.LogCh(ch, "warn", "catalog", fmt.Sprintf("列出录像中断（已获 %d 段，继续对账）: %v", len(segs), err))
			if len(segs) == 0 {
				continue
			}
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
	if strings.HasPrefix(old.Err, retentionErrPrefix) || old.Err == "empty time range" {
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
