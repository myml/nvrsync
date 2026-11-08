package api

import (
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/myml/nvrsync/internal/metrics"
	"github.com/myml/nvrsync/internal/store"
)

// --- metrics ----------------------------------------------------------------

// metricsContentType is the Prometheus text exposition format (version 0.0.4).
const metricsContentType = "text/plain; version=0.0.4; charset=utf-8"

// handleMetrics renders the sync counters plus the index gauges. The counters
// are process-wide and monotonic; the nvrsync_segments / nvrsync_queue_depth
// gauges are computed from the store on every scrape.
func (s *server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	stats := s.st.Stats(store.SegFilter{})
	f := metrics.Funcs{
		Segments: func(status string) float64 {
			switch status {
			case "done":
				return float64(stats.Done)
			case "failed":
				return float64(stats.Failed)
			case "skipped":
				return float64(stats.Skipped)
			case "pending":
				// "syncing" rows are queued work too: a cancelled run resets
				// them to pending, and one in flight will finish as done or
				// failed (which the counters above already recorded).
				return float64(stats.Pending + stats.Syncing)
			}
			return 0
		},
		Queue: func() float64 {
			return float64(stats.Pending + stats.Syncing + stats.Failed)
		},
		Running: s.sy.Running,
	}

	// Render in memory first so a write error never leaves a truncated
	// exposition body behind a 200.
	var buf strings.Builder
	if err := metrics.Default().WriteTo(&buf, f); err != nil {
		writeError(w, http.StatusInternalServerError, "render metrics: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", metricsContentType)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, buf.String())
}
