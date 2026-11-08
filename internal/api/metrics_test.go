package api

import (
	"bufio"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/myml/nvrsync/internal/metrics"
	"github.com/myml/nvrsync/internal/play"
	"github.com/myml/nvrsync/internal/store"
	"github.com/myml/nvrsync/internal/syncer"
)

// metricSample is one parsed `name{labels} value` line.
type metricSample struct {
	labels map[string]string
	value  float64
}

var (
	sampleLine = regexp.MustCompile(`^([a-zA-Z_:][a-zA-Z0-9_:]*)(\{(.*)\})? (\S+)$`)
	labelPart  = regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)="((?:[^"\\]|\\.)*)"`)
)

// parseExposition parses a Prometheus text-format body into
// metric name -> label fingerprint -> sample. It also asserts the structural
// rules the format requires: every sample belongs to a metric with exactly one
// HELP and one TYPE line, TYPE comes after HELP, and values parse as floats.
func parseExposition(t *testing.T, body string) map[string]map[string]metricSample {
	t.Helper()
	if !strings.HasSuffix(body, "\n") {
		t.Fatal("exposition body must end with a newline")
	}
	out := map[string]map[string]metricSample{}
	help := map[string]bool{}
	typ := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(body))
	for i := 1; sc.Scan(); i++ {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "# HELP "):
			fields := strings.SplitN(strings.TrimPrefix(line, "# HELP "), " ", 2)
			if len(fields) != 2 || fields[1] == "" {
				t.Fatalf("line %d: malformed HELP: %q", i, line)
			}
			if help[fields[0]] {
				t.Fatalf("line %d: duplicate HELP for %s", i, fields[0])
			}
			help[fields[0]] = true
		case strings.HasPrefix(line, "# TYPE "):
			fields := strings.SplitN(strings.TrimPrefix(line, "# TYPE "), " ", 2)
			if len(fields) != 2 || (fields[1] != "counter" && fields[1] != "gauge") {
				t.Fatalf("line %d: malformed TYPE: %q", i, line)
			}
			if typ[fields[0]] != "" {
				t.Fatalf("line %d: duplicate TYPE for %s", i, fields[0])
			}
			if !help[fields[0]] {
				t.Fatalf("line %d: TYPE for %s before its HELP", i, fields[0])
			}
			typ[fields[0]] = fields[1]
		case strings.HasPrefix(line, "#"):
			// other comment; nothing to check
		case strings.TrimSpace(line) == "":
			t.Fatalf("line %d: blank line inside the exposition", i)
		default:
			m := sampleLine.FindStringSubmatch(line)
			if m == nil {
				t.Fatalf("line %d: not a valid sample: %q", i, line)
			}
			name := m[1]
			if typ[name] == "" {
				t.Fatalf("line %d: sample for %s without HELP/TYPE", i, name)
			}
			labels := map[string]string{}
			for _, lm := range labelPart.FindAllStringSubmatch(m[3], -1) {
				labels[lm[1]] = lm[2]
			}
			v, err := strconv.ParseFloat(m[4], 64)
			if err != nil {
				t.Fatalf("line %d: value %q of %s is not a number", i, m[4], name)
			}
			if v < 0 {
				t.Fatalf("line %d: %s = %v, want a non-negative value", i, name, v)
			}
			if out[name] == nil {
				out[name] = map[string]metricSample{}
			}
			key := labelFingerprint(labels)
			if _, dup := out[name][key]; dup {
				t.Fatalf("line %d: duplicate series %s%s", i, name, m[2])
			}
			out[name][key] = metricSample{labels: labels, value: v}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan body: %v", err)
	}
	for name := range help {
		if typ[name] == "" {
			t.Fatalf("%s has HELP but no TYPE", name)
		}
	}
	return out
}

func labelFingerprint(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	// Insertion sort keeps the helper dependency-free.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k + "=" + labels[k] + ";")
	}
	return b.String()
}

// scrapeMetrics GETs /api/metrics through the test server and parses the body.
func scrapeMetrics(t *testing.T, a *testAPI, cookies ...*http.Cookie) (map[string]map[string]metricSample, string) {
	t.Helper()
	rec := a.do(http.MethodGet, "/api/metrics", nil, cookies...)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/metrics status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "text/plain; version=0.0.4; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want text/plain; version=0.0.4; charset=utf-8", got)
	}
	raw := rec.Body.String()
	return parseExposition(t, raw), raw
}

func sampleValue(t *testing.T, all map[string]map[string]metricSample, name string, labels map[string]string) float64 {
	t.Helper()
	series, ok := all[name]
	if !ok {
		t.Fatalf("metric %s missing from exposition", name)
	}
	s, ok := series[labelFingerprint(labels)]
	if !ok {
		t.Fatalf("series %s%v missing from exposition (have %d series)", name, labels, len(series))
	}
	return s.value
}

