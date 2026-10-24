// Package nvr implements the TP-Link NVR protocol used by nvrsync.
//
// Two independent services are involved:
//
//   - the /ds management API (HTTP, normally port 8081 on current firmware,
//     but 80/8080 are also probed): login, get_user_id, get_media_list;
//   - the stream server on port 8000: HTTP Digest + a two-step multipart
//     handshake, used by Download to pull raw TS from /stream/download.
//
// The stream handshake deliberately uses a raw net.Conn (not net/http):
// the first request carries "Content-Length: -1" and both requests are sent
// on the same connection before the response body is consumed. net/http
// refuses to handle that combination.
package nvr

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// defaultStreamPort is the NVR media/stream server port.
	defaultStreamPort = 8000

	// nvrRealm is the fixed Digest realm reported by these NVRs.
	nvrRealm = "TP-Link IP-Camera"

	// tpSalt / tpKey implement the TP-Link password obfuscation
	// (securityEncode). The encoded password is used both for /ds login and
	// for the port-8000 HTTP Digest.
	tpSalt = "RDpbLfCPsJZ7fiv"
	tpKey  = "yLwVl0zKqws7LgKPRQ84Mdt708T1qQ3Ha7xv3H7NyU84p21BriUWBU43odz3iP4rBL3cD02KZci" +
		"XTysVXiV8ngg6vL48rPJyAUw0HurW20xqxv9aYb4M9wK1Ae0wlro510qXeU07kV57fQMc8L6aLgMLwygtc0F10a0Dg70TOoouy" +
		"FhdysuRMO51yY5ZlOZZLEal1h0t9YQW0Ko7oBwmCAHoic4HYbUyVeU3sfQ1xtXcPcf1aT303wAQhv66qzW"

	// streamBoundary is the request-side multipart boundary. The odd
	// "boundary=--name--" spelling matches the device/App and is kept
	// verbatim.
	streamBoundary = "client-stream-boundary"

	maxDSBody = 32 << 20
)

// ErrNoMedia is returned by MediaList when the NVR replies -71104/-40101:
// the requested range has no index (too old, outside retention, or not
// recorded). It is not a transport failure.
var ErrNoMedia = errors.New("nvr: no media in requested range")

// Client is a connection to one NVR. Addr is the stream server address
// ("host" or "host:8000"); AdminPort, when non-zero, is tried first for the
// /ds management API before the built-in 80/8081/8080 probe.
type Client struct {
	Addr      string
	User      string
	Pass      string
	AdminPort int

	encPw string
	hc    *http.Client
	dial  net.Dialer

	mu     sync.Mutex
	base   string // management API base, e.g. http://host:8081
	stok   string
	userID int
}

// New returns a client for host. host is the stream address and may omit
// the port (8000 is assumed); adminPort, when non-zero, is tried first for
// the /ds management API before the 80/8081/8080 probe. The connection is
// lazy: no network I/O happens until Login/MediaList/Download is called.
// password is the administrator password in clear text; it is obfuscated
// internally with the TP-Link securityEncode algorithm.
func New(host, user, password string, adminPort int) *Client {
	return &Client{
		Addr:      strings.TrimSpace(host),
		User:      user,
		Pass:      password,
		AdminPort: adminPort,
		encPw:     securityEncode(password),
		hc:        &http.Client{Timeout: 20 * time.Second},
		dial:      net.Dialer{Timeout: 8 * time.Second},
	}
}

// securityEncode is the TP-Link password obfuscation. It is a reversible
// byte-wise XOR against a large key table (the device only needs the
// obfuscated form, so the reverse direction is not implemented).
func securityEncode(data string) string {
	d := max(len(data), len(tpSalt))
	var out strings.Builder
	out.Grow(d)
	for m := 0; m < d; m++ {
		k, l := 187, 187
		switch {
		case m >= len(data):
			l = int(tpSalt[m])
		case m >= len(tpSalt):
			k = int(data[m])
		default:
			k = int(data[m])
			l = int(tpSalt[m])
		}
		out.WriteByte(tpKey[(k^l)%len(tpKey)])
	}
	return out.String()
}

func md5hex(s string) string {
	h := md5.Sum([]byte(s))
	return hex.EncodeToString(h[:])
}

