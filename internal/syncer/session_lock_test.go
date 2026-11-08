package syncer

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/myml/nvrsync/internal/nvr"
	"github.com/myml/nvrsync/internal/store"
)

// concurrencyProbe is a fake NVR that records how many requests it is handling
// at once. Overlapping sync + catalogue work would show up as a peak above 1,
// which is exactly the -40401 session contention the global lock prevents.
type concurrencyProbe struct {
	mu        sync.Mutex
	inFlight  int
	peak      int
	requests  int
	holdFor   time.Duration
	mediaList int
}

func (p *concurrencyProbe) enter() {
	p.mu.Lock()
	p.inFlight++
	p.requests++
	if p.inFlight > p.peak {
		p.peak = p.inFlight
	}
	p.mu.Unlock()
}

func (p *concurrencyProbe) leave() {
	p.mu.Lock()
	p.inFlight--
	p.mu.Unlock()
}

func (p *concurrencyProbe) stats() (peak, requests, listings int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.peak, p.requests, p.mediaList
}

// newProbeNVR starts a management API (and stream listener) that answers every
// request after holdFor, so two concurrent clients are guaranteed to overlap.
func newProbeNVR(t *testing.T, holdFor time.Duration) (*fakeNVR, *concurrencyProbe) {
	t.Helper()
	probe := &concurrencyProbe{holdFor: holdFor}
	f := &fakeNVR{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probe.enter()
		defer probe.leave()
		time.Sleep(holdFor)
		handleProbeDS(probe, w, r)
	}))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f.ln = ln
	go f.acceptStreams()
	t.Cleanup(func() {
		f.srv.Close()
		ln.Close()
	})
	return f, probe
}

