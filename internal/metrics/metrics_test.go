package metrics

import (
	"strings"
	"sync"
	"testing"
	"time"
)

func render(t *testing.T, r *Registry, f Funcs) string {
	t.Helper()
	var b strings.Builder
	if err := r.WriteTo(&b, f); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	return b.String()
}

func TestExpositionHasHelpAndTypeForEveryMetric(t *testing.T) {
	out := render(t, New(), Funcs{
		Segments: func(string) float64 { return 1 },
		Queue:    func() float64 { return 2 },
		Running:  func() bool { return true },
	})
	want := []string{
		"nvrsync_sync_segments_total", "nvrsync_sync_bytes_total",
		"nvrsync_nvr_busy_total", "nvrsync_nvr_session_wait_seconds_total",
		"nvrsync_nvr_session_wait_count", "nvrsync_nvr_session_wait_aborted_total",
		"nvrsync_download_seconds_total", "nvrsync_download_count",
		"nvrsync_segments", "nvrsync_queue_depth", "nvrsync_sync_running",
		"nvrsync_uptime_seconds",
	}
	for _, name := range want {
		if !strings.Contains(out, "# HELP "+name+" ") {
			t.Errorf("missing HELP for %s", name)
		}
		if !strings.Contains(out, "# TYPE "+name+" ") {
			t.Errorf("missing TYPE for %s", name)
		}
	}
	for _, line := range []string{
		`nvrsync_sync_segments_total{status="done"} 0`,
		`nvrsync_sync_segments_total{status="failed"} 0`,
		`nvrsync_sync_segments_total{status="skipped"} 0`,
		`nvrsync_sync_segments_total{status="pending"} 0`,
		"nvrsync_queue_depth 2",
		"nvrsync_sync_running 1",
	} {
		if !strings.Contains(out, line+"\n") {
			t.Errorf("missing sample %q in:\n%s", line, out)
		}
	}
}

func TestNilFuncsRenderZeroGauges(t *testing.T) {
	out := render(t, New(), Funcs{})
	for _, line := range []string{
		`nvrsync_segments{status="done"} 0`,
		"nvrsync_queue_depth 0",
		"nvrsync_sync_running 0",
	} {
		if !strings.Contains(out, line+"\n") {
			t.Errorf("missing sample %q", line)
		}
	}
}

func TestCountersAccumulate(t *testing.T) {
	r := New()
	r.IncSegment(StatusDone)
	r.IncSegment(StatusDone)
	r.IncSegment("syncing") // folded into pending
	r.IncSegment(StatusFailed)
	r.AddBytes(1000)
	r.AddBytes(-5) // ignored
	r.IncNVRBusy()
	r.ObserveSessionWait(1500*time.Millisecond, true)
	r.ObserveSessionWait(0, true) // not a wait: ignored
	r.ObserveDownload(2 * time.Second)

	out := render(t, r, Funcs{})
	for _, line := range []string{
		`nvrsync_sync_segments_total{status="done"} 2`,
		`nvrsync_sync_segments_total{status="pending"} 1`,
		`nvrsync_sync_segments_total{status="failed"} 1`,
		`nvrsync_sync_segments_total{status="skipped"} 0`,
		"nvrsync_sync_bytes_total 1000",
		"nvrsync_nvr_busy_total 1",
		"nvrsync_nvr_session_wait_seconds_total 1.5",
		"nvrsync_nvr_session_wait_count 1",
		"nvrsync_nvr_session_wait_aborted_total 0",
		"nvrsync_download_seconds_total 2",
		"nvrsync_download_count 1",
	} {
		if !strings.Contains(out, line+"\n") {
			t.Errorf("missing sample %q in:\n%s", line, out)
		}
	}
}

func TestSetDefaultUsesFreshRegistry(t *testing.T) {
	old := Default()
	t.Cleanup(func() { SetDefault(old) })
	SetDefault(New())
	Default().IncNVRBusy()
	if !strings.Contains(render(t, Default(), Funcs{}), "nvrsync_nvr_busy_total 1\n") {
		t.Fatal("new default registry did not reset the counter")
	}
	SetDefault(nil) // nil means "fresh registry"
	if !strings.Contains(render(t, Default(), Funcs{}), "nvrsync_nvr_busy_total 0\n") {
		t.Fatal("SetDefault(nil) did not create an empty registry")
	}
}

// TestRegistryIsConcurrencySafe is meant to run under -race: workers update
// every counter while a scraper renders the exposition.
func TestRegistryIsConcurrencySafe(t *testing.T) {
	r := New()
	var wg sync.WaitGroup
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				r.IncSegment(StatusDone)
				r.AddBytes(1)
				r.IncNVRBusy()
				r.ObserveSessionWait(time.Millisecond, true)
				r.ObserveDownload(time.Millisecond)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
				_ = render(t, r, Funcs{})
			}
		}
	}()
	time.Sleep(20 * time.Millisecond)
	close(done)
	wg.Wait()

	out := render(t, r, Funcs{})
	if !strings.Contains(out, `nvrsync_sync_segments_total{status="done"} 1600`) {
		t.Errorf("done counter did not reach 1600:\n%s", out)
	}
	if !strings.Contains(out, "nvrsync_download_count 1600\n") {
		t.Errorf("download count did not reach 1600:\n%s", out)
	}
}

func TestNegativeDurationClampsToZero(t *testing.T) {
	r := New()
	r.ObserveDownload(-time.Second)
	if !strings.Contains(render(t, r, Funcs{}), "nvrsync_download_seconds_total 0\n") {
		t.Error("negative duration was not clamped")
	}
}

func TestFormatFloat(t *testing.T) {
	if got := formatFloat(3); got != "3" {
		t.Errorf("formatFloat(3) = %q", got)
	}
	if got := formatFloat(0.5); got != "0.5" {
		t.Errorf("formatFloat(0.5) = %q", got)
	}
}