func randHexN(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failure should not kill a sync; a fixed-ish value only
		// weakens the client UUID, not authentication.
		for i := range b {
			b[i] = byte(i*31 + 7)
		}
	}
	return hex.EncodeToString(b)
}

// digestResp builds the Authorization header value for one POST.
// uri must be exactly the request path (/stream/download for downloads).
// Quoting every value is required by these devices.
func digestResp(nonce, opaque, user, pw, path string) string {
	ha1 := md5hex(user + ":" + nvrRealm + ":" + pw)
	ha2 := md5hex("POST:" + path)
	cn := randHexN(8)
	nc := "00000001"
	r := md5hex(ha1 + ":" + nonce + ":" + nc + ":" + cn + ":auth:" + ha2)
	return fmt.Sprintf(`Digest username="%s", realm="%s", nonce="%s", uri="%s", qop=auth, nc=%s, cnonce="%s", response="%s", opaque="%s"`,
		user, nvrRealm, nonce, path, nc, cn, r, opaque)
}

// Login authenticates against the /ds management API. It probes AdminPort
// (when set) first, then the conventional 80, 8081 and 8080 ports. A
// successful login also invalidates any cached get_user_id value, because
// the id is a per-session media identifier.
func (c *Client) Login() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stok = ""
	c.userID = 0
	return c.loginLocked()
}

func (c *Client) loginLocked() error {
	host, err := c.adminHost()
	if err != nil {
		return err
	}
	var ports []int
	if c.AdminPort > 0 {
		ports = append(ports, c.AdminPort)
	}
	for _, p := range []int{80, 8081, 8080} {
		if p != c.AdminPort {
			ports = append(ports, p)
		}
	}

	var lastErr error
	for _, port := range ports {
		base := "http://" + net.JoinHostPort(host, strconv.Itoa(port))
		stok, err := c.loginAt(base)
		if err != nil {
			lastErr = fmt.Errorf("%s: %w", base, err)
			continue
		}
		c.base = base
		c.stok = stok
		c.userID = 0
		return nil
	}
	return fmt.Errorf("nvr: management API login failed (tried ports %v): %w", ports, lastErr)
}

