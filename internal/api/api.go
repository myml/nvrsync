// Package api implements the nvrsync HTTP API and serves the embedded web UI.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/myml/nvrsync/internal/nvr"
	"github.com/myml/nvrsync/internal/play"
	"github.com/myml/nvrsync/internal/store"
	"github.com/myml/nvrsync/internal/syncer"
	"github.com/myml/nvrsync/web"
)

const (
	dateLayout = "2006-01-02"

	maxBodyBytes = 1 << 20 // 1 MiB is plenty for settings and sync requests

	defaultSegmentLimit = 200
	maxSegmentLimit     = 1000
	defaultLogLimit     = 200
	maxLogLimit         = 1000

	indexFile = "index.html"
)

var (
	camPattern  = regexp.MustCompile(`^(?:cam)?([1-9][0-9]*)$`)
	datePattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
)

// server holds the dependencies shared by all handlers.
type server struct {
	st     *store.Store
	sy     *syncer.Syncer
	pl     *play.Player
	assets fs.FS
	ver    string
}

// New returns the complete HTTP handler: JSON API, media files and the SPA.
//
// assets is the frontend filesystem (normally web.Sub()). Passing nil uses the
// assets embedded in the binary, which is what production does.
func New(st *store.Store, sy *syncer.Syncer, pl *play.Player, assets fs.FS, ver string) http.Handler {
	if assets == nil {
		assets = web.Sub()
	}
	s := &server{st: st, sy: sy, pl: pl, assets: assets, ver: ver}
	return withMiddleware(s.routes())
}

// routes registers every endpoint from ARCH.md.
func (s *server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /api/settings", s.handleGetSettings)
	mux.HandleFunc("PUT /api/settings", s.handlePutSettings)
	mux.HandleFunc("POST /api/nvr/test", s.handleNVRTest)
	mux.HandleFunc("POST /api/nvr/scan", s.handleNVRScan)
	mux.HandleFunc("POST /api/catalog/refresh", s.handleCatalogRefresh)

	mux.HandleFunc("GET /api/segments", s.handleSegments)

	mux.HandleFunc("POST /api/sync/start", s.handleSyncStart)
	mux.HandleFunc("POST /api/sync/stop", s.handleSyncStop)
	mux.HandleFunc("GET /api/sync/status", s.handleSyncStatus)

	mux.HandleFunc("GET /api/logs", s.handleLogs)
	mux.HandleFunc("GET /api/days", s.handleDays)
	mux.HandleFunc("GET /api/playlist", s.handlePlaylist)

	mux.HandleFunc("GET /media/{ch}/{date}/{file}", s.handleMedia)

	mux.Handle("GET /assets/", http.StripPrefix("/assets/", s.assetHandler()))
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /sync", s.handleIndex)
	mux.HandleFunc("GET /recordings", s.handleIndex)
	mux.HandleFunc("GET /settings", s.handleIndex)
	mux.HandleFunc("/", s.handleFallback)

	return mux
}

// --- health / settings ------------------------------------------------------

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	// The documented response is {ok,version}; keep the version both at the
	// top level (ARCH.md table) and under data (generic envelope).
	writeJSON(w, http.StatusOK, struct {
		OK      bool   `json:"ok"`
		Version string `json:"version"`
		Data    struct {
			Version string `json:"version"`
		} `json:"data"`
	}{
		OK:      true,
		Version: s.ver,
		Data: struct {
			Version string `json:"version"`
		}{Version: s.ver},
	})
}

func (s *server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	writeOK(w, http.StatusOK, s.st.Settings())
}

func (s *server) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	var in store.Settings
	if err := decodeJSON(w, r, &in, false); err != nil {
		writeError(w, http.StatusBadRequest, "invalid settings: "+err.Error())
		return
	}
	if err := validateSettings(in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.st.SaveSettings(in); err != nil {
		writeError(w, http.StatusInternalServerError, "save settings: "+err.Error())
		return
	}
	// Nothing to restart: the syncer reads the latest settings from the store
	// at the start of every loop iteration and before each download.
	s.st.Log("info", "settings", "settings updated")
	writeOK(w, http.StatusOK, s.st.Settings())
}