// TestMetricsEndpointExposesExpectedSeries checks the shape of /api/metrics
// against a seeded index: every documented metric exists, the gauges reflect
// the store and the counters start at zero.
func TestMetricsEndpointExposesExpectedSeries(t *testing.T) {
	metrics.SetDefault(metrics.New())
	a := newTestAPI(t)

	seed := []store.Segment{
		{Ch: 1, Start: 1000, End: 1060, Status: "done"},
		{Ch: 1, Start: 1060, End: 1120, Status: "done"},
		{Ch: 1, Start: 1120, End: 1180, Status: "pending"},
		{Ch: 1, Start: 1180, End: 1240, Status: "failed", Err: "failures=1: boom"},
		{Ch: 1, Start: 1240, End: 1300, Status: "skipped"},
		{Ch: 1, Start: 1300, End: 1360, Status: "syncing"},
		{Ch: 1, Start: 1360, End: 1420, Status: "local_only"},
	}
	for _, seg := range seed {
		if err := a.st.UpsertSegment(seg); err != nil {
			t.Fatalf("UpsertSegment: %v", err)
		}
	}

	all, raw := scrapeMetrics(t, a)

	// Counters exist, one series per status, and start at zero.
	for _, status := range []string{"done", "failed", "skipped", "pending"} {
		if v := sampleValue(t, all, "nvrsync_sync_segments_total", map[string]string{"status": status}); v != 0 {
			t.Errorf("nvrsync_sync_segments_total{status=%q} = %v, want 0 before any sync", status, v)
		}
	}
	for _, name := range []string{
		"nvrsync_sync_bytes_total",
		"nvrsync_nvr_busy_total",
		"nvrsync_nvr_session_wait_seconds_total",
		"nvrsync_nvr_session_wait_count",
		"nvrsync_download_seconds_total",
		"nvrsync_download_count",
	} {
		if v := sampleValue(t, all, name, nil); v != 0 {
			t.Errorf("%s = %v, want 0 before any work", name, v)
		}
	}

	// Gauges come from the store. "syncing" is folded into "pending" so the
	// gauge series match the counter series.
	wantSegments := map[string]float64{"done": 2, "failed": 1, "skipped": 1, "pending": 2}
	for status, want := range wantSegments {
		if v := sampleValue(t, all, "nvrsync_segments", map[string]string{"status": status}); v != want {
			t.Errorf("nvrsync_segments{status=%q} = %v, want %v", status, v, want)
		}
	}
	if got := len(all["nvrsync_segments"]); got != 4 {
		t.Errorf("nvrsync_segments has %d series, want 4", got)
	}
	// queue depth counts everything still to be downloaded: pending + syncing
	// + failed.
	if v := sampleValue(t, all, "nvrsync_queue_depth", nil); v != 3 {
		t.Errorf("nvrsync_queue_depth = %v, want 3", v)
	}
	if v := sampleValue(t, all, "nvrsync_sync_running", nil); v != 0 {
		t.Errorf("nvrsync_sync_running = %v, want 0", v)
	}
	if v := sampleValue(t, all, "nvrsync_uptime_seconds", nil); v < 0 {
		t.Errorf("nvrsync_uptime_seconds = %v, want >= 0", v)
	}

	// TYPE lines must match the documented metric kinds.
	if !strings.Contains(raw, "# TYPE nvrsync_sync_segments_total counter\n") {
		t.Error("nvrsync_sync_segments_total is not declared as a counter")
	}
	if !strings.Contains(raw, "# TYPE nvrsync_segments gauge\n") {
		t.Error("nvrsync_segments is not declared as a gauge")
	}
}

