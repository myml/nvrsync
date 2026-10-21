package nvr

// Recording download over the port-8000 stream server.
//
// Protocol differences from live preview (verified byte-for-byte on
// TL-NVR6108C-L / 1.0.26, see nvr2rtc/DLFILE.md):
//
//  1. path must be /stream/download, and the Digest uri must match it;
//  2. event_type must cover the segment kinds: ["1","2","21"];
//  3. credit flow control (X-Data-Received) decides throughput. The device
//     stops after each 50-part window, so ACKs are sent by a separate
//     goroutine. The default (ACKInterval=0, ACKEveryBytes=0) ACKs after
//     every video part, which reaches the 100 Mbit link limit (45-52x
//     realtime) instead of the App's ~1 Mbit 2-second cadence;
//  4. parts must be read by their Content-Length. The final JSON part may
//     have no trailing boundary and may span TCP segments, so scanning for
//     boundaries would lose the "finished" notification.
//
// Output is the raw TS stream (188-byte packets), not remultiplexed.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	downloadPath  = "/stream/download"
	tsPacketSize  = 188
	ackStep       = 25
	defaultIdle   = 20 * time.Second
	maxPartBytes  = 64 << 20
	prepaidCredit = 250
)

// ErrTruncated means the device closed the stream (or idled out) without a
// "finished" status. Bytes already written by the caller must be discarded.
var ErrTruncated = errors.New("nvr: download truncated")

// ErrInvalidTS means the downloaded bytes are not a clean 188-byte TS
// stream (bad sync byte, partial packet, or no packets at all).
var ErrInvalidTS = errors.New("nvr: download is not a valid transport stream")

// DownloadOpt controls flow-control pacing and timeouts.
//
//   - ACKInterval == 0 and ACKEveryBytes == 0 (the default): ACK every
//     video part, i.e. as fast as data arrives. This is the fastest mode.
//   - ACKInterval > 0: send an ACK at least this often.
//   - ACKEveryBytes > 0: send an ACK after this many TS bytes.
//
// Idle defaults to 20s and is the maximum time to wait for the next part.
type DownloadOpt struct {
	ACKInterval   time.Duration
	ACKEveryBytes int
	Idle          time.Duration
}

// Stats summarises a download. PTSStart/PTSEnd are the min/max PES
// presentation timestamps in 90 kHz units; Packets counts 188-byte TS
// packets actually written.
type Stats struct {
	Bytes    int64 `json:"bytes"`
	Packets  int64 `json:"packets"`
	PTSStart int64 `json:"pts_start"`
	PTSEnd   int64 `json:"pts_end"`
}

// Download fetches raw TS for physical channel ch (1-based) in [from, to]
// and writes it to w. On error the caller should discard whatever was
// written (the syncer downloads into a temp file and removes it).
func (c *Client) Download(ch int, from, to int64, w io.Writer, opt DownloadOpt) (Stats, error) {
	return c.DownloadContext(context.Background(), ch, from, to, w, opt)
}

