// Package syncer is the NVR -> local disk sync engine.
//
// A run is deliberately simple and restartable:
//
//  1. scan: for each selected channel ask the NVR for indexed segments since
//     the last successfully synced position (or the backfill window) and
//     upsert them as "pending";
//  2. queue: take every pending/failed segment of the selected channels;
//  3. download: two workers pull segments concurrently (the NVR supports 8
//     sessions, so 2 downloads plus occasional scans stay well below the
//     limit), each into rec/camN/YYYY-MM-DD/HH-MM-SS_HH-MM-SS.ts;
//  4. verify and record: non-empty file, 188-byte TS sync headers, sidecar
//     JSON next to the file, then UpsertSegment{Status:"done"};
//  5. retention: when RetentionD > 0, remove expired files that are both
//     "done"/"local_only" and inside StorageDir, drop their index rows and
//     the empty date directories they leave behind.
//
// Failures are retried twice with exponential backoff and then recorded as
// "failed" with the error message. Progress is published through
// store.SetSyncState; every scan and download is also written to the store
// log.
package syncer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/myml/nvrsync/internal/metrics"
	"github.com/myml/nvrsync/internal/store"
)

const (
	// defaultDownloadWorkers is how many segments are pulled at once when
	// Settings.DownloadWorkers is unset.
	//
	// One is deliberate: a single download already saturates the device's
	// 100 Mbit interface (89 Mbps measured, ~11.2 MB/s, see DL_MEASURE.md in
	// the nvr2rtc repository), so a second worker adds no throughput. It only
	// competes for the same link, burns another of the device's few media
	// sessions and doubles the chance of a -40401 storm when the NVR is busy.
	defaultDownloadWorkers = 1
	// maxDownloadWorkers is capped well below the device's stream_max_sessions
	// (8) so scans and the catalogue refresh still fit.
	maxDownloadWorkers = 4
	// maxAttempts includes the first try: two retries after a failure.
	maxAttempts = 3
	// maxBusyStreak stops one run after this many consecutive busy failures,
	// so a saturated NVR does not turn the whole queue into retries.
	maxBusyStreak = 8
	// maxFailedPerRun bounds automatic retries of the permanent-looking
	// backlog. Pending segments are always processed first.
	maxFailedPerRun = 200
	// failedAttemptsBeforeBackoff lets a row be retried on the round after its
	// first failure, so a transient hiccup is not punished with the full
	// backoff. From the second failure on, failedRetryBackoff applies.
	failedAttemptsBeforeBackoff = 1
	// minFreeBytes keeps the last gigabyte of the volume free for SQLite,
	// sidecars and the OS; a 1 GB NVR segment can otherwise fill the disk.
	minFreeBytes = 1 << 30
	// autoPoll is how often AutoLoop re-reads settings, so changing
	// AutoMinutes takes effect without restarting the process.
	autoPoll = 15 * time.Second
	// liveGuard leaves the newest recordings alone for a couple of minutes:
	// a segment that ends too close to "now" may still be recording, and
	// downloading it now would persist a truncated file whose tail the
	// watermark then skips forever.
	liveGuard = 120 // seconds
)

// Retry pacing. These are variables rather than constants so tests can shorten
// the retry loop; production code never changes them.
var (
	// retryBase is the backoff for an ordinary failure.
	retryBase = 2 * time.Second
	// busyRetryBase is used for the NVR's -40401 ("no session budget") reply.
	// It is transient; hammering the device with 2s retries makes it worse.
	busyRetryBase = 10 * time.Second
	// transientRetryBase is used for device/network hiccups (truncated stream,
	// idle timeout, broken handshake). These say nothing about whether the
	// segment can be fetched later, so they back off much longer than a plain
	// retry and the segment stays pending instead of being marked failed.
	transientRetryBase = 10 * time.Second
	// failedRetryBase is the per-segment cooldown for rows in the "failed"
	// state; it doubles with each consecutive failure up to failedRetryMax.
	// Without it every run re-tried the same maxFailedPerRun failures first,
	// each with its full 3-attempt ladder, so a multi-thousand-row backlog
	// kept a run busy for tens of minutes without downloading anything new.
	failedRetryBase = 10 * time.Minute
	failedRetryMax  = 6 * time.Hour
)

