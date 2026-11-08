package nvr

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// busyNVR always answers get_user_id with -40401 (no session budget), which is
// the answer that used to make callers sleep through a 1s+2s ladder.
func newBusyNVR(t *testing.T) (*Client, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			System json.RawMessage `json:"system"`
		}
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		if len(req.System) > 0 {
			calls.Add(1)
			io.WriteString(w, `{"error_code":-40401}`)
			return
		}
		io.WriteString(w, `{"error_code":0,"stok":"tok"}`)
	}))
	t.Cleanup(srv.Close)
	host, portStr, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split host: %v", err)
	}
	port, _ := strconv.Atoi(portStr)
	c := New(host+":8000", "admin", "secret", port)
	return c, &calls
}

// TestGetUserIDContextCancelsBackoff proves the retry ladder is cancellable:
// cancelling during the backoff must return promptly instead of sleeping 1s+2s.
func TestGetUserIDContextCancelsBackoff(t *testing.T) {
	c, calls := newBusyNVR(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		// Cancel once the first attempt has been made and the backoff started.
		for calls.Load() == 0 {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()

	start := time.Now()
	_, err := c.GetUserIDContext(ctx)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("GetUserIDContext returned nil error for a busy NVR")
	}
	// The first backoff alone is 1s; cancelling must beat it comfortably.
	if elapsed > 400*time.Millisecond {
		t.Errorf("cancellation took %v, want it to abort the backoff promptly", elapsed)
	}
	if n := calls.Load(); n > 2 {
		t.Errorf("get_user_id attempted %d times after cancel, want at most 2", n)
	}
}

// TestGetUserIDContextRetriesBusy keeps the documented behaviour: a busy NVR is
// retried (it is transient), and the error mentions the device code.
func TestGetUserIDContextRetriesBusy(t *testing.T) {
	if testing.Short() {
		t.Skip("exercises the real 1s+2s backoff")
	}
	c, calls := newBusyNVR(t)
	_, err := c.GetUserIDContext(context.Background())
	if err == nil {
		t.Fatal("expected an error from an always-busy NVR")
	}
	if !strings.Contains(err.Error(), "-40401") {
		t.Errorf("error %q should surface the device code", err)
	}
	if n := calls.Load(); n != nvrUserIDAttempts {
		t.Errorf("attempts = %d, want %d", n, nvrUserIDAttempts)
	}
}