func (c *Client) loginAt(base string) (string, error) {
	req := map[string]any{
		"method": "do",
		"login":  map[string]any{"username": c.User, "password": c.encPw},
	}
	raw, err := c.post(context.Background(), base+"/", req)
	if err != nil {
		return "", err
	}
	var r struct {
		Stok      string `json:"stok"`
		ErrorCode int    `json:"error_code"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return "", fmt.Errorf("login: bad response: %w (raw %.200s)", err, raw)
	}
	if r.ErrorCode != 0 {
		return "", fmt.Errorf("login: error_code=%d", r.ErrorCode)
	}
	if r.Stok == "" {
		return "", fmt.Errorf("login: response has no stok (raw %.200s)", raw)
	}
	return r.Stok, nil
}

// GetUserID returns system.get_user_id for the current login session. The
// value is a media session id (not a stable account id) and must be passed
// to download as client_id, so it is cached until the next Login.
//
// The NVR answers -40401 when it has no session budget left. That is
// transient and should be retried with a fresh login instead of turning every
// queued download into a permanent "failed" row.
func (c *Client) GetUserID() (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * time.Second)
		}
		if c.stok == "" {
			if err := c.loginLocked(); err != nil {
				lastErr = err
				continue
			}
		}
		if c.userID > 0 {
			return c.userID, nil
		}
		raw, err := c.callLocked(map[string]any{
			"system": map[string]any{"get_user_id": nil},
			"method": "do",
		})
		if err != nil {
			lastErr = err
			c.stok = ""
			continue
		}
		var r struct {
			UserID    int `json:"user_id"`
			ErrorCode int `json:"error_code"`
			System    struct {
				UserID int `json:"get_user_id"`
			} `json:"system"`
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			return 0, fmt.Errorf("get_user_id: bad response: %w (raw %.200s)", err, raw)
		}
		if r.ErrorCode == -40401 {
			lastErr = fmt.Errorf("get_user_id: error_code=%d (NVR busy)", r.ErrorCode)
			c.stok = ""
			c.userID = 0
			continue
		}
		if r.ErrorCode != 0 {
			return 0, fmt.Errorf("get_user_id: error_code=%d", r.ErrorCode)
		}
		switch {
		case r.UserID > 0:
			c.userID = r.UserID
		case r.System.UserID > 0:
			c.userID = r.System.UserID
		default:
			return 0, fmt.Errorf("get_user_id: no user_id in response (raw %.200s)", raw)
		}
		return c.userID, nil
	}
	if lastErr == nil {
		lastErr = errors.New("get_user_id: no attempt made")
	}
	return 0, fmt.Errorf("get_user_id after retries: %w", lastErr)
}

func (c *Client) callLocked(v any) (json.RawMessage, error) {
	if c.stok == "" || c.base == "" {
		return nil, errors.New("nvr: not logged in")
	}
	u := c.base + "/stok=" + url.QueryEscape(c.stok) + "/ds"
	return c.post(context.Background(), u, v)
}

// callContext is call with a context for cancellation (media scans).
func (c *Client) callContext(ctx context.Context, v any) (json.RawMessage, error) {
	c.mu.Lock()
	if c.stok == "" {
		if err := c.loginLocked(); err != nil {
			c.mu.Unlock()
			return nil, err
		}
	}
	u := c.base + "/stok=" + url.QueryEscape(c.stok) + "/ds"
	c.mu.Unlock()
	return c.post(ctx, u, v)
}

func (c *Client) post(ctx context.Context, u string, v any) (json.RawMessage, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxDSBody))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("POST %s: HTTP %s (%.200s)", u, resp.Status, raw)
	}
	return raw, nil
}

// adminHost extracts the bare host from Addr, accepting an optional scheme
// and/or port.
func (c *Client) adminHost() (string, error) {
	a := strings.TrimSpace(c.Addr)
	if a == "" {
		return "", errors.New("nvr: empty address")
	}
	if strings.Contains(a, "://") {
		u, err := url.Parse(a)
		if err != nil {
			return "", fmt.Errorf("nvr: bad address %q: %w", c.Addr, err)
		}
		a = u.Host
	}
	if h, _, err := net.SplitHostPort(a); err == nil {
		return strings.Trim(h, "[]"), nil
	}
	return strings.Trim(a, "[]"), nil
}

// streamAddr returns Addr with a port, defaulting to 8000.
func (c *Client) streamAddr() (string, error) {
	a := strings.TrimSpace(c.Addr)
	if a == "" {
		return "", errors.New("nvr: empty stream address")
	}
	if strings.Contains(a, "://") {
		u, err := url.Parse(a)
		if err != nil {
			return "", fmt.Errorf("nvr: bad address %q: %w", c.Addr, err)
		}
		a = u.Host
	}
	if _, _, err := net.SplitHostPort(a); err == nil {
		return a, nil
	}
	if strings.Count(a, ":") > 1 && !strings.HasPrefix(a, "[") {
		return net.JoinHostPort(a, strconv.Itoa(defaultStreamPort)), nil // bare IPv6
	}
	return net.JoinHostPort(a, strconv.Itoa(defaultStreamPort)), nil
}

// ---------------------------------------------------------------------------
// Raw HTTP helpers shared with the stream handshake.
// ---------------------------------------------------------------------------

type streamConn struct {
	conn net.Conn
	br   *bufio.Reader
}

func readHTTPHead(br *bufio.Reader) ([]string, error) {
	var lines []string
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			return lines, nil
		}
		lines = append(lines, line)
	}
}

func headerVal(lines []string, key string) string {
	for _, l := range lines {
		if i := strings.Index(l, ":"); i > 0 && strings.EqualFold(strings.TrimSpace(l[:i]), key) {
			return strings.TrimSpace(l[i+1:])
		}
	}
	return ""
}

func authParam(wa, key string) string {
	if strings.HasPrefix(strings.ToLower(wa), "digest") {
		wa = wa[len("digest"):]
	}
	for _, part := range strings.Split(wa, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) == 2 && strings.EqualFold(strings.TrimSpace(kv[0]), key) {
			return strings.Trim(strings.TrimSpace(kv[1]), `"`)
		}
	}
	return ""
}

func writeAll(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, err := w.Write(b)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}