// retrySleep is the canceller-aware wait used between download attempts. It is
// a variable so a test can shorten the backoff without touching the production
// pacing constants above.
var retrySleep = sleepCtx

// ErrAlreadyRunning is returned by Start when a sync is already in flight.
var ErrAlreadyRunning = errors.New("syncer: sync already running")

// ErrCatalogBusy is returned when a catalogue refresh is already running.
var ErrCatalogBusy = errors.New("syncer: catalogue refresh already running")

// catalogCooldown coalesces bursts of refresh requests (page loads, multiple
// tabs, the background loop) into at most one NVR scan per interval.
const catalogCooldown = 60 * time.Second

// SyncRequest selects which channels and which time range a sync drains from
// the local index. Zero From/To means "everything pending".
//
// Channels empty means Settings.Channels. From/To are unix seconds filtering
// which segments start inside the range; zero means "no bound".
type SyncRequest struct {
	Channels []int `json:"channels"`
	From     int64 `json:"from"`
	To       int64 `json:"to"`
}

// Syncer owns the sync lifecycle. It is safe for concurrent use.
type Syncer struct {
	st *store.Store

	mu      sync.Mutex
	running bool
	cancel  context.CancelFunc
	done    chan struct{}

	// Catalogue refreshes are expensive (one NVR login + many media list
	// queries), so concurrent triggers are coalesced and repeats inside
	// catalogCooldown reuse the previous result.
	catalogMu   sync.Mutex
	catalogAt   time.Time
	catalogLast CatalogResult

	// nvrSession serialises *ownership* of the device's media sessions: a
	// sync run and a catalogue refresh must never talk to the NVR at the same
	// time. The device has a small session budget (stream_max_sessions=8) and
	// answers -40401 when it runs out, so overlapping work turns into a retry
	// storm. The old guard was `!s.Running()`, which races both ways: the
	// catalogue could start in the window between Start() and the run goroutine
	// setting the flag, and a sync could start on top of a running catalogue.
	nvrSession chan struct{}
	// activeSession names the current holder ("sync" / "catalog") for logs.
	activeSession atomic.Value

	// Stale .part files are removed once per process, not on every run.
	cleanupOnce sync.Once
}

// New returns a Syncer backed by st.
func New(st *store.Store) *Syncer {
	return &Syncer{st: st, nvrSession: make(chan struct{}, 1)}
}

// acquireNVR takes exclusive ownership of the NVR session budget until release
// is called. It blocks until the current holder (if any) is done or ctx is
// cancelled, and logs once when it has to wait so a queued run is visible in
// the sync log instead of looking stuck.
//
// Every path that logs in to the device or opens a media session must hold
// this lock: sync runs, catalogue refreshes and on-demand scans.
func (s *Syncer) acquireNVR(ctx context.Context, who string) (func(), error) {
	waited := time.Now()
	select {
	case s.nvrSession <- struct{}{}:
	default:
		s.st.Log("info", who, fmt.Sprintf("%s 正在等待 NVR 会话（%s 占用中）", whoLabel(who), s.SessionHolder()))
		select {
		case s.nvrSession <- struct{}{}:
			metrics.Default().ObserveSessionWait(time.Since(waited), true)
		case <-ctx.Done():
			metrics.Default().ObserveSessionWait(time.Since(waited), false)
			return nil, ctx.Err()
		}
	}
	s.activeSession.Store(who)
	var once sync.Once
	return func() {
		once.Do(func() {
			s.activeSession.Store("")
			<-s.nvrSession
		})
	}, nil
}

// WithNVRSession runs fn while holding exclusive ownership of the NVR session
// and releases it afterwards, even if fn panics. It is the public form of
// acquireNVR for callers that live outside the sync loop (the API's "test
// connection" button logs in and lists media exactly like a sync does), so
// they queue behind — instead of competing with — a running sync, scan or
// catalogue refresh for the device's small session budget.
func (s *Syncer) WithNVRSession(ctx context.Context, who string, fn func() error) error {
	release, err := s.acquireNVR(ctx, who)
	if err != nil {
		return err
	}
	defer release()
	return fn()
}

// SessionHolder reports which activity currently owns the NVR session, or ""
// when the device is idle. It is for diagnostics and tests.
func (s *Syncer) SessionHolder() string {
	if v, ok := s.activeSession.Load().(string); ok {
		return v
	}
	return ""
}

