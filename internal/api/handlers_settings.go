package api

import (
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/myml/nvrsync/internal/nvr"
	"github.com/myml/nvrsync/internal/store"
)

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	// The documented response is {ok,version}; keep the version both at the
	// top level (ARCH.md table) and under data (generic envelope). Commit and
	// build_date make it possible to tell which image is actually running.
	type healthData struct {
		Version   string `json:"version"`
		Commit    string `json:"commit,omitempty"`
		BuildDate string `json:"build_date,omitempty"`
		GoVersion string `json:"go_version"`
	}
	data := healthData{Version: s.ver, Commit: s.commit, BuildDate: s.buildDate, GoVersion: runtime.Version()}
	writeJSON(w, http.StatusOK, struct {
		OK        bool       `json:"ok"`
		Version   string     `json:"version"`
		Commit    string     `json:"commit,omitempty"`
		BuildDate string     `json:"build_date,omitempty"`
		Data      healthData `json:"data"`
	}{
		OK: true, Version: s.ver, Commit: s.commit, BuildDate: s.buildDate, Data: data,
	})
}

// handleReady is the readiness probe: the process is only useful if SQLite is
// reachable and (when configured) the storage directory is mounted. The NVR is
// deliberately not part of it: the service can still serve existing footage
// while the device is offline.
func (s *server) handleReady(w http.ResponseWriter, r *http.Request) {
	dbErr := s.st.Ping()
	set := s.st.Settings()
	storageDir := strings.TrimSpace(set.StorageDir)
	storageOK := true
	var free, total int64
	if storageDir != "" {
		var fs syscall.Statfs_t
		if err := syscall.Statfs(storageDir, &fs); err != nil {
			storageOK = false
		} else {
			free = int64(fs.Bavail) * int64(fs.Bsize)
			total = int64(fs.Blocks) * int64(fs.Bsize)
		}
	}
	ready := dbErr == nil && storageOK
	body := map[string]any{
		"ready":         ready,
		"db":            dbErr == nil,
		"storage":       storageOK,
		"version":       s.ver,
		"commit":        s.commit,
		"build_date":    s.buildDate,
		"storage_free":  free,
		"storage_total": total,
	}
	if dbErr != nil {
		body["db_error"] = dbErr.Error()
	}
	if !ready {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ok": false, "error": "not ready", "data": body,
		})
		return
	}
	writeOK(w, http.StatusOK, body)
}

// settingsResponse is the public shape of Settings. The NVR password is never
// returned: nvr_pass_set tells the UI whether a password is stored, and an
// empty nvr_pass on PUT means "keep the current password".
type settingsResponse struct {
	store.Settings
	NVRPassSet bool `json:"nvr_pass_set"`
}

func (s *server) publicSettings() settingsResponse {
	set := s.st.Settings()
	hasPassword := set.NVRPass != ""
	set.NVRPass = ""
	return settingsResponse{Settings: set, NVRPassSet: hasPassword}
}

func (s *server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	writeOK(w, http.StatusOK, s.publicSettings())
}

func (s *server) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	var in store.Settings
	if err := decodeJSON(w, r, &in, false); err != nil {
		writeError(w, http.StatusBadRequest, "invalid settings: "+err.Error())
		return
	}
	before := s.st.Settings()
	// The API never returns nvr_pass, so an empty value means "unchanged".
	// Clearing the NVR password is not a supported operation.
	if in.NVRPass == "" {
		in.NVRPass = before.NVRPass
	}
	// Settings() strips the hash, so the frontend never sends it back. Enabling
	// login without a stored password would lock everyone out.
	if in.AuthEnabled && s.st.AuthHash() == "" {
		writeError(w, http.StatusBadRequest, "set a login password before enabling login protection")
		return
	}
	if in.AuthEnabled && strings.TrimSpace(in.AuthUser) == "" {
		in.AuthUser = store.DefaultAuthUser
	}
	if err := validateSettings(in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.st.SaveSettings(in); err != nil {
		writeError(w, http.StatusInternalServerError, "save settings: "+err.Error())
		return
	}
	saved := s.st.Settings()
	// A password change already resets sessions; also invalidate them when
	// login protection is switched on/off or the login name changes, so a
	// re-enabled instance cannot resurrect old tokens.
	if before.AuthEnabled != saved.AuthEnabled || before.AuthUser != saved.AuthUser {
		s.auth.resetAll()
	}
	if !before.AuthEnabled && saved.AuthEnabled {
		s.st.Log("info", "auth", "login protection enabled")
	}
	if before.AuthEnabled && !saved.AuthEnabled {
		s.st.Log("info", "auth", "login protection disabled")
	}
	// Nothing to restart: the syncer reads the latest settings from the store
	// at the start of every loop iteration and before each download.
	s.st.Log("info", "settings", "settings updated")
	writeOK(w, http.StatusOK, s.publicSettings())
}