// DownloadContext is Download with cancellation. The syncer uses it so Stop
// can abort an in-flight transfer without waiting for the idle timeout.
func (c *Client) DownloadContext(ctx context.Context, ch int, from, to int64, w io.Writer, opt DownloadOpt) (stats Stats, err error) {
	if w == nil {
		return stats, errors.New("nvr: download: nil writer")
	}
	if to <= from {
		return stats, fmt.Errorf("nvr: download: end %d must be greater than start %d", to, from)
	}
	if opt.Idle <= 0 {
		opt.Idle = defaultIdle
	}
	clientID, err := c.GetUserID()
	if err != nil {
		return stats, err
	}
	apiCh := ch - 1
	if apiCh < 0 {
		apiCh = 0
	}

	req := map[string]any{
		"type": "request", "seq": 0,
		"params": map[string]any{"method": "get", "download": map[string]any{
			"channels":     []int{apiCh},
			"privary_auth": []int{apiCh},
			"client_id":    clientID,
			"media_type":   0,
			"start_time":   fmt.Sprintf("%d", from),
			"end_time":     fmt.Sprintf("%d", to),
			"event_type":   []string{"1", "2", "21"},
		}},
	}
	reqJSON, err := json.Marshal(req)
	if err != nil {
		return stats, err
	}
	sconn, err := c.dialStream(ctx, downloadPath, reqJSON)
	if err != nil {
		return stats, err
	}
	defer sconn.conn.Close()
	conn := sconn.conn
	br := sconn.br

	probe := newTSProbe()
	acker := newDownloadAcker(conn)
	defer func() {
		stats.Packets = probe.packets
		if probe.havePTS {
			stats.PTSStart = int64(probe.ptsMin)
			stats.PTSEnd = int64(probe.ptsMax)
		}
	}()

	// ACK goroutine: independent from the read loop so a server that filled
	// its window and stopped sending still gets credit.
	ackDone := make(chan struct{})
	ackWake := make(chan struct{}, 1)
	var ackPending atomic.Int64
	var ackWG sync.WaitGroup
	ackWG.Add(1)
	go func() {
		defer ackWG.Done()
		var tick <-chan time.Time
		if opt.ACKInterval > 0 {
			t := time.NewTicker(opt.ACKInterval)
			defer t.Stop()
			tick = t.C
		}
		for {
			select {
			case <-ackDone:
				return
			case <-ackWake:
				// Send one ACK per signalled part. Coalescing the wake
				// notification is fine, dropping credits is not: a missing
				// ACK can stall the device after its 50-part window.
				for {
					n := ackPending.Swap(0)
					if n == 0 {
						break
					}
					for i := int64(0); i < n; i++ {
						acker.send()
					}
				}
			case <-tick:
				acker.send()
			}
		}
	}()
	signalAck := func() {
		ackPending.Add(1)
		select {
		case ackWake <- struct{}{}:
		default:
		}
	}

	// Cancel a blocked read promptly when ctx is done.
	watchDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.SetReadDeadline(time.Now())
		case <-watchDone:
		}
	}()
	defer close(watchDone)
	defer func() {
		close(ackDone)
		ackWG.Wait()
		sendStop(conn, acker.sessionID())
	}()

	immediate := opt.ACKInterval == 0 && opt.ACKEveryBytes <= 0
	lastByteAck := int64(0)
	finished := false
	noData := false
	stopReason := "eof"

readLoop:
	for {
		if ctx.Err() != nil {
			stopReason = "context"
			break
		}
		_ = conn.SetReadDeadline(time.Now().Add(opt.Idle))
		line, err := readTrimmedLine(br)
		if err != nil {
			if ctx.Err() != nil {
				stopReason = "context"
				break
			}
			if isTimeout(err) {
				stopReason = "idle-timeout"
				break
			}
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				stopReason = "eof"
				break
			}
			return stats, fmt.Errorf("nvr: download: read response: %w", err)
		}
		if line == "" { // CRLF between parts
			continue
		}
		if !strings.HasPrefix(line, "--") {
			return stats, fmt.Errorf("nvr: download: unexpected multipart line %q", line)
		}
		hdrs, err := readPartHeaders(br)
		if err != nil {
			return stats, fmt.Errorf("nvr: download: read part headers: %w", err)
		}
		n, err := parseContentLength(hdrs["content-length"])
		if err != nil {
			return stats, fmt.Errorf("nvr: download: invalid Content-Length %q: %w", hdrs["content-length"], err)
		}
		if n < 0 || n > maxPartBytes {
			return stats, fmt.Errorf("nvr: download: Content-Length out of range: %d", n)
		}
		body := make([]byte, n)
		_ = conn.SetReadDeadline(time.Now().Add(opt.Idle))
		if _, err := io.ReadFull(br, body); err != nil {
			if ctx.Err() != nil {
				stopReason = "context"
				break
			}
			if isTimeout(err) {
				stopReason = "idle-timeout"
			}
			return stats, fmt.Errorf("nvr: download: read %d-byte part: %w", n, err)
		}
		if sid := hdrs["x-session-id"]; sid != "" {
			acker.setSession(sid)
		}

		ct := strings.ToLower(hdrs["content-type"])
		switch {
		case strings.Contains(ct, "video/mp2t"):
			if err := writeAll(w, body); err != nil {
				return stats, fmt.Errorf("nvr: download: write output: %w", err)
			}
			stats.Bytes += int64(len(body))
			probe.feed(body)
			switch {
			case immediate:
				signalAck()
			case opt.ACKEveryBytes > 0 && stats.Bytes-lastByteAck >= int64(opt.ACKEveryBytes):
				lastByteAck = stats.Bytes
				signalAck()
			}
		case strings.Contains(ct, "json"):
			fin, nd := handleJSONPart(body, acker)
			if nd {
				noData = true
			}
			if fin {
				finished = true
				stopReason = "finished"
				break readLoop
			}
		}
	}

	switch {
	case ctx.Err() != nil:
		return stats, fmt.Errorf("nvr: download: %w", ctx.Err())
	case !finished:
		if noData && stats.Bytes == 0 {
			return stats, fmt.Errorf("nvr: download: device reported no data (%s): %w", stopReason, ErrNoMedia)
		}
		return stats, fmt.Errorf("nvr: download: %s after %d bytes: %w", stopReason, stats.Bytes, ErrTruncated)
	case stats.Bytes == 0:
		return stats, fmt.Errorf("nvr: download: finished with no data: %w", ErrNoMedia)
	case probe.syncErrors > 0 || len(probe.buf) > 0 || probe.packets == 0:
		return stats, fmt.Errorf("nvr: download: %w (packets=%d, sync_errors=%d, trailing=%d)",
			ErrInvalidTS, probe.packets, probe.syncErrors, len(probe.buf))
	}
	return stats, nil
}