// validateSettings rejects values that would make the syncer loop misbehave.
// Incomplete settings are allowed so the UI can save a partially filled form.
func validateSettings(v store.Settings) error {
	switch {
	case v.AdminPort < 0 || v.AdminPort > 65535:
		return fmt.Errorf("admin_port must be between 0 and 65535")
	case v.AutoMinutes < 0:
		return fmt.Errorf("auto_minutes must not be negative")
	case v.BackfillH < 0:
		return fmt.Errorf("backfill_hours must not be negative")
	case v.RetentionD < 0:
		return fmt.Errorf("retention_days must not be negative")
	}
	for _, ch := range v.Channels {
		if ch <= 0 {
			return fmt.Errorf("channels must contain positive channel numbers, got %d", ch)
		}
	}
	return nil
}

func (s *server) handleNVRTest(w http.ResponseWriter, r *http.Request) {
	set := s.st.Settings()
	if strings.TrimSpace(set.NVRHost) == "" {
		writeError(w, http.StatusBadRequest, "nvr_host is not configured")
		return
	}

	client := nvr.New(set.NVRHost, set.NVRUser, set.NVRPass, set.AdminPort)
	if err := client.Login(); err != nil {
		writeError(w, http.StatusBadGateway, "login failed: "+err.Error())
		return
	}

	ch := 1
	if len(set.Channels) > 0 {
		ch = set.Channels[0]
	}
	to := time.Now()
	from := to.Add(-10 * time.Minute)
	segments, err := client.MediaList(ch, from.Unix(), to.Unix())
	if err != nil && !errors.Is(err, nvr.ErrNoMedia) {
		writeError(w, http.StatusBadGateway, "media list failed: "+err.Error())
		return
	}

	writeOK(w, http.StatusOK, map[string]any{
		"login":    true,
		"ch":       ch,
		"segments": len(segments),
		"from":     from.Unix(),
		"to":       to.Unix(),
	})
}

// --- segments / days --------------------------------------------------------

func (s *server) handleSegments(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	ch, err := parseIntParam(q.Get("ch"), 0)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid ch: "+err.Error())
		return
	}
	from, err := parseTimeParam(q.Get("from"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid from: "+err.Error())
		return
	}
	to, err := parseTimeParam(q.Get("to"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid to: "+err.Error())
		return
	}
	if from != 0 && to != 0 && from > to {
		writeError(w, http.StatusBadRequest, "from must not be after to")
		return
	}
	status := strings.TrimSpace(q.Get("status"))
	if status == "all" {
		status = ""
	}
	limit, err := parseLimit(q.Get("limit"), defaultSegmentLimit, maxSegmentLimit)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid limit: "+err.Error())
		return
	}
	offset, err := parseIntParam(q.Get("offset"), 0)
	if err != nil || offset < 0 {
		writeError(w, http.StatusBadRequest, "invalid offset")
		return
	}

	items, total := s.st.Segments(store.SegFilter{
		Ch:     ch,
		From:   from,
		To:     to,
		Status: status,
		Limit:  limit,
		Offset: offset,
	})
	if items == nil {
		items = []store.Segment{}
	}
	writeOK(w, http.StatusOK, struct {
		Total int             `json:"total"`
		Items []store.Segment `json:"items"`
	}{Total: total, Items: items})
}

func (s *server) handleDays(w http.ResponseWriter, r *http.Request) {
	ch, err := parseIntParam(r.URL.Query().Get("ch"), 0)
	if err != nil || ch < 0 {
		writeError(w, http.StatusBadRequest, "invalid ch")
		return
	}
	days := s.st.Days(ch)
	if days == nil {
		days = []store.DayInfo{}
	}
	writeOK(w, http.StatusOK, days)
}

// --- sync -------------------------------------------------------------------

