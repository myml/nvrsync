// Package metrics keeps the process-wide sync counters and renders them in the
// Prometheus text exposition format (version 0.0.4).
//
// It is deliberately dependency-free: the format is simple enough to write by
// hand, and the module is otherwise kept to modernc.org/sqlite only. Counters
// are atomics, so every hook is safe to call from the sync workers without a
// lock; gauges that depend on external state (the SQLite index, the running
// flag) are computed by the caller at scrape time.
package metrics

import (
	"fmt"
	"io"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Segment statuses exposed as the `status` label of nvrsync_sync_segments_total
// and nvrsync_segments. "syncing" is mapped onto "pending" so the two series
// always describe the same queue (a row is only transiently "syncing").
const (
	StatusDone    = "done"
	StatusFailed  = "failed"
	StatusSkipped = "skipped"
	StatusPending = "pending"
)

// statuses is the fixed label order for the segment counters and gauges.
var statuses = [4]string{StatusDone, StatusFailed, StatusSkipped, StatusPending}

// statusIndex maps a store status onto one of statuses; unknown values fall
// back to pending.
func statusIndex(status string) int {
	switch status {
	case StatusDone:
		return 0
	case StatusFailed:
		return 1
	case StatusSkipped:
		return 2
	default:
		return 3
	}
}

// Registry is the process-wide metric set. The zero value is not usable; call
// New. All methods are safe for concurrent use.
type Registry struct {
	segments [len(statuses)]atomic.Uint64 // keyed by statusIndex
	bytes    atomic.Uint64

	nvrBusy atomic.Uint64

	// Session wait: total time a caller blocked in acquireNVR (before the
	// lock was obtained), the number of such waits and the number that ended
	// because ctx was cancelled instead of obtaining the lock.
	sessionWaitNanos   atomic.Uint64
	sessionWaitCount   atomic.Uint64
	sessionWaitAborted atomic.Uint64

	// Per-segment download duration: only attempts that actually moved bytes
	// (the file was persisted) are measured; failed attempts have their own
	// interpretation and would drag the average down.
	downloadNanos atomic.Uint64
	downloadCount atomic.Uint64

	startedAt time.Time
}

// New returns an empty registry stamped with its creation time.
func New() *Registry {
	return &Registry{startedAt: time.Now()}
}

// Funcs holds the scrape-time callbacks for the gauges that cannot be kept in
// the registry itself. A nil callback renders as zero.
type Funcs struct {
	Segments func(status string) float64
	Queue    func() float64
	Running  func() bool
}

var (
	mu      sync.Mutex
	current = New()
)

// Default returns the process-wide registry used by the syncer and the API.
func Default() *Registry { return current }

// SetDefault replaces the process-wide registry. It is meant for tests that
// need an isolated counter set; production code never calls it.
func SetDefault(r *Registry) {
	if r == nil {
		r = New()
	}
	mu.Lock()
	current = r
	mu.Unlock()
}

// --- recording --------------------------------------------------------------

// IncSegment records one segment outcome. The counter tracks attempts (what
// the syncer did), not the current index contents: see Gauge nvrsync_segments
// for the latter.
func (r *Registry) IncSegment(status string) {
	r.segments[statusIndex(status)].Add(1)
}

// AddBytes records bytes written to disk by successful downloads.
func (r *Registry) AddBytes(n int64) {
	if n > 0 {
		r.bytes.Add(uint64(n))
	}
}

// IncNVRBusy records one NVR busy (-40401) reply.
func (r *Registry) IncNVRBusy() {
	r.nvrBusy.Add(1)
}

// ObserveSessionWait records one wait in acquireNVR: waited is how long the
// caller blocked before obtaining the session and obtained reports whether it
// got the session (false when ctx was cancelled while waiting).
func (r *Registry) ObserveSessionWait(waited time.Duration, obtained bool) {
	if waited <= 0 {
		return
	}
	r.sessionWaitNanos.Add(uint64(waited))
	r.sessionWaitCount.Add(1)
	if !obtained {
		r.sessionWaitAborted.Add(1)
	}
}

// ObserveDownload records the wall time of one successful segment download.
func (r *Registry) ObserveDownload(d time.Duration) {
	if d < 0 {
		d = 0
	}
	r.downloadNanos.Add(uint64(d))
	r.downloadCount.Add(1)
}

// --- exposition -------------------------------------------------------------

// WriteTo renders the registry in the Prometheus text exposition format
// (version 0.0.4). Every metric is emitted even when its value is zero, so a
// scraper sees a stable series set from the first scrape.
func (r *Registry) WriteTo(w io.Writer, f Funcs) error {
	b := &builder{w: w}

	b.header("nvrsync_sync_segments_total", "counter",
		"Segment download outcomes recorded by the syncer, by final status (attempts, not index size; syncing is reported as pending).")
	for i, status := range statuses {
		b.sample("nvrsync_sync_segments_total", `{status="`+status+`"}`, float64(r.segments[i].Load()))
	}

	b.header("nvrsync_sync_bytes_total", "counter",
		"Bytes of recording written to disk by successful segment downloads.")
	b.sample("nvrsync_sync_bytes_total", "", float64(r.bytes.Load()))

	b.header("nvrsync_nvr_busy_total", "counter",
		"Times the NVR answered the busy error (-40401, out of media sessions).")
	b.sample("nvrsync_nvr_busy_total", "", float64(r.nvrBusy.Load()))

	b.header("nvrsync_nvr_session_wait_seconds_total", "counter",
		"Total time callers waited to acquire the exclusive NVR session (before holding it).")
	b.sample("nvrsync_nvr_session_wait_seconds_total", "", nsToSeconds(r.sessionWaitNanos.Load()))

	b.header("nvrsync_nvr_session_wait_count", "counter",
		"Number of acquireNVR calls that had to wait for the NVR session.")
	b.sample("nvrsync_nvr_session_wait_count", "", float64(r.sessionWaitCount.Load()))

	b.header("nvrsync_nvr_session_wait_aborted_total", "counter",
		"Waits for the NVR session that ended in cancellation instead of obtaining it.")
	b.sample("nvrsync_nvr_session_wait_aborted_total", "", float64(r.sessionWaitAborted.Load()))

	b.header("nvrsync_download_seconds_total", "counter",
		"Total wall time of successful per-segment downloads.")
	b.sample("nvrsync_download_seconds_total", "", nsToSeconds(r.downloadNanos.Load()))

	b.header("nvrsync_download_count", "counter",
		"Number of successful per-segment downloads measured by nvrsync_download_seconds_total.")
	b.sample("nvrsync_download_count", "", float64(r.downloadCount.Load()))

	b.header("nvrsync_segments", "gauge",
		"Segments in the local index by status; syncing is reported as pending.")
	for _, status := range statuses {
		v := 0.0
		if f.Segments != nil {
			v = f.Segments(status)
		}
		b.sample("nvrsync_segments", `{status="`+status+`"}`, v)
	}

	b.header("nvrsync_queue_depth", "gauge",
		"Segments currently waiting to be downloaded (pending plus failed).")
	q := 0.0
	if f.Queue != nil {
		q = f.Queue()
	}
	b.sample("nvrsync_queue_depth", "", q)

	b.header("nvrsync_sync_running", "gauge",
		"1 while a sync run is in flight, 0 otherwise.")
	running := 0.0
	if f.Running != nil && f.Running() {
		running = 1
	}
	b.sample("nvrsync_sync_running", "", running)

	b.header("nvrsync_uptime_seconds", "gauge",
		"Seconds since the process started.")
	b.sample("nvrsync_uptime_seconds", "", math.Max(0, time.Since(r.startedAt).Seconds()))

	return b.err
}

func nsToSeconds(ns uint64) float64 { return float64(ns) / float64(time.Second) }

// builder accumulates the exposition text line by line.
type builder struct {
	w   io.Writer
	err error
}

func (b *builder) line(s string) {
	if b.err != nil {
		return
	}
	_, b.err = io.WriteString(b.w, s+"\n")
}

func (b *builder) header(name, typ, help string) {
	b.line("# HELP " + name + " " + escapeHelp(help))
	b.line("# TYPE " + name + " " + typ)
}

func (b *builder) sample(name, labels string, v float64) {
	b.line(name + labels + " " + formatFloat(v))
}

// escapeHelp escapes backslashes and newlines in HELP text, as the format
// requires.
func escapeHelp(s string) string {
	if !strings.ContainsAny(s, "\\\n") {
		return s
	}
	r := strings.NewReplacer(`\`, `\\`, "\n", `\n`)
	return r.Replace(s)
}

// formatFloat renders a sample value: integers stay integral (Prometheus
// accepts either), non-finite values become NaN or +/-Inf.
func formatFloat(v float64) string {
	switch {
	case math.IsNaN(v):
		return "NaN"
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	}
	return fmt.Sprintf("%g", v)
}