// dialStream performs the two-step Digest + multipart handshake and returns
// the live connection plus a buffered reader positioned at the response
// body. The whole sequence must happen on one raw TCP connection.
func (c *Client) dialStream(ctx context.Context, path string, reqJSON []byte) (*streamConn, error) {
	addr, err := c.streamAddr()
	if err != nil {
		return nil, err
	}
	conn, err := c.dial.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("nvr: stream dial %s: %w", addr, err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = conn.Close()
		}
	}()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	br := bufio.NewReaderSize(conn, 256<<10)

	// Abort a stalled handshake as soon as the caller cancels.
	watchDone := make(chan struct{})
	defer close(watchDone)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.SetDeadline(time.Now())
		case <-watchDone:
		}
	}()

	post1 := fmt.Sprintf("POST %s HTTP/1.1\r\nContent-Length: -1\r\nX-Client-UUID: %s\r\nX-Client-Model: Android\r\nConnection: keep-alive\r\nHost: %s\r\nContent-Type: multipart/mixed;boundary=--%s--\r\n\r\n",
		path, randHexN(16), addr, streamBoundary)
	if _, err := conn.Write([]byte(post1)); err != nil {
		return nil, fmt.Errorf("nvr: stream handshake: %w", err)
	}
	head, err := readHTTPHead(br)
	if err != nil {
		return nil, fmt.Errorf("nvr: stream handshake: read 401: %w", err)
	}
	if len(head) == 0 {
		return nil, errors.New("nvr: stream handshake: empty response")
	}
	status := head[0]
	wa := headerVal(head, "www-authenticate")
	if !strings.Contains(status, "401") {
		return nil, fmt.Errorf("nvr: stream handshake: expected 401, got %q", status)
	}
	nonce := authParam(wa, "nonce")
	opaque := authParam(wa, "opaque")
	if nonce == "" {
		return nil, fmt.Errorf("nvr: stream handshake: no nonce in %q", wa)
	}
	// A 401 body (if any) would otherwise be parsed as part of the next
	// response head.
	if n, _ := parseContentLength(headerVal(head, "content-length")); n > 0 && n <= 64<<10 {
		_, _ = io.CopyN(io.Discard, br, int64(n))
	}

	var body bytes.Buffer
	body.WriteString("----" + streamBoundary + "--\r\n")
	body.WriteString("Content-Type: application/json\r\n")
	body.WriteString("X-Data-Window-Size: 50\r\n")
	fmt.Fprintf(&body, "Content-Length: %d\r\n\r\n", len(reqJSON))
	body.Write(reqJSON)
	body.WriteString("\r\n")
	for i, rc := 0, ackStep; i < 10; i, rc = i+1, rc+ackStep {
		nj, _ := json.Marshal(map[string]any{
			"type":   "notification",
			"params": map[string]any{"event_type": "stream_sequence"},
		})
		body.WriteString("----" + streamBoundary + "--\r\n")
		body.WriteString("X-Session-Id: 1\r\nContent-Type: application/json\r\n")
		fmt.Fprintf(&body, "X-Data-Received: %d\r\n", rc)
		fmt.Fprintf(&body, "Content-Length: %d\r\n\r\n", len(nj))
		body.Write(nj)
		body.WriteString("\r\n")
	}

	auth := digestResp(nonce, opaque, c.User, c.encPw, path)
	post2 := fmt.Sprintf("POST %s HTTP/1.1\r\nContent-Length: -1\r\nX-Client-UUID: %s\r\nX-Client-Model: Android\r\nConnection: keep-alive\r\nHost: %s\r\nAuthorization: %s\r\nContent-Type: multipart/mixed;boundary=--%s--\r\n\r\n",
		path, randHexN(16), addr, auth, streamBoundary)
	if _, err := conn.Write(append([]byte(post2), body.Bytes()...)); err != nil {
		return nil, fmt.Errorf("nvr: stream handshake: %w", err)
	}
	head2, err := readHTTPHead(br)
	if err != nil {
		return nil, fmt.Errorf("nvr: stream handshake: read response: %w", err)
	}
	if len(head2) == 0 {
		return nil, errors.New("nvr: stream handshake: empty response")
	}
	if !strings.Contains(head2[0], "200") {
		return nil, fmt.Errorf("nvr: stream handshake: %q (%.200s)", head2[0], strings.Join(head2, " | "))
	}
	_ = conn.SetDeadline(time.Time{})
	ok = true
	return &streamConn{conn: conn, br: br}, nil
}