// handleCatalogRefresh reconciles the local index with the NVR list.
func (s *server) handleCatalogRefresh(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 180*time.Second)
	defer cancel()
	force := r.URL.Query().Get("force") == "1" || r.URL.Query().Get("force") == "true"
	res, err := s.sy.RefreshCatalogForce(ctx, force)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, syncer.ErrAlreadyRunning) {
			status = http.StatusConflict
		}
		writeError(w, status, err.Error())
		return
	}
	writeOK(w, http.StatusOK, res)
}

// handleNVRScan lists NVR recordings for a range without downloading them, so
// the sync page can browse any day before deciding what to pull.
func (s *server) handleNVRScan(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Channels []int `json:"channels"`
		From     int64 `json:"from"`
		To       int64 `json:"to"`
	}
	if err := decodeJSON(w, r, &req, true); err != nil {
		writeError(w, http.StatusBadRequest, "invalid scan request: "+err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	listed, added, err := s.sy.ScanOnly(ctx, req.Channels, req.From, req.To)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeOK(w, http.StatusOK, map[string]any{"listed": listed, "added": added})
}

func (s *server) handleSyncStart(w http.ResponseWriter, r *http.Request) {
	var req syncer.SyncRequest
	if err := decodeJSON(w, r, &req, true); err != nil {
		writeError(w, http.StatusBadRequest, "invalid sync request: "+err.Error())
		return
	}
	if req.From != 0 && req.To != 0 && req.From >= req.To {
		writeError(w, http.StatusBadRequest, "from must be before to")
		return
	}
	if err := s.sy.Start(req); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, syncer.ErrAlreadyRunning) {
			status = http.StatusConflict
		}
		writeError(w, status, err.Error())
		return
	}
	writeOK(w, http.StatusOK, map[string]any{"running": true})
}

func (s *server) handleSyncStop(w http.ResponseWriter, r *http.Request) {
	s.sy.Stop()
	writeOK(w, http.StatusOK, map[string]any{"running": s.sy.Running()})
}

func (s *server) handleSyncStatus(w http.ResponseWriter, r *http.Request) {
	writeOK(w, http.StatusOK, s.st.SyncState())
}

// --- logs -------------------------------------------------------------------

func (s *server) handleLogs(w http.ResponseWriter, r *http.Request) {
	limit, err := parseLimit(r.URL.Query().Get("limit"), defaultLogLimit, maxLogLimit)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid limit: "+err.Error())
		return
	}
	level := strings.TrimSpace(r.URL.Query().Get("level"))
	if level == "all" {
		level = ""
	}
	if level != "" && level != "info" && level != "warn" && level != "error" {
		writeError(w, http.StatusBadRequest, `level must be one of "info", "warn", "error"`)
		return
	}

	// The store filters by level and returns newest first, so a request for
	// the newest N errors does not get diluted by unrelated log levels.
	logs := s.st.Logs(limit, level)
	if logs == nil {
		logs = []store.LogEntry{}
	}
	writeOK(w, http.StatusOK, logs)
}

// --- playlist / media -------------------------------------------------------

func (s *server) handlePlaylist(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ch, err := parseIntParam(q.Get("ch"), 0)
	if err != nil || ch <= 0 {
		writeError(w, http.StatusBadRequest, "invalid ch")
		return
	}
	date := q.Get("date")
	if !validDate(date) {
		writeError(w, http.StatusBadRequest, "invalid date, want YYYY-MM-DD")
		return
	}

	playlist, err := s.pl.DayPlaylist(ch, date)
	if err != nil {
		switch {
		case errors.Is(err, play.ErrInvalidDate), errors.Is(err, play.ErrInvalidChannel):
			writeError(w, http.StatusBadRequest, err.Error())
		default:
			writeError(w, http.StatusInternalServerError, "build playlist: "+err.Error())
		}
		return
	}

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Length", strconv.Itoa(len(playlist)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(playlist)
}

