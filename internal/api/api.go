// Package api implements the nvrsync HTTP API and serves the embedded web UI.
package api

import (
	"io/fs"
	"log"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/myml/nvrsync/internal/play"
	"github.com/myml/nvrsync/internal/store"
	"github.com/myml/nvrsync/internal/syncer"
	"github.com/myml/nvrsync/web"
)

// server holds the dependencies shared by all handlers.
type server struct {
	st        *store.Store
	sy        *syncer.Syncer
	pl        *play.Player
	assets    fs.FS
	ver       string
	commit    string
	buildDate string
	auth      *authManager
}

// New returns the complete HTTP handler: JSON API, media files and the SPA.
//
// assets is the frontend filesystem (normally web.Sub()). Passing nil uses the
// assets embedded in the binary, which is what production does.
func New(st *store.Store, sy *syncer.Syncer, pl *play.Player, assets fs.FS, ver string, build ...string) http.Handler {
	if assets == nil {
		assets = web.Sub()
	}
	commit, buildDate := "", ""
	if len(build) > 0 {
		commit = build[0]
	}
	if len(build) > 1 {
		buildDate = build[1]
	}
	s := &server{
		st: st, sy: sy, pl: pl, assets: assets, ver: ver,
		commit: commit, buildDate: buildDate, auth: newAuthManager(),
	}
	// Headers and compression wrap everything; logging/panic recovery stays
	// next; authentication runs before any business handler.
	return withSecurityHeaders(withCompression(withMiddleware(s.authMiddleware(s.routes()))))
}

// routes registers every endpoint from ARCH.md.
func (s *server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /api/ready", s.handleReady)
	mux.HandleFunc("GET /api/settings", s.handleGetSettings)
	mux.HandleFunc("PUT /api/settings", s.handlePutSettings)

	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.handleLogout)
	mux.HandleFunc("POST /api/auth/password", s.handleAuthPassword)

	mux.HandleFunc("POST /api/nvr/test", s.handleNVRTest)
	mux.HandleFunc("POST /api/nvr/scan", s.handleNVRScan)
	mux.HandleFunc("POST /api/catalog/refresh", s.handleCatalogRefresh)

	mux.HandleFunc("GET /api/segments", s.handleSegments)
	mux.HandleFunc("GET /api/stats", s.handleStats)

	mux.HandleFunc("POST /api/sync/start", s.handleSyncStart)
	mux.HandleFunc("POST /api/sync/stop", s.handleSyncStop)
	mux.HandleFunc("GET /api/sync/status", s.handleSyncStatus)
	mux.HandleFunc("GET /api/metrics", s.handleMetrics)

	mux.HandleFunc("GET /api/logs", s.handleLogs)
	mux.HandleFunc("GET /api/days", s.handleDays)
	mux.HandleFunc("GET /api/playlist", s.handlePlaylist)

	mux.HandleFunc("GET /media/{ch}/{date}/{file}", s.handleMedia)

	mux.Handle("GET /assets/", http.StripPrefix("/assets/", s.assetHandler()))
	mux.HandleFunc("GET /login", s.handleLoginPage)
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /sync", s.handleIndex)
	mux.HandleFunc("GET /recordings", s.handleIndex)
	mux.HandleFunc("GET /settings", s.handleIndex)
	mux.HandleFunc("/", s.handleFallback)

	return mux
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
			elapsed := time.Since(start)
			// Successful status polling, health checks and static files would
			// otherwise drown every useful line in the container log.
			if rec.status >= http.StatusBadRequest || elapsed > time.Second || !isQuietRequest(r) {
				log.Printf("%s %s -> %d (%s)", r.Method, r.URL.RequestURI(), rec.status,
					elapsed.Round(time.Microsecond))
			}
		}()

		if crossOriginMutation(r) {
			writeError(rec, http.StatusForbidden, "cross-origin request rejected")
			return
		}

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