type downloadAcker struct {
	conn    net.Conn
	mu      sync.Mutex
	credit  int
	session string
	sent    int64
}

func newDownloadAcker(conn net.Conn) *downloadAcker {
	return &downloadAcker{conn: conn, credit: prepaidCredit, session: "1"}
}

func (a *downloadAcker) setSession(s string) {
	a.mu.Lock()
	a.session = s
	a.mu.Unlock()
}

func (a *downloadAcker) sessionID() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.session
}

func (a *downloadAcker) send() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.credit += ackStep
	body := []byte(`{"type":"notification","params":{"event_type":"stream_sequence"}}`)
	msg := fmt.Sprintf("\r\n----%s--\r\nX-Session-Id: %s\r\nContent-Type: application/json\r\nX-Data-Received: %d\r\nContent-Length: %d\r\n\r\n%s\r\n",
		streamBoundary, a.session, a.credit, len(body), body)
	_ = a.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := a.conn.Write([]byte(msg)); err == nil {
		a.sent++
	}
}

// sendStop tells the device we are done (best effort, App-compatible).
func sendStop(conn net.Conn, session string) {
	body := []byte(`{"type":"request","seq":1,"params":{"method":"do","stop":"null"}}`)
	msg := fmt.Sprintf("\r\n----%s--\r\nX-Session-Id: %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s\r\n",
		streamBoundary, session, len(body), body)
	_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_, _ = conn.Write([]byte(msg))
}

