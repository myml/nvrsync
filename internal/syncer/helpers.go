package syncer

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/myml/nvrsync/internal/store"
)

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