// TestMetricsCounterIncrementsAfterSync runs a real sync through the API against
// a fake NVR and asserts the counters moved: done segments, bytes, download
// duration and session wait.
func TestMetricsCounterIncrementsAfterSync(t *testing.T) {
	metrics.SetDefault(metrics.New())

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sy := syncer.New(st)
	a := &testAPI{t: t, st: st, h: New(st, sy, play.New(st), nil, "test")}

	f := newFakeAPINVR(t, [][2]int64{{0, 0}})
	storage := t.TempDir()
	if err := st.SaveSettings(store.Settings{
		NVRHost: f.host(), NVRUser: "admin", NVRPass: "secret",
		AdminPort: f.adminPort(), StorageDir: storage, Channels: []int{1},
	}); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	// One day back: newer than RetentionD (so retention, when it runs, leaves
	// these files alone) but older than the live guard.
	now := time.Now().Unix() - 86400
	for _, seg := range [][2]int64{{now - 900, now - 780}, {now - 780, now - 660}, {now - 660, now - 540}} {
		if err := st.UpsertSegment(store.Segment{Ch: 1, Start: seg[0], End: seg[1], Status: "pending"}); err != nil {
			t.Fatalf("UpsertSegment: %v", err)
		}
	}

	before, _ := scrapeMetrics(t, a)
	beforeDone := sampleValue(t, before, "nvrsync_sync_segments_total", map[string]string{"status": "done"})

	rec := a.do(http.MethodPost, "/api/sync/start", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("sync start status = %d; body=%s", rec.Code, rec.Body.String())
	}
	deadline := time.Now().Add(10 * time.Second)
	for sy.Running() {
		if time.Now().After(deadline) {
			t.Fatal("sync did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}

	after, _ := scrapeMetrics(t, a)
	done := sampleValue(t, after, "nvrsync_sync_segments_total", map[string]string{"status": "done"})
	if done-beforeDone != 3 {
		t.Errorf("nvrsync_sync_segments_total{status=done} went %v -> %v, want +3", beforeDone, done)
	}
	// The fake device serves two 188-byte packets per segment.
	if v := sampleValue(t, after, "nvrsync_sync_bytes_total", nil); v != 3*376 {
		t.Errorf("nvrsync_sync_bytes_total = %v, want %v", v, 3*376)
	}
	if v := sampleValue(t, after, "nvrsync_download_count", nil); v != 3 {
		t.Errorf("nvrsync_download_count = %v, want 3", v)
	}
	if v := sampleValue(t, after, "nvrsync_download_seconds_total", nil); v <= 0 {
		t.Errorf("nvrsync_download_seconds_total = %v, want > 0", v)
	}
	// The run took the NVR session uncontended, so there was no waiting, but
	// the scrape must still expose the (zero) series.
	if v := sampleValue(t, after, "nvrsync_nvr_session_wait_count", nil); v != 0 {
		t.Errorf("nvrsync_nvr_session_wait_count = %v, want 0 for an uncontended run", v)
	}
	// All three segments are done now: the gauge follows the store.
	if v := sampleValue(t, after, "nvrsync_segments", map[string]string{"status": "done"}); v != 3 {
		t.Errorf("nvrsync_segments{status=done} = %v, want 3", v)
	}
	if v := sampleValue(t, after, "nvrsync_queue_depth", nil); v != 0 {
		t.Errorf("nvrsync_queue_depth = %v, want 0", v)
	}
	if v := sampleValue(t, after, "nvrsync_sync_running", nil); v != 0 {
		t.Errorf("nvrsync_sync_running = %v, want 0 after the sync finished", v)
	}
}

// TestMetricsBusyCounterTracksNVRBusy pins the nvrsync_nvr_busy_total hook: the
// fake device answers every media list with -40401, so a catalogue refresh must
// bump the counter.
func TestMetricsBusyCounterTracksNVRBusy(t *testing.T) {
	metrics.SetDefault(metrics.New())

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sy := syncer.New(st)
	a := &testAPI{t: t, st: st, h: New(st, sy, play.New(st), nil, "test")}

	f := newFakeAPINVR(t, [][2]int64{{0, 0}})
	f.busy = true
	if err := st.SaveSettings(store.Settings{
		NVRHost: f.host(), NVRUser: "admin", NVRPass: "secret",
		AdminPort: f.adminPort(), StorageDir: t.TempDir(), Channels: []int{1},
		CatalogDays: 1, CatalogChannels: []int{1, 2},
	}); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}

	before, _ := scrapeMetrics(t, a)
	if _, err := sy.RefreshCatalogForce(t.Context(), true); err != nil {
		t.Fatalf("RefreshCatalogForce: %v", err)
	}
	after, _ := scrapeMetrics(t, a)

	b0 := sampleValue(t, before, "nvrsync_nvr_busy_total", nil)
	b1 := sampleValue(t, after, "nvrsync_nvr_busy_total", nil)
	if b1 <= b0 {
		t.Errorf("nvrsync_nvr_busy_total = %v -> %v, want an increase after -40401", b0, b1)
	}
}

// TestMetricsRequiresAuth confirms /api/metrics follows the same rule as every
// other /api/ endpoint: 401 without a session when login protection is on, 200
// with it. It is not on the public-path list.
func TestMetricsRequiresAuth(t *testing.T) {
	metrics.SetDefault(metrics.New())
	a := newTestAPI(t)
	a.enableAuth("admin", "s3cret")

	if rec := a.do(http.MethodGet, "/api/metrics", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET /api/metrics = %d, want 401; body=%s", rec.Code, rec.Body.String())
	}
	cookie := a.login("admin", "s3cret")
	scrapeMetrics(t, a, cookie)
}