// handleJSONPart parses an envelope part. The stream-status key varies by
// firmware: status, stream_status or channel_stream_status.
func handleJSONPart(body []byte, acker *downloadAcker) (finished, noData bool) {
	var j struct {
		Type   string `json:"type"`
		Seq    int    `json:"seq"`
		Params struct {
			ErrorCode           int             `json:"error_code"`
			SessionID           string          `json:"session_id"`
			EventType           string          `json:"event_type"`
			Status              json.RawMessage `json:"status"`
			StreamStatus        json.RawMessage `json:"stream_status"`
			ChannelStreamStatus json.RawMessage `json:"channel_stream_status"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &j); err != nil {
		return false, false
	}
	if j.Params.SessionID != "" {
		acker.setSession(j.Params.SessionID)
	}
	for _, raw := range []json.RawMessage{j.Params.Status, j.Params.StreamStatus, j.Params.ChannelStreamStatus} {
		switch jsonStatus(raw) {
		case "finished":
			return true, false
		case "no_data":
			noData = true
		}
	}
	return false, noData
}

// jsonStatus handles both a plain string and the string-array form used by
// channel_stream_status.
func jsonStatus(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var a []string
	if json.Unmarshal(raw, &a) == nil {
		for _, v := range a {
			if v != "" && v != "ok" && v != "normal" {
				return v
			}
		}
		if len(a) > 0 {
			return a[0]
		}
	}
	return ""
}

func readTrimmedLine(br *bufio.Reader) (string, error) {
	s, err := br.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(s, "\r\n"), nil
}

func readPartHeaders(br *bufio.Reader) (map[string]string, error) {
	h := make(map[string]string)
	for {
		line, err := readTrimmedLine(br)
		if err != nil {
			return nil, err
		}
		if line == "" {
			return h, nil
		}
		i := strings.Index(line, ":")
		if i <= 0 {
			continue
		}
		h[strings.ToLower(strings.TrimSpace(line[:i]))] = strings.TrimSpace(line[i+1:])
	}
}

func parseContentLength(s string) (int, error) {
	var n int
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%d", &n); err != nil {
		return 0, err
	}
	return n, nil
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// ---------------------------------------------------------------------------
// Minimal TS probe: packet count, sync errors, PES PTS range.
// ---------------------------------------------------------------------------

type tsProbe struct {
	buf        []byte
	packets    int64
	syncErrors int64

	ptsMin  uint64
	ptsMax  uint64
	havePTS bool
}

func newTSProbe() *tsProbe { return &tsProbe{} }

func (p *tsProbe) feed(data []byte) {
	p.buf = append(p.buf, data...)
	for len(p.buf) >= tsPacketSize {
		if p.buf[0] != 0x47 {
			j := bytes.IndexByte(p.buf[1:], 0x47)
			if j < 0 {
				p.buf = p.buf[:0]
				p.syncErrors++
				return
			}
			p.buf = p.buf[1+j:]
			p.syncErrors++
			continue
		}
		p.parsePacket(p.buf[:tsPacketSize])
		p.packets++
		p.buf = p.buf[tsPacketSize:]
	}
}

func (p *tsProbe) parsePacket(pkt []byte) {
	pid := (int(pkt[1]&0x1f) << 8) | int(pkt[2])
	if pid == 0 || pid == 0x1fff {
		return
	}
	if pkt[1]&0x40 == 0 { // no payload unit start => not a PES header
		return
	}
	afc := (pkt[3] >> 4) & 0x03
	if afc == 0 || afc == 2 {
		return
	}
	pos := 4
	if afc == 3 {
		pos += 1 + int(pkt[4])
	}
	if pos >= tsPacketSize {
		return
	}
	payload := pkt[pos:]
	pts, ok := parsePES_PTS(payload)
	if !ok {
		return
	}
	if !p.havePTS {
		p.ptsMin, p.ptsMax, p.havePTS = pts, pts, true
		return
	}
	if pts < p.ptsMin {
		p.ptsMin = pts
	}
	if pts > p.ptsMax {
		p.ptsMax = pts
	}
}

// parsePES_PTS extracts the 33-bit 90 kHz PTS from a PES header.
func parsePES_PTS(p []byte) (uint64, bool) {
	if len(p) < 14 || p[0] != 0x00 || p[1] != 0x00 || p[2] != 0x01 {
		return 0, false
	}
	if (p[7]>>6)&0x03 == 0 {
		return 0, false
	}
	b := p[9:14]
	pts := (uint64(b[0]&0x0E) << 29) |
		(uint64(b[1]) << 22) |
		(uint64(b[2]&0xFE) << 14) |
		(uint64(b[3]) << 7) |
		(uint64(b[4]&0xFE) >> 1)
	return pts, true
}