// validateSettings rejects values that would make the syncer loop misbehave.
// Incomplete settings are allowed so the UI can save a partially filled form.
func validateSettings(v store.Settings) error {
	switch {
	case v.AdminPort < 0 || v.AdminPort > 65535:
		return fmt.Errorf("admin_port must be between 0 and 65535")
	case v.AutoMinutes < 0:
		return fmt.Errorf("auto_minutes must not be negative")
	case v.RetentionD < 0:
		return fmt.Errorf("retention_days must not be negative")
	case v.DownloadWorkers < 0 || v.DownloadWorkers > 4:
		// The device serves at most 8 media sessions and one download already
		// saturates its 100 Mbit link, so more workers only add contention.
		return fmt.Errorf("download_workers must be between 0 and 4")
	case v.CutoffDays < 0:
		return fmt.Errorf("cutoff_days must not be negative")
	case v.CatalogDays < 0:
		return fmt.Errorf("catalog_days must not be negative")
	case v.CatalogMinutes < 0:
		return fmt.Errorf("catalog_minutes must not be negative")
	}
	switch strings.TrimSpace(v.Cutoff) {
	case "", "latest", "yesterday", "days":
	default:
		return fmt.Errorf("cutoff must be one of latest, yesterday or days")
	}
	for _, ch := range v.Channels {
		if ch <= 0 {
			return fmt.Errorf("channels must contain positive channel numbers, got %d", ch)
		}
	}
	for _, ch := range v.CatalogChannels {
		if ch <= 0 {
			return fmt.Errorf("catalog_channels must contain positive channel numbers, got %d", ch)
		}
	}
	if len(v.Channels) > 64 || len(v.CatalogChannels) > 64 {
		return fmt.Errorf("at most 64 channels may be configured")
	}
	if user := strings.TrimSpace(v.AuthUser); len(user) > 64 || strings.ContainsAny(user, "\r\n\t") {
		return fmt.Errorf("auth_user must be at most 64 characters and contain no control characters")
	}
	return nil
}

func (s *server) handleNVRTest(w http.ResponseWriter, r *http.Request) {
	set := s.st.Settings()
	if strings.TrimSpace(set.NVRHost) == "" {
		writeError(w, http.StatusBadRequest, "nvr_host is not configured")
		return
	}

	ch := 1
	if len(set.Channels) > 0 {
		ch = set.Channels[0]
	}
	to := time.Now()
	from := to.Add(-10 * time.Minute)

	// A connection test is a real NVR activity (one login plus a media list
	// query), so it must take the same global session lock as a sync, scan or
	// catalogue refresh: clicking it mid-sync used to open a second session
	// on a device with only stream_max_sessions=8 and earn a -40401. It
	// queues behind the current holder and gives up when the client goes
	// away.
	var (
		segments []nvr.MediaSeg
		loginErr error
		listErr  error
	)
	err := s.sy.WithNVRSession(r.Context(), "test", func() error {
		client := nvr.New(set.NVRHost, set.NVRUser, set.NVRPass, set.AdminPort)
		if err := client.Login(); err != nil {
			loginErr = err
			return nil
		}
		// ErrNoMedia just means "nothing recorded in the last 10 minutes",
		// which is still a successful connection test.
		segments, listErr = client.MediaListContext(r.Context(), ch, from.Unix(), to.Unix())
		if errors.Is(listErr, nvr.ErrNoMedia) {
			listErr = nil
		}
		return nil
	})
	if err != nil {
		// Only a failed lock acquisition lands here (viewer gone or server
		// shutting down); the device was never touched.
		writeError(w, http.StatusBadGateway, "nvr session: "+err.Error())
		return
	}
	if loginErr != nil {
		writeError(w, http.StatusBadGateway, "login failed: "+loginErr.Error())
		return
	}
	if listErr != nil {
		writeError(w, http.StatusBadGateway, "media list failed: "+listErr.Error())
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
