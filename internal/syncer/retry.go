package syncer

import (
	"time"

	"github.com/myml/nvrsync/internal/store"
)

// ---------------------------------------------------------------------------
// Retry policy
// ---------------------------------------------------------------------------

// downloadWorkers resolves Settings.DownloadWorkers into a usable worker count.
// The default is 1 because a single download already saturates the device's
// 100 Mbit link; larger values are only useful on a gigabit deployment.
func downloadWorkers(settings store.Settings) int {
	n := settings.DownloadWorkers
	if n <= 0 {
		n = defaultDownloadWorkers
	}
	if n > maxDownloadWorkers {
		n = maxDownloadWorkers
	}
	return n
}

// failedRetryDue reports whether a "failed" row may be retried now, given how
// many consecutive runs have already failed on it and when it last synced.
//
// The first failure is retried on the next run; afterwards the cooldown doubles
// with every failure up to failedRetryMax. This keeps a multi-thousand-row
// failure backlog from being re-attempted (each with its full retry ladder)
// ahead of fresh work on every single run.
func failedRetryDue(seg store.Segment, now int64) bool {
	attempts := failedAttempts(seg.Err)
	if attempts <= failedAttemptsBeforeBackoff {
		return true
	}
	last := seg.SyncedAt
	if last <= 0 {
		// No timestamp: treat it as long overdue rather than never retrying.
		return true
	}
	return now-last >= int64(failedRetryBackoff(attempts)/time.Second)
}

// failedRetryBackoff returns the cooldown for a row that has failed attempts
// times in a row.
func failedRetryBackoff(attempts int) time.Duration {
	if attempts <= failedAttemptsBeforeBackoff {
		return 0
	}
	d := failedRetryBase
	for i := failedAttemptsBeforeBackoff + 1; i < attempts; i++ {
		d *= 2
		if d >= failedRetryMax {
			return failedRetryMax
		}
	}
	if d > failedRetryMax {
		d = failedRetryMax
	}
	return d
}
