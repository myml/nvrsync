package syncer

import (
	"testing"
	"time"

	"github.com/myml/nvrsync/internal/store"
)

// TestFailedAttemptsRoundTrip pins the failure counter encoding stored in
// Segment.Err, including rows written by older builds that have no counter.
func TestFailedAttemptsRoundTrip(t *testing.T) {
	cases := []struct {
		err  string
		want int
	}{
		{"", 0},
		{"nvr: download: truncated", 0}, // legacy row
		{"failures=1: nvr: download: truncated", 1},
		{"failures=12: x", 12},
		{"failures=: x", 0},
		{"failures=abc: x", 0},
		{"failures=3", 0},
	}
	for _, c := range cases {
		if got := failedAttempts(c.err); got != c.want {
			t.Errorf("failedAttempts(%q) = %d, want %d", c.err, got, c.want)
		}
		// The record itself must round-trip.
		if c.want > 0 {
			rec := failureRecord{failures: c.want, msg: "x"}.String()
			if got := failedAttempts(rec); got != c.want {
				t.Errorf("round trip %q -> %d, want %d", rec, got, c.want)
			}
		}
	}
}

// TestFailedRetryBackoffIsExponentialAndCapped verifies the per-segment cooldown
// grows with consecutive failures and stops at failedRetryMax.
func TestFailedRetryBackoffIsExponentialAndCapped(t *testing.T) {
	if got := failedRetryBackoff(failedAttemptsBeforeBackoff); got != 0 {
		t.Errorf("first failure backoff = %v, want 0 (retry next run)", got)
	}
	prev := failedRetryBackoff(failedAttemptsBeforeBackoff + 1)
	if prev <= 0 {
		t.Fatalf("second failure backoff = %v, want > 0", prev)
	}
	for attempts := failedAttemptsBeforeBackoff + 2; attempts <= 20; attempts++ {
		got := failedRetryBackoff(attempts)
		if got < prev {
			t.Errorf("attempts=%d backoff %v shrank below %v", attempts, got, prev)
		}
		if got > failedRetryMax {
			t.Errorf("attempts=%d backoff %v exceeds cap %v", attempts, got, failedRetryMax)
		}
		prev = got
	}
	if prev != failedRetryMax {
		t.Errorf("backoff did not reach the cap: got %v want %v", prev, failedRetryMax)
	}
}

// TestFailedRetryDue checks the gate that keeps a failure backlog from being
// re-attempted on every run.
func TestFailedRetryDue(t *testing.T) {
	now := time.Now().Unix()
	seg := func(err string, syncedAt int64) store.Segment {
		return store.Segment{Ch: 1, Start: 1, End: 2, Status: "failed", Err: err, SyncedAt: syncedAt}
	}
	// First failure and legacy rows without a counter are retried immediately.
	if !failedRetryDue(seg("failures=1: x", now), now) {
		t.Error("first failure should be due immediately")
	}
	if !failedRetryDue(seg("legacy error", now), now) {
		t.Error("legacy failure without a counter should be due")
	}
	// A repeatedly failing row waits out its cooldown.
	polite := seg("failures=3: x", now)
	if failedRetryDue(polite, now) {
		t.Error("row that just failed 3 times should not be due yet")
	}
	backoff := failedRetryBackoff(3)
	if !failedRetryDue(polite, now+int64(backoff/time.Second)) {
		t.Error("row should be due once the cooldown elapsed")
	}
	// A row with no timestamp must not be stuck forever.
	if !failedRetryDue(seg("failures=9: x", 0), now) {
		t.Error("row without a timestamp should be retried")
	}
}

// TestCollectQueueSkipsBackedOffFailures proves the end-to-end effect: a failed
// segment inside its cooldown is not queued while pending work still is.
func TestCollectQueueSkipsBackedOffFailures(t *testing.T) {
	st := openSyncerStore(t)
	now := time.Now().Unix()
	pending := store.Segment{Ch: 1, Start: now - 3600, End: now - 3400, Status: "pending"}
	hot := store.Segment{
		Ch: 1, Start: now - 3600, End: now - 3400, Status: "failed",
		Err: "failures=5: nvr: download: truncated", SyncedAt: now,
	}
	cold := store.Segment{
		Ch: 1, Start: now - 7200, End: now - 7000, Status: "failed",
		Err: "failures=5: nvr: download: truncated", SyncedAt: 0,
	}
	// Distinct keys: the upsert merges equal (ch, start, end).
	hot.Start, hot.End = now-3000, now-2800
	cold.Start, cold.End = now-7000, now-6800
	for _, seg := range []store.Segment{pending, hot, cold} {
		if err := st.UpsertSegment(seg); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}

	queue := New(st).collectQueue([]int{1}, 0, 0)
	got := map[string]bool{}
	for _, seg := range queue {
		got[segKeyString(seg)] = true
	}
	if !got[segKeyString(pending)] {
		t.Error("pending segment missing from queue")
	}
	if got[segKeyString(hot)] {
		t.Error("failed segment inside its cooldown was queued")
	}
	if !got[segKeyString(cold)] {
		t.Error("failed segment without a timestamp should still be queued")
	}
}

// TestDownloadWorkersSettings pins the default and the clamp.
func TestDownloadWorkersSettings(t *testing.T) {
	cases := []struct {
		in   int
		want int
	}{
		{0, defaultDownloadWorkers},
		{-3, defaultDownloadWorkers},
		{1, 1},
		{2, 2},
		{maxDownloadWorkers, maxDownloadWorkers},
		{maxDownloadWorkers + 10, maxDownloadWorkers},
	}
	for _, c := range cases {
		if got := downloadWorkers(store.Settings{DownloadWorkers: c.in}); got != c.want {
			t.Errorf("downloadWorkers(%d) = %d, want %d", c.in, got, c.want)
		}
	}
	if defaultDownloadWorkers != 1 {
		t.Errorf("default workers = %d, want 1 (one stream saturates the 100 Mbit link)", defaultDownloadWorkers)
	}
}
