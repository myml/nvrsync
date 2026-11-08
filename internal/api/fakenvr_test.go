package api

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"
)

// fakeAPINVR is a minimal TP-Link style NVR: /ds answers login and media list
// requests, and a raw TCP listener serves two TS packets per download. It is
// the API-package twin of syncer_test.go's fakeNVR; the two packages cannot
// share test helpers.
type fakeAPINVR struct {
	srv  *httptest.Server
	ln   net.Listener
	segs [][2]int64
	busy bool

	mu       sync.Mutex
	listings int
}

// newFakeAPINVR starts the fake device; busy makes every media list answer the
// NVR's -40401 "out of sessions" error.
func newFakeAPINVR(t *testing.T, segs [][2]int64) *fakeAPINVR {
	t.Helper()
	f := &fakeAPINVR{segs: segs}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handleDS))
	t.Cleanup(f.srv.Close)
	var err error
	f.ln, err = net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = f.ln.Close() })
	go f.acceptStreams()
	return f
}

func (f *fakeAPINVR) host() string { return f.ln.Addr().String() }

func (f *fakeAPINVR) adminPort() int {
	_, p, _ := net.SplitHostPort(f.srv.Listener.Addr().String())
	n, _ := strconv.Atoi(p)
	return n
}

func (f *fakeAPINVR) handleDS(w http.ResponseWriter, r *http.Request) {
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
		f.mu.Lock()
		f.listings++
		busy := f.busy
		f.mu.Unlock()
		if busy {
			io.WriteString(w, `{"error_code":-40401,"err_msg":"busy"}`)
			return
		}
		var st, et, sz, ev []string
		for _, s := range f.segs {
			st = append(st, strconv.FormatInt(s[0], 10))
			et = append(et, strconv.FormatInt(s[1], 10))
			sz = append(sz, "376")
			ev = append(ev, "1")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error_code": 0,
			"media": map[string]any{
				"start_time": st, "end_time": et, "size": sz,
				"event_type": ev, "media_type": []string{"0"},
				"channel": req.Media.GetMediaList.Channel,
			},
		})
	}
}

func (f *fakeAPINVR) acceptStreams() {
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.serveStream(conn)
	}
}

func (f *fakeAPINVR) serveStream(conn net.Conn) {
	defer conn.Close()
	br := bufio.NewReader(conn)
	if _, err := readAPILines(br); err != nil {
		return
	}
	fmt.Fprint(conn, "HTTP/1.1 401 Unauthorized\r\nWWW-Authenticate: Digest realm=\"TP-Link IP-Camera\", nonce=\"n\", opaque=\"o\"\r\nContent-Length: 0\r\n\r\n")
	if _, err := readAPILines(br); err != nil {
		return
	}
	fmt.Fprint(conn, "HTTP/1.0 200 OK\r\nContent-Type: multipart/mixed;boundary=--device--\r\n\r\n")
	writePart := func(ct string, body []byte) {
		fmt.Fprintf(conn, "----device--\r\nContent-Type: %s\r\nContent-Length: %d\r\n\r\n", ct, len(body))
		_, _ = conn.Write(body)
		io.WriteString(conn, "\r\n")
	}
	writePart("application/json", []byte(`{"params":{"session_id":"s1"}}`))
	writePart("video/mp2t", append(apiTSPacket(), apiTSPacket()...))
	writePart("application/json", []byte(`{"params":{"stream_status":"finished"}}`))
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	_, _ = io.Copy(io.Discard, conn)
}

func readAPILines(br *bufio.Reader) ([]string, error) {
	var lines []string
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = trimCRLF(line)
		if line == "" {
			return lines, nil
		}
		lines = append(lines, line)
	}
}

func trimCRLF(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

// apiTSPacket is one valid 188-byte MPEG-TS null packet.
func apiTSPacket() []byte {
	p := make([]byte, 188)
	p[0] = 0x47
	p[1] = 0x1f
	p[2] = 0xff
	p[3] = 0x10
	return p
}