func handleProbeDS(probe *concurrencyProbe, w http.ResponseWriter, r *http.Request) {
	var req struct {
		System json.RawMessage `json:"system"`
		Media  struct {
			GetMediaList struct {
				Channel []string `json:"channel"`
			} `json:"get_media_list"`
		} `json:"media"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/":
		io.WriteString(w, `{"error_code":0,"stok":"tok"}`)
	case len(req.System) > 0:
		io.WriteString(w, `{"error_code":0,"user_id":9}`)
	default:
		probe.mu.Lock()
		probe.mediaList++
		probe.mu.Unlock()
		io.WriteString(w, `{"error_code":0,"media":{"start_time":[],"end_time":[],"size":[],"event_type":[],"media_type":[],"channel":[]}}`)
	}
}

// TestSyncAndCatalogNeverOverlap is the regression test for the NVR session
// contention: a sync and a catalogue refresh started together must not hit the
// device at the same time.
func TestSyncAndCatalogNeverOverlap(t *testing.T) {
	f, probe := newProbeNVR(t, 40*time.Millisecond)
	st := openSyncerStore(t)
	now := time.Now().Unix()
	storage := filepath.Join(t.TempDir(), "storage")
	if err := st.SaveSettings(store.Settings{
		NVRHost: f.host(), NVRUser: "admin", NVRPass: "secret", AdminPort: f.adminPort(),
		StorageDir: storage, Channels: []int{1}, CatalogDays: 1,
	}); err != nil {
		t.Fatalf("save settings: %v", err)
	}
	if err := st.UpsertSegment(store.Segment{
		Ch: 1, Start: now - 900, End: now - 780, Status: "pending",
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	s := New(st)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := s.Start(SyncRequest{}); err != nil {
			t.Errorf("Start: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if _, err := s.RefreshCatalogForce(context.Background(), true); err != nil {
			t.Errorf("RefreshCatalog: %v", err)
		}
	}()
	wg.Wait()
	waitDone(t, s, st, 1)

	peak, requests, _ := probe.stats()
	if peak > 1 {
		t.Errorf("peak concurrent NVR requests = %d, want 1 (sync and catalogue overlapped)", peak)
	}
	if requests < 2 {
		t.Errorf("NVR saw %d requests, want both activities to have run", requests)
	}
}

// TestNVRSessionLockIsExclusive pins acquireNVR itself: a second owner blocks
// until the first releases, and a cancelled waiter gives up.
func TestNVRSessionLockIsExclusive(t *testing.T) {
	st := openSyncerStore(t)
	s := New(st)

	release, err := s.acquireNVR(context.Background(), "sync")
	if err != nil {
		t.Fatalf("acquireNVR: %v", err)
	}
	if got := s.SessionHolder(); got != "sync" {
		t.Errorf("SessionHolder() = %q, want sync", got)
	}

	// A second acquisition must block while the first is held.
	got := make(chan string, 1)
	go func() {
		rel, err := s.acquireNVR(context.Background(), "catalog")
		if err != nil {
			got <- "error: " + err.Error()
			return
		}
		defer rel()
		got <- s.SessionHolder()
	}()
	select {
	case who := <-got:
		t.Fatalf("second acquirer did not wait (got %q)", who)
	case <-time.After(120 * time.Millisecond):
	}

	release()
	select {
	case who := <-got:
		if who != "catalog" {
			t.Fatalf("second acquirer holder = %q, want catalog", who)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second acquirer never obtained the session after release")
	}
	// Release is idempotent and leaves the device free.
	if got := s.SessionHolder(); got != "" {
		t.Errorf("SessionHolder() after release = %q, want empty", got)
	}
}

// TestNVRSessionLockHonoursCancel makes sure Stop() does not hang when a run is
// queued behind another NVR activity.
func TestNVRSessionLockHonoursCancel(t *testing.T) {
	st := openSyncerStore(t)
	s := New(st)
	release, err := s.acquireNVR(context.Background(), "catalog")
	if err != nil {
		t.Fatalf("acquireNVR: %v", err)
	}
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := s.acquireNVR(ctx, "sync")
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled waiter returned nil error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled waiter did not return")
	}
}

// TestWithNVRSessionQueuesBehindHolder pins the public lock wrapper the API
// uses for "test connection": fn must not run while another activity owns the
// session, and the holder must be reported as "test" meanwhile.
func TestWithNVRSessionQueuesBehindHolder(t *testing.T) {
	st := openSyncerStore(t)
	s := New(st)
	release, err := s.acquireNVR(context.Background(), "sync")
	if err != nil {
		t.Fatalf("acquireNVR: %v", err)
	}

	ran := make(chan string, 1)
	go func() {
		err := s.WithNVRSession(context.Background(), "test", func() error {
			ran <- s.SessionHolder()
			return nil
		})
		if err != nil {
			ran <- "error: " + err.Error()
		}
	}()
	select {
	case got := <-ran:
		t.Fatalf("WithNVRSession ran fn while the session was held (holder %q)", got)
	case <-time.After(120 * time.Millisecond):
	}

	release()
	select {
	case got := <-ran:
		if got != "test" {
			t.Fatalf("fn ran with holder %q, want test", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WithNVRSession never ran fn after release")
	}
	// The lock must be free again, and a callback error must be propagated.
	if got := s.SessionHolder(); got != "" {
		t.Errorf("SessionHolder() after WithNVRSession = %q, want empty", got)
	}
	sentinel := errors.New("boom")
	if err := s.WithNVRSession(context.Background(), "test", func() error { return sentinel }); !errors.Is(err, sentinel) {
		t.Errorf("WithNVRSession error = %v, want %v", err, sentinel)
	}
	if got := s.SessionHolder(); got != "" {
		t.Errorf("SessionHolder() after failing fn = %q, want empty (lock leaked)", got)
	}
}

// TestSyncWithoutQueueDoesNotTakeSession verifies the optimisation that keeps a
// no-op run (nothing pending) off the device entirely.
func TestSyncWithoutQueueDoesNotTakeSession(t *testing.T) {
	f := newFakeNVR(t, [][2]int64{{0, 0}})
	st := openSyncerStore(t)
	if err := st.SaveSettings(store.Settings{
		NVRHost: f.host(), NVRUser: "admin", NVRPass: "secret", AdminPort: f.adminPort(),
		StorageDir: t.TempDir(), Channels: []int{1},
	}); err != nil {
		t.Fatalf("save settings: %v", err)
	}
	s := New(st)
	if err := s.Start(SyncRequest{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for s.Running() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if s.Running() {
		t.Fatal("idle sync did not finish")
	}
	f.mu.Lock()
	listings := f.listings
	f.mu.Unlock()
	if listings != 0 {
		t.Errorf("idle sync queried the media list %d times, want 0", listings)
	}
	if !logContains(st, "没有需要下载的录像段") {
		t.Error("expected the 'nothing to download' log line")
	}
}

// TestLoginStopsOnCredentialError checks the P1 login fix: a definitive auth
// rejection must not be retried on every other port.
func TestLoginStopsOnCredentialError(t *testing.T) {
	var hits int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"error_code":-40224}`)
	}))
	defer srv.Close()
	host, portStr, _ := net.SplitHostPort(srv.Listener.Addr().String())
	port, _ := strconv.Atoi(portStr)
	client := nvr.New(host+":8000", "admin", "wrong", port)
	if err := client.Login(); err == nil {
		t.Fatal("Login succeeded with a rejected credential")
	}
	mu.Lock()
	got := hits
	mu.Unlock()
	if got != 1 {
		t.Errorf("login attempted %d ports, want 1 (stop on credential error)", got)
	}
}

func logContains(st *store.Store, want string) bool {
	for _, entry := range st.Logs(0, "", 0) {
		if strings.Contains(entry.Msg, want) {
			return true
		}
	}
	return false
}