// whoLabel is the human-readable form of a session owner for logs.
func whoLabel(who string) string {
	switch who {
	case "sync":
		return "同步"
	case "catalog":
		return "对账"
	case "scan":
		return "扫描"
	case "test":
		return "测试连接"
	default:
		return who
	}
}

// Start validates the request, marks the sync as running and returns
// immediately; the work happens in a background goroutine. Progress is
// available through store.SyncState.
func (s *Syncer) Start(req SyncRequest) error {
	if s == nil || s.st == nil {
		return errors.New("syncer: nil store")
	}
	settings := s.st.Settings()
	channels := req.Channels
	if len(channels) == 0 {
		channels = settings.Channels
	}
	channels = cleanChannels(channels)
	if len(channels) == 0 {
		return errors.New("syncer: no channels configured")
	}
	if strings.TrimSpace(settings.NVRHost) == "" {
		return errors.New("syncer: NVR host is not configured")
	}
	if strings.TrimSpace(settings.StorageDir) == "" {
		return errors.New("syncer: storage directory is not configured")
	}
	if req.From > 0 && req.To > 0 && req.To <= req.From {
		return fmt.Errorf("syncer: invalid range %d..%d", req.From, req.To)
	}

	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return ErrAlreadyRunning
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s.running = true
	s.cancel = cancel
	s.done = done
	s.mu.Unlock()

	// Publish "running" before returning so /api/sync/status never shows a
	// stale idle state after a successful start.
	s.st.SetSyncState(store.SyncState{Running: true, StartedAt: time.Now().Unix()})

	req.Channels = channels
	go s.run(ctx, req, done)
	return nil
}

// Stop requests cancellation and waits for the worker to exit. In-flight
// downloads are cancelled through DownloadContext, so this normally returns
// quickly.
func (s *Syncer) Stop() {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return
	}
	cancel, done := s.cancel, s.done
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

// Running reports whether a sync goroutine is active.
func (s *Syncer) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// AutoLoop runs incremental syncs every Settings().AutoMinutes until ctx is
// done. It starts the first sync immediately when AutoMinutes > 0, re-reads
// settings periodically (so enabling/disabling takes effect without a
// restart), and never starts a second run while one is in flight.
func (s *Syncer) AutoLoop(ctx context.Context) {
	var last time.Time
	var retryAt time.Time
	var failures int
	for {
		settings := s.st.Settings()
		if settings.AutoMinutes <= 0 {
			last = time.Time{}
			retryAt = time.Time{}
			failures = 0
		} else {
			interval := time.Duration(settings.AutoMinutes) * time.Minute
			if (last.IsZero() || time.Since(last) >= interval) &&
				!time.Now().Before(retryAt) && !s.Running() {
				err := s.Start(SyncRequest{})
				switch {
				case err == nil:
					failures = 0
					retryAt = time.Time{}
					last = time.Now().Truncate(time.Second)
					s.st.Log("info", "scan", fmt.Sprintf("自动同步已启动（间隔 %d 分钟）", settings.AutoMinutes))
				case errors.Is(err, ErrAlreadyRunning):
					// another run is finishing; retry on the next poll
				default:
					// A misconfigured NVR must not produce a log line every
					// 15 seconds forever. Back off up to 10 minutes.
					failures++
					backoff := 15 * time.Second
					for i := 1; i < failures && i < 6; i++ {
						backoff *= 2
					}
					if backoff > 10*time.Minute {
						backoff = 10 * time.Minute
					}
					retryAt = time.Now().Add(backoff)
					if failures == 1 || failures%10 == 0 {
						s.st.Log("error", "scan", fmt.Sprintf("自动同步启动失败（第 %d 次，%s 后重试）: %v",
							failures, backoff.Round(time.Second), err))
					}
				}
			}
		}

		wait := autoPoll
		if settings.AutoMinutes > 0 && !last.IsZero() {
			remain := time.Duration(settings.AutoMinutes)*time.Minute - time.Since(last)
			if remain < time.Second {
				remain = time.Second
			}
			if remain < wait {
				wait = remain
			}
		}
		if !retryAt.IsZero() {
			remain := time.Until(retryAt)
			if remain < time.Second {
				remain = time.Second
			}
			if remain < wait {
				wait = remain
			}
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
