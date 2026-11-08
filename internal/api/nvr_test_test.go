package api

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/myml/nvrsync/internal/store"
	"github.com/myml/nvrsync/internal/syncer"
)

// nvrProbe is a fake NVR management API that counts concurrent requests and
// lets the test hold the sync request open. A "test connection" that ignores
// the global session lock is answered while the sync is still on the wire, so
// it shows up as a second in-flight request (peak 2) — the -40401 session
// contention the lock exists for.
type nvrProbe struct {
	mu        sync.Mutex
	inFlight  int
	peak      int
	requests  int
	mediaList int

	// arrived is signalled when a media list request is being handled; the
	// handler then blocks until the test calls releaseMedia (or closes it).
	arrived      chan struct{}
	releaseMedia chan struct{}
	releaseOnce  sync.Once
}

// release lets every blocked media list answer. It is safe to call twice.
func (p *nvrProbe) release() { p.releaseOnce.Do(func() { close(p.releaseMedia) }) }

func (p *nvrProbe) enter(list bool) {
	p.mu.Lock()
	p.inFlight++
	p.requests++
	if list {
		p.mediaList++
	}
	if p.inFlight > p.peak {
		p.peak = p.inFlight
	}
	p.mu.Unlock()
}

func (p *nvrProbe) leave() {
	p.mu.Lock()
	p.inFlight--
	p.mu.Unlock()
}

func (p *nvrProbe) stats() (peak, requests, mediaList int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.peak, p.requests, p.mediaList
}

// newProbeNVR answers the management API. Media list queries block while
// armed is true, so the test controls how long the "sync" occupies the device.
func newProbeNVR(t *testing.T) (*nvrProbe, string, int) {
	t.Helper()
	p := &nvrProbe{arrived: make(chan struct{}, 8), releaseMedia: make(chan struct{})}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			System json.RawMessage `json:"system"`
		}
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req)
		isList := r.URL.Path != "/" && len(req.System) == 0
		p.enter(isList)
		defer p.leave()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/":
			io.WriteString(w, `{"error_code":0,"stok":"tok"}`)
		case len(req.System) > 0:
			io.WriteString(w, `{"error_code":0,"user_id":9}`)
		default:
			select {
			case p.arrived <- struct{}{}:
			default:
			}
			<-p.releaseMedia
			io.WriteString(w, `{"error_code":0,"media":{"start_time":[],"end_time":[],"size":[],"event_type":[],"media_type":[],"channel":[]}}`)
		}
	}))
	t.Cleanup(srv.Close)
	host, portStr, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split host: %v", err)
	}
	port, _ := strconv.Atoi(portStr)
	return p, host, port
}

// TestNVRTestWaitsForSessionLock is the regression test for /api/nvr/test
// bypassing the NVR session lock. It sets up the exact shape of the race:
//
//   - a sync (simulated by a login while holding the lock, then a media list
//     that the fake NVR keeps open) occupies the session;
//   - the "test connection" request is fired from inside that lock window, so
//     an unlocked handler has its login and media list served right away and
//     the device sees two sessions at once (peak 2).
//
// With the fix the probe queues on the lock: while the sync holds the session
// the device sees exactly one media list (the sync's own), and the test
// connection's listing only arrives afterwards.
func TestNVRTestWaitsForSessionLock(t *testing.T) {
	p, host, port := newProbeNVR(t)
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sy := syncer.New(st)
	set := st.Settings()
	set.NVRHost = host
	set.NVRUser = "admin"
	set.NVRPass = "secret"
	set.AdminPort = port
	set.StorageDir = filepath.Join(t.TempDir(), "storage")
	set.Channels = []int{1}
	if err := st.SaveSettings(set); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	a := &testAPI{t: t, st: st, h: New(st, sy, nil, nil, "test")}

	// handlerDone is created up front so both the sync goroutine and this one
	// can use it without a data race on the variable itself.
	handlerDone := make(chan *httptest.ResponseRecorder, 1)
	releaseSync := make(chan struct{})
	holdErr := make(chan error, 1)
	// A failing assertion must not leave the fake NVR blocking or the session
	// lock held: that would deadlock the remaining goroutines and hang the
	// test binary instead of reporting the failure.
	var releaseSyncOnce sync.Once
	endSync := func() { releaseSyncOnce.Do(func() { close(releaseSync) }) }
	t.Cleanup(func() {
		endSync()
		p.release()
	})

	// Simulate the sync owner: take the session, log in, keep a media list on
	// the wire and hold the lock for the whole window in which the HTTP
	// handler under test runs.
	go func() {
		holdErr <- sy.WithNVRSession(context.Background(), "sync", func() error {
			// A real sync logs in first; that request must not be mistaken
			// for the signal that the handler reached the device.
			req, _ := http.NewRequest(http.MethodPost, "http://"+net.JoinHostPort(host, strconv.Itoa(port))+"/", nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return err
			}
			resp.Body.Close()
			// The fake NVR blocks every media list; this one stands for the
			// sync's own listing and is on the wire until releaseSync.
			go func() { _, _ = nvrMediaList(host, port) }()
			// The "测试连接" click happens while the sync owns the session.
			go func() { handlerDone <- a.do("POST", "/api/nvr/test", nil) }()
			<-releaseSync
			return nil
		})
	}()

	// Wait until the sync's media list is being handled: from here on the
	// device is busy and the handler is either queued (correct) or already
	// knocking (bug).
	select {
	case <-p.arrived:
	case <-time.After(2 * time.Second):
		t.Fatal("sync never reached the fake NVR")
	}
	select {
	case rec := <-handlerDone:
		t.Fatalf("handleNVRTest answered %d while the sync held the session: %s",
			rec.Code, rec.Body.String())
	case <-time.After(150 * time.Millisecond):
	}
	_, _, held := p.stats()
	if held != 1 {
		t.Fatalf("device handled %d media lists while the sync held the session, want only the sync's own", held)
	}

	// End the sync's lock window; only now may the queued handler talk to the
	// device and answer.
	endSync()
	if err := <-holdErr; err != nil {
		t.Fatalf("WithNVRSession: %v", err)
	}
	p.release()
	select {
	case rec := <-handlerDone:
		if rec.Code != http.StatusOK {
			t.Fatalf("test connection = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		body := decodeBody(t, rec)
		data, _ := body["data"].(map[string]any)
		if data == nil || data["login"] != true {
			t.Errorf("test connection data = %v, want login=true", body["data"])
		}
	case <-time.After(3 * time.Second):
		t.Fatal("queued test connection never completed")
	}

	// The two media lists never overlapped: an unlocked handler would have had
	// its login and media list served on top of the sync's, which the real
	// device answers with -40401.
	peak, _, mediaList := p.stats()
	if peak > 1 {
		t.Errorf("peak concurrent NVR requests = %d, want 1 (test connection overlapped the sync)", peak)
	}
	if mediaList != 2 {
		t.Errorf("device handled %d media lists in total, want 2 (sync + test)", mediaList)
	}
}

// nvrMediaList is a stand-in for the sync's media list request.
func nvrMediaList(host string, port int) ([]byte, error) {
	req, _ := http.NewRequest(http.MethodPost, "http://"+net.JoinHostPort(host, strconv.Itoa(port))+"/stok=tok/ds", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}
