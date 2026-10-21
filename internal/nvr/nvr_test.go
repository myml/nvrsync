package nvr

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
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

// fakeNVR is a minimal /ds implementation: login, get_user_id and
// get_media_list with the same column arrays as the real device.
type fakeNVR struct {
	srv     *httptest.Server
	segs    []MediaSeg
	noMedia bool

	gotChannel atomic.Value // string
	gotEvents  atomic.Value // []int
	pages      atomic.Int64
}

func newFakeNVR(t *testing.T, segs []MediaSeg) *fakeNVR {
	t.Helper()
	f := &fakeNVR{segs: segs}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeNVR) handle(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Method string          `json:"method"`
		Login  json.RawMessage `json:"login"`
		System json.RawMessage `json:"system"`
		Media  struct {
			GetMediaList struct {
				Channel    []string `json:"channel"`
				StartTime  string   `json:"start_time"`
				EndTime    string   `json:"end_time"`
				EventType  []int    `json:"event_type"`
				StartIndex int      `json:"start_index"`
				MaxNum     int      `json:"max_num"`
				UserID     int      `json:"user_id"`
			} `json:"get_media_list"`
		} `json:"media"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/":
		io.WriteString(w, `{"error_code":0,"stok":"tok123"}`)
	case len(req.System) > 0:
		io.WriteString(w, `{"error_code":0,"user_id":7}`)
	case len(req.Media.GetMediaList.Channel) > 0 || len(req.Media.GetMediaList.StartTime) > 0:
		q := req.Media.GetMediaList
		if len(q.Channel) > 0 {
			f.gotChannel.Store(q.Channel[0])
		}
		f.gotEvents.Store(q.EventType)
		from, _ := strconv.ParseInt(q.StartTime, 10, 64)
		if f.noMedia || (from > 0 && from < 1000) {
			io.WriteString(w, `{"error_code":-71104,"media":{}}`)
			return
		}
		f.pages.Add(1)
		n := q.MaxNum
		if n <= 0 {
			n = 100
		}
		start := q.StartIndex
		if start > len(f.segs) {
			start = len(f.segs)
		}
		end := start + n
		if end > len(f.segs) {
			end = len(f.segs)
		}
		var st, et, sz, ev, mt, ch []string
		for _, s := range f.segs[start:end] {
			st = append(st, strconv.FormatInt(s.Start, 10))
			et = append(et, strconv.FormatInt(s.End, 10))
			sz = append(sz, strconv.FormatInt(s.Size, 10))
			ev = append(ev, strconv.Itoa(s.EventType))
			mt = append(mt, "0")
			ch = append(ch, "0")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error_code": 0,
			"media": map[string]any{
				"start_time": st, "end_time": et, "size": sz,
				"event_type": ev, "media_type": mt, "channel": ch,
			},
		})
	default:
		io.WriteString(w, `{"error_code":0}`)
	}
}

func (f *fakeNVR) port() int {
	_, p, _ := net.SplitHostPort(f.srv.Listener.Addr().String())
	n, _ := strconv.Atoi(p)
	return n
}

func TestMediaListPagination(t *testing.T) {
	const base = 1792000000
	var want []MediaSeg
	for i := 0; i < 250; i++ {
		want = append(want, MediaSeg{
			Start:     base + int64(i*60),
			End:       base + int64(i*60+60),
			Size:      int64(1_000_000 + i),
			EventType: []int{1, 2, 21}[i%3],
		})
	}
	f := newFakeNVR(t, want)
	c := New(f.srv.Listener.Addr().String(), "admin", "secret", f.port())
	if err := c.Login(); err != nil {
		t.Fatalf("Login: %v", err)
	}
	got, err := c.MediaList(1, base, base+250*60)
	if err != nil {
		t.Fatalf("MediaList: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("segments = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("segment[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	if got := f.pages.Load(); got != 3 {
		t.Fatalf("pages fetched = %d, want 3", got)
	}
	if ch := f.gotChannel.Load(); ch != "0" { // physical 1 -> API 0
		t.Fatalf("channel sent = %v, want \"0\"", ch)
	}
	ev, _ := f.gotEvents.Load().([]int)
	if len(ev) != 3 || ev[0] != 1 || ev[1] != 2 || ev[2] != 21 {
		t.Fatalf("event_type sent = %v, want [1 2 21]", ev)
	}

	// The device's -71104 must be reported as ErrNoMedia, not a transport
	// error.
	if _, err := c.MediaList(1, 1, 2); !errors.Is(err, ErrNoMedia) {
		t.Fatalf("MediaList old range error = %v, want ErrNoMedia", err)
	}
}

// startFakeStream serves one download connection: 401, then a multipart
// response with a JSON session part, two video parts and a finished part.
func startFakeStream(t *testing.T, parts [][]byte) (addr string, gotAuth *atomic.Bool) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	gotAuth = &atomic.Bool{}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		br := bufio.NewReader(conn)

		// First request: no Authorization -> 401.
		if _, err := readHTTPHead(br); err != nil {
			return
		}
		fmt.Fprint(conn, "HTTP/1.1 401 Unauthorized\r\nWWW-Authenticate: Digest realm=\"TP-Link IP-Camera\", nonce=\"abc\", opaque=\"xyz\"\r\nContent-Length: 0\r\n\r\n")

		// Second request: headers (the multipart body follows, but the fake
		// device does not need to read it).
		head, err := readHTTPHead(br)
		if err != nil {
			return
		}
		if strings.Contains(strings.Join(head, "\n"), "Authorization: Digest") {
			gotAuth.Store(true)
		}
		fmt.Fprint(conn, "HTTP/1.0 200 OK\r\nContent-Type: multipart/mixed;boundary=--device-stream-boundary--\r\n\r\n")
		writePart := func(ct string, body []byte) {
			fmt.Fprintf(conn, "----device-stream-boundary--\r\nContent-Type: %s\r\nX-Session-Id: s1\r\nContent-Length: %d\r\n\r\n", ct, len(body))
			conn.Write(body)
			io.WriteString(conn, "\r\n")
		}
		writePart("application/json", []byte(`{"type":"notification","seq":0,"params":{"session_id":"s1"}}`))
		for _, p := range parts {
			writePart("video/mp2t", p)
		}
		writePart("application/json", []byte(`{"type":"notification","seq":1,"params":{"stream_status":"finished"}}`))
		// Drain the client's ACKs / stop notification until it closes.
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		io.Copy(io.Discard, conn)
	}()
	return ln.Addr().String(), gotAuth
}

// tsPacket builds one 188-byte TS packet carrying a PES header with pts.
func tsPacket(pts uint64) []byte {
	pes := []byte{0x00, 0x00, 0x01, 0xE0, 0x00, 0x00, 0x80, 0x80, 0x05,
		byte((pts >> 29) & 0x0E),
		byte(pts >> 22),
		byte((pts>>14)&0xFE) | 0x01,
		byte(pts >> 7),
		byte(pts<<1&0xFE) | 0x01,
	}
	// Add the PTS prefix/marker bits expected by the standard encoding.
	pes[9] |= 0x21
	pes[11] |= 0x01
	pes[13] |= 0x01
	pkt := make([]byte, tsPacketSize)
	pkt[0] = 0x47
	pkt[1] = 0x41 // PUSI, PID 0x0100
	pkt[2] = 0x00
	pkt[3] = 0x10 // payload only
	copy(pkt[4:], pes)
	for i := 4 + len(pes); i < len(pkt); i++ {
		pkt[i] = 0xFF
	}
	return pkt
}

func TestDownloadParsesMultipart(t *testing.T) {
	f := newFakeNVR(t, nil)
	streamAddr, gotAuth := startFakeStream(t, [][]byte{tsPacket(90000), tsPacket(180000)})

	c := New(streamAddr, "admin", "secret", f.port())
	if err := c.Login(); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if _, err := c.GetUserID(); err != nil {
		t.Fatalf("GetUserID: %v", err)
	}

	var out bytes.Buffer
	stats, err := c.Download(1, 1000, 1060, &out, DownloadOpt{Idle: 3 * time.Second})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if !gotAuth.Load() {
		t.Fatal("second request carried no Digest Authorization header")
	}
	if stats.Bytes != 2*tsPacketSize || out.Len() != int(stats.Bytes) {
		t.Fatalf("bytes = %d (writer %d), want %d", stats.Bytes, out.Len(), 2*tsPacketSize)
	}
	if stats.Packets != 2 {
		t.Fatalf("packets = %d, want 2", stats.Packets)
	}
	if stats.PTSStart != 90000 || stats.PTSEnd != 180000 {
		t.Fatalf("PTS range = %d..%d, want 90000..180000", stats.PTSStart, stats.PTSEnd)
	}
	// Output must be untouched raw TS.
	if out.Bytes()[0] != 0x47 || out.Bytes()[tsPacketSize] != 0x47 {
		t.Fatal("output is not raw TS with 0x47 sync bytes")
	}
}

func TestDownloadTruncated(t *testing.T) {
	f := newFakeNVR(t, nil)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		readHTTPHead(br)
		fmt.Fprint(conn, "HTTP/1.1 401 Unauthorized\r\nWWW-Authenticate: Digest realm=\"x\", nonce=\"n\", opaque=\"o\"\r\nContent-Length: 0\r\n\r\n")
		readHTTPHead(br)
		fmt.Fprint(conn, "HTTP/1.0 200 OK\r\n\r\n")
		fmt.Fprintf(conn, "----b--\r\nContent-Type: video/mp2t\r\nContent-Length: %d\r\n\r\n", tsPacketSize)
		conn.Write(tsPacket(90000))
		// No more parts: the client must hit the idle timeout and report a
		// truncated transfer rather than a successful single packet.
		time.Sleep(500 * time.Millisecond)
	}()
	c := New(ln.Addr().String(), "admin", "secret", f.port())
	if err := c.Login(); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	_, err = c.Download(1, 1000, 1060, &out, DownloadOpt{Idle: 100 * time.Millisecond})
	if !errors.Is(err, ErrTruncated) {
		t.Fatalf("Download error = %v, want ErrTruncated", err)
	}
}
