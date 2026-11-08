package api

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/myml/nvrsync/internal/play"
	"github.com/myml/nvrsync/internal/store"
)

const indexFile = "index.html"

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
	if info, err := os.Stat(full); err == nil {
		w.Header().Set("ETag", fmt.Sprintf(`"%x-%x"`, info.ModTime().UnixNano(), info.Size()))
	}
	// ServeFile supports Range/If-Range/If-Modified-Since, which hls.js and
	// Safari rely on for seeking inside an hour-long segment.
	http.ServeFile(w, r, full)
}

// resolveMedia maps a validated (ch, date, file) triple to an absolute local
// path. The syncer always writes the canonical layout
// (<storage>/rec/camN/<date>/<file>), so that is checked first; the metadata
// lookup remains as a fallback for legacy or custom paths.
func (s *server) resolveMedia(ch int, date, file string) (string, bool) {
	set := s.st.Settings()
	root := strings.TrimSpace(set.StorageDir)

	if root != "" {
		for _, rel := range []string{
			filepath.Join("rec", fmt.Sprintf("cam%d", ch), date, file),
			filepath.Join(fmt.Sprintf("cam%d", ch), date, file),
		} {
			candidate, ok := joinUnderRoot(root, rel)
			if ok && regularFile(candidate) {
				return candidate, true
			}
		}
	}

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

	return "", false
}

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
		// The application bundle changes with every release, so it must be
		// revalidated; only the vendored player is large and stable enough to
		// cache for a day.
		if strings.HasSuffix(r.URL.Path, "/vendor/hls.min.js") {
			w.Header().Set("Cache-Control", "public, max-age=86400")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		if etag, ok := s.assetETag(r.URL.Path); ok {
			w.Header().Set("ETag", etag)
			if etagMatches(r.Header.Get("If-None-Match"), etag) {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
		fileServer.ServeHTTP(w, r)
	})
}
