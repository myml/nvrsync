package syncer

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/myml/nvrsync/internal/metrics"
)

// renderMetrics is the exposition of r with no scrape-time gauges.
func renderMetrics(t *testing.T, r *metrics.Registry) string {
	t.Helper()
	var b strings.Builder
	if err := r.WriteTo(&b, metrics.Funcs{}); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	return b.String()
}

// metricValue extracts `name<labels> value` from an exposition body.
func metricValue(t *testing.T, body, sample string) float64 {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, sample+" ") {
			v, err := strconv.ParseFloat(strings.TrimPrefix(line, sample+" "), 64)
			if err != nil {
				t.Fatalf("parse %q: %v", line, err)
			}
			return v
		}
	}
	t.Fatalf("sample %q missing from exposition:\n%s", sample, body)
	return 0
}

// TestAcquireNVRObservesSessionWait pins the nvrsync_nvr_session_wait_* hooks:
// actually blocking for the session is measured, an uncontended acquisition is
// not counted, and a cancelled waiter is counted as aborted.
func TestAcquireNVRObservesSessionWait(t *testing.T) {
	old := metrics.Default()
	metrics.SetDefault(metrics.New())
	t.Cleanup(func() { metrics.SetDefault(old) })
	st := openSyncerStore(t)
	s := New(st)

	// Uncontended: no wait recorded.
	release, err := s.acquireNVR(context.Background(), "sync")
	if err != nil {
		t.Fatalf("acquireNVR: %v", err)
	}
	if v := metricValue(t, renderMetrics(t, metrics.Default()), "nvrsync_nvr_session_wait_count"); v != 0 {
		t.Fatalf("uncontended acquire counted as a wait (%v)", v)
	}

	// Contended: blocks ~80ms, then obtains the session.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(80 * time.Millisecond)
		release()
	}()
	release2, err := s.acquireNVR(context.Background(), "catalog")
	if err != nil {
		t.Fatalf("contended acquireNVR: %v", err)
	}
	release2()
	wg.Wait()

	body := renderMetrics(t, metrics.Default())
	if v := metricValue(t, body, "nvrsync_nvr_session_wait_count"); v != 1 {
		t.Errorf("nvrsync_nvr_session_wait_count = %v, want 1", v)
	}
	if v := metricValue(t, body, "nvrsync_nvr_session_wait_seconds_total"); v < 0.07 {
		t.Errorf("nvrsync_nvr_session_wait_seconds_total = %v, want >= ~0.08", v)
	}
	if v := metricValue(t, body, "nvrsync_nvr_session_wait_aborted_total"); v != 0 {
		t.Errorf("nvrsync_nvr_session_wait_aborted_total = %v, want 0", v)
	}

	// Cancelled waiter: counted, and as aborted.
	hold, err := s.acquireNVR(context.Background(), "sync")
	if err != nil {
		t.Fatalf("acquireNVR: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := s.acquireNVR(ctx, "catalog")
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := <-done; err == nil {
		t.Fatal("cancelled waiter returned nil error")
	}
	hold()

	body = renderMetrics(t, metrics.Default())
	if v := metricValue(t, body, "nvrsync_nvr_session_wait_count"); v != 2 {
		t.Errorf("nvrsync_nvr_session_wait_count = %v, want 2", v)
	}
	if v := metricValue(t, body, "nvrsync_nvr_session_wait_aborted_total"); v != 1 {
		t.Errorf("nvrsync_nvr_session_wait_aborted_total = %v, want 1", v)
	}
}