func (s *server) handleMedia(w http.ResponseWriter, r *http.Request) {
	ch, ok := parseChannel(r.PathValue("ch"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	date := r.PathValue("date")
	if !validDate(date) {
		http.NotFound(w, r)
		return
	}
	file := r.PathValue("file")
	if !safeMediaFile(file) {
		http.NotFound(w, r)
		return
	}

	full, ok := s.resolveMedia(ch, date, file)
	if !ok {
		http.NotFound(w, r)
		return
	}

	// .ts is ambiguous in system mime databases, so never let mime.TypeByExtension
	// decide: raw NVR segments are always MPEG-TS.
	w.Header().Set("Content-Type", "video/mp2t")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	// ServeFile supports Range/If-Range/If-Modified-Since, which hls.js and
	// Safari rely on for seeking inside an hour-long segment.
	http.ServeFile(w, r, full)
}

// resolveMedia maps a validated (ch, date, file) triple to an absolute local
// path. Recorded segment metadata is preferred because it is authoritative for
// where the syncer actually wrote the file; the canonical layout
// (<storage>/rec/camN/<date>/<file>) is used as a fallback.
func (s *server) resolveMedia(ch int, date, file string) (string, bool) {
	set := s.st.Settings()
	root := strings.TrimSpace(set.StorageDir)

	from, to := dayBounds(date)
	if from != 0 {
		segments, _ := s.st.Segments(store.SegFilter{Ch: ch, From: from, To: to})
		for _, seg := range segments {
			if seg.Ch != ch || seg.Path == "" {
				continue
			}
			if filepath.Base(filepath.Clean(seg.Path)) != file {
				continue
			}
			if filepath.IsAbs(seg.Path) {
				candidate := filepath.Clean(seg.Path)
				if root == "" || underRoot(root, candidate) {
					if regularFile(candidate) {
						return candidate, true
					}
				}
				continue
			}
			if root == "" {
				continue
			}
			if candidate, ok := joinUnderRoot(root, seg.Path); ok && regularFile(candidate) {
				return candidate, true
			}
		}
	}

	if root == "" {
		return "", false
	}
	for _, rel := range []string{
		filepath.Join("rec", fmt.Sprintf("cam%d", ch), date, file),
		filepath.Join(fmt.Sprintf("cam%d", ch), date, file),
	} {
		candidate, ok := joinUnderRoot(root, rel)
		if ok && regularFile(candidate) {
			return candidate, true
		}
	}
	return "", false
}

// --- SPA / static -----------------------------------------------------------

func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	index, err := fs.ReadFile(s.assets, indexFile)
	if err != nil {
		// The frontend build may not have been generated yet. Keep the server
		// usable for API smoke tests instead of returning a confusing 404.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "<!doctype html><meta charset=\"utf-8\"><title>nvrsync</title>"+
			"<body style=\"font:16px system-ui;padding:2rem;background:#111;color:#ddd\">"+
			"<h1>nvrsync</h1><p>Frontend assets are not built yet (web/assets/index.html missing).</p>")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Length", strconv.Itoa(len(index)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(index)
}

// handleFallback keeps deep SPA links working and returns JSON for unknown API
// paths. Root-relative static files are served from the embedded assets as a
// convenience for bundles that do not use an /assets/ prefix.
func (s *server) handleFallback(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.NotFound(w, r)
		return
	}

	if name := strings.TrimPrefix(path.Clean(r.URL.Path), "/"); name != "" && name != "." {
		if info, err := fs.Stat(s.assets, name); err == nil && !info.IsDir() {
			s.assetHandler().ServeHTTP(w, r)
			return
		}
	}
	if path.Ext(r.URL.Path) == "" {
		s.handleIndex(w, r)
		return
	}
	http.NotFound(w, r)
}

func (s *server) assetHandler() http.Handler {
	fileServer := http.FileServerFS(s.assets)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		fileServer.ServeHTTP(w, r)
	})
}

// --- middleware -------------------------------------------------------------

// withMiddleware logs method, path, status and elapsed time, and converts a
// handler panic into a 500 instead of tearing down the connection.
func withMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &responseRecorder{ResponseWriter: w, status: http.StatusOK}

		defer func() {
			if v := recover(); v != nil {
				log.Printf("http: panic serving %s %s: %v\n%s", r.Method, r.URL.RequestURI(), v, debug.Stack())
				if !rec.wroteHeader {
					writeError(rec, http.StatusInternalServerError, "internal server error")
				}
			}
			log.Printf("%s %s -> %d (%s)", r.Method, r.URL.RequestURI(), rec.status,
				time.Since(start).Round(time.Microsecond))
		}()

		next.ServeHTTP(rec, r)
	})
}

type responseRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	bytes       int64
}

func (r *responseRecorder) WriteHeader(code int) {
	if r.wroteHeader {
		return
	}
	r.status = code
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(code)
}

func (r *responseRecorder) Write(p []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(p)
	r.bytes += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach the original writer.
func (r *responseRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// --- JSON helpers -----------------------------------------------------------

func writeOK(w http.ResponseWriter, status int, data any) {
	writeJSON(w, status, map[string]any{"ok": true, "data": data})
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"ok": false, "error": msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// decodeJSON decodes a single JSON value from a size-limited body. Empty
// bodies are accepted when allowEmpty is true (sync start takes optional JSON).
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any, allowEmpty bool) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		if allowEmpty && errors.Is(err, io.EOF) {
			return nil
		}
		if errors.Is(err, io.EOF) {
			return errors.New("empty request body")
		}
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("request body must contain a single JSON value")
		}
		return err
	}
	return nil
}

// --- query / path helpers ---------------------------------------------------

func parseIntParam(raw string, def int) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%q is not a number", raw)
	}
	return n, nil
}

func parseLimit(raw string, def, max int) (int, error) {
	n, err := parseIntParam(raw, def)
	if err != nil {
		return 0, err
	}
	if n < 0 {
		return 0, errors.New("must not be negative")
	}
	if n == 0 {
		return max, nil
	}
	if n > max {
		return max, nil
	}
	return n, nil
}

// parseTimeParam accepts unix seconds, RFC3339, or a YYYY-MM-DD date.
func parseTimeParam(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if n < 0 {
			return 0, errors.New("must not be negative")
		}
		return n, nil
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t.Unix(), nil
	}
	if t, err := time.ParseInLocation(dateLayout, raw, time.Local); err == nil {
		return t.Unix(), nil
	}
	return 0, fmt.Errorf("%q is not a unix timestamp, RFC3339 timestamp or YYYY-MM-DD date", raw)
}

func parseChannel(raw string) (int, bool) {
	m := camPattern.FindStringSubmatch(raw)
	if m == nil {
		return 0, false
	}
	ch, err := strconv.Atoi(m[1])
	if err != nil || ch <= 0 {
		return 0, false
	}
	return ch, true
}

func validDate(date string) bool {
	if !datePattern.MatchString(date) {
		return false
	}
	_, err := time.ParseInLocation(dateLayout, date, time.Local)
	return err == nil
}

func dayBounds(date string) (int64, int64) {
	day, err := time.ParseInLocation(dateLayout, date, time.Local)
	if err != nil {
		return 0, 0
	}
	return day.Unix(), day.AddDate(0, 0, 1).Unix()
}

// safeMediaFile rejects anything that is not a plain TS file name: no path
// separators, no dot entries and no sidecar JSON metadata.
func safeMediaFile(file string) bool {
	if file == "" || file == "." || file == ".." {
		return false
	}
	if strings.ContainsAny(file, `/\`) || strings.ContainsRune(file, 0) {
		return false
	}
	if filepath.Base(file) != file {
		return false
	}
	return strings.EqualFold(filepath.Ext(file), ".ts")
}

// joinUnderRoot clean-joins rel to root and verifies the result stays inside
// root. This is the directory-traversal guard for media requests.
func joinUnderRoot(root, rel string) (string, bool) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", false
	}
	full := filepath.Clean(filepath.Join(absRoot, filepath.Clean(rel)))
	if !underRoot(absRoot, full) {
		return "", false
	}
	return full, true
}

func underRoot(root, target string) bool {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(absRoot, absTarget)
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." &&
		!strings.HasPrefix(rel, ".."+string(filepath.Separator)) &&
		!filepath.IsAbs(rel)
}

func regularFile(name string) bool {
	info, err := os.Stat(name)
	return err == nil && info.Mode().IsRegular()
}
