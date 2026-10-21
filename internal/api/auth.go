package api

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/myml/nvrsync/internal/store"
)

const (
	// sessionCookieName is the HttpOnly cookie holding the session token.
	sessionCookieName = "nvrsync_session"

	// sessionTTL is how long a login stays valid. Sessions live in memory
	// only, so a process restart logs everybody out.
	sessionTTL = 24 * time.Hour

	// maxLoginFailures is the number of consecutive failures from one IP
	// after which login is locked for loginLockout.
	maxLoginFailures = 5
	loginLockout     = 60 * time.Second

	// staleFailureTTL bounds how long failure bookkeeping is kept for an IP
	// that stopped trying.
	staleFailureTTL = 10 * time.Minute

	// loginFile is the standalone login page in web/assets.
	loginFile = "login.html"

	// bcryptMaxPasswordBytes is the longest input bcrypt uses; longer
	// passwords would be silently truncated.
	bcryptMaxPasswordBytes = 72
)

// sessionInfo is one logged-in session. Tokens are random and never derived
// from the password.
type sessionInfo struct {
	user   string
	expiry time.Time
}

// failureState tracks consecutive login failures for one client IP.
type failureState struct {
	count       int
	lockedUntil time.Time
	lastSeen    time.Time
}

// authManager keeps sessions and login rate-limit state in memory. It is safe
// for concurrent use.
type authManager struct {
	mu       sync.Mutex
	sessions map[string]sessionInfo
	failures map[string]*failureState

	// now is replaced in tests to exercise expiry without sleeping.
	now func() time.Time
}

func newAuthManager() *authManager {
	return &authManager{
		sessions: make(map[string]sessionInfo),
		failures: make(map[string]*failureState),
		now:      time.Now,
	}
}

// newSession creates a random 32-byte token, stores it and returns it.
func (a *authManager) newSession(user string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("auth: generate session token: %w", err)
	}
	token := hex.EncodeToString(raw)

	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.purgeLocked(now)
	a.sessions[token] = sessionInfo{user: user, expiry: now.Add(sessionTTL)}
	return token, nil
}

// validate returns the session user when token is live.
func (a *authManager) validate(token string) (string, bool) {
	if token == "" {
		return "", false
	}
	now := a.now()

	a.mu.Lock()
	defer a.mu.Unlock()
	info, ok := a.sessions[token]
	if !ok {
		return "", false
	}
	if !now.Before(info.expiry) {
		delete(a.sessions, token)
		return "", false
	}
	return info.user, true
}

func (a *authManager) remove(token string) {
	if token == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.sessions, token)
}

// resetAll invalidates every session, e.g. after a password change.
func (a *authManager) resetAll() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sessions = make(map[string]sessionInfo)
}

// lockedFor reports how much longer ip must wait before another login attempt.
// Zero means the IP may try now.
func (a *authManager) lockedFor(ip string) time.Duration {
	now := a.now()

	a.mu.Lock()
	defer a.mu.Unlock()
	state, ok := a.failures[ip]
	if !ok {
		return 0
	}
	if state.lockedUntil.IsZero() {
		// Failures are accumulating but the limit has not been reached yet.
		return 0
	}
	if now.Before(state.lockedUntil) {
		return state.lockedUntil.Sub(now)
	}
	// The lock has expired: start from a clean slate.
	delete(a.failures, ip)
	return 0
}

func (a *authManager) recordFailure(ip string) {
	now := a.now()

	a.mu.Lock()
	defer a.mu.Unlock()
	state, ok := a.failures[ip]
	if !ok {
		state = &failureState{}
		a.failures[ip] = state
	}
	state.count++
	state.lastSeen = now
	if state.count >= maxLoginFailures {
		state.lockedUntil = now.Add(loginLockout)
		// Start counting again once the lock expires, otherwise a single
		// later failure would immediately lock the IP again forever.
		state.count = 0
	}
	a.purgeLocked(now)
}

func (a *authManager) recordSuccess(ip string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.failures, ip)
}

// purgeLocked drops expired sessions and stale rate-limit state. The caller
// must hold a.mu.
func (a *authManager) purgeLocked(now time.Time) {
	for token, info := range a.sessions {
		if !now.Before(info.expiry) {
			delete(a.sessions, token)
		}
	}
	for ip, state := range a.failures {
		expiredLock := !state.lockedUntil.IsZero() && !now.Before(state.lockedUntil)
		if expiredLock || now.Sub(state.lastSeen) > staleFailureTTL {
			delete(a.failures, ip)
		}
	}
}

// --- HTTP handlers ----------------------------------------------------------

// handleLogin verifies the password and issues a session cookie.
func (s *server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		User     string `json:"user"`
		Password string `json:"password"`
	}
	if err := decodeJSON(w, r, &req, false); err != nil {
		writeError(w, http.StatusBadRequest, "invalid login request: "+err.Error())
		return
	}

	ip := clientIP(r)
	if wait := s.auth.lockedFor(ip); wait > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
		s.st.Log("warn", "auth", "login rate limited from "+ip)
		writeError(w, http.StatusTooManyRequests, "too many failed login attempts, try again later")
		return
	}

	set := s.st.Settings()
	if !set.AuthEnabled {
		writeError(w, http.StatusBadRequest, "login protection is not enabled")
		return
	}

	// Always run bcrypt when a hash exists, whatever the username, so a wrong
	// username does not answer measurably faster than a wrong password.
	hash := s.st.AuthHash()
	passwordOK := hash != "" && bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password)) == nil
	userOK := strings.TrimSpace(req.User) == effectiveAuthUser(set)
	if !passwordOK || !userOK {
		s.auth.recordFailure(ip)
		// Never log the submitted password or hash.
		s.st.Log("warn", "auth", "login failed from "+ip)
		writeError(w, http.StatusUnauthorized, "invalid username or password")
		return
	}

	token, err := s.auth.newSession(strings.TrimSpace(req.User))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "create session failed")
		return
	}
	s.auth.recordSuccess(ip)
	s.setSessionCookie(w, token)
	s.st.Log("info", "auth", "login succeeded for "+strings.TrimSpace(req.User)+" from "+ip)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleLogout discards the current session and clears the cookie.
func (s *server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		s.auth.remove(cookie.Value)
	}
	s.clearSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleAuthPassword sets or changes the login password.
//
// While login protection is disabled only the password is set (old_password is
// ignored). Once enabled, the current password must be supplied and the caller
// must be logged in; the middleware already guarantees the latter.
func (s *server) handleAuthPassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		User        string `json:"user"`
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := decodeJSON(w, r, &req, false); err != nil {
		writeError(w, http.StatusBadRequest, "invalid password request: "+err.Error())
		return
	}

	set := s.st.Settings()
	hash := s.st.AuthHash()
	if set.AuthEnabled {
		if hash == "" {
			writeError(w, http.StatusBadRequest, "no password is configured")
			return
		}
		if bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.OldPassword)) != nil {
			s.st.Log("warn", "auth", "password change rejected: wrong current password")
			writeError(w, http.StatusBadRequest, "current password is incorrect")
			return
		}
	}

	if req.NewPassword == "" {
		writeError(w, http.StatusBadRequest, "new_password must not be empty")
		return
	}
	if len(req.NewPassword) > bcryptMaxPasswordBytes {
		writeError(w, http.StatusBadRequest, "new_password must be at most 72 bytes")
		return
	}

	user := strings.TrimSpace(req.User)
	if user == "" {
		user = effectiveAuthUser(set)
	}
	if len(user) > 64 || strings.ContainsAny(user, "\r\n\t") {
		writeError(w, http.StatusBadRequest, "invalid user name")
		return
	}

	newHash, err := bcrypt.GenerateFromPassword([]byte(req.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "hash password failed")
		return
	}
	if err := s.st.SetPassword(user, string(newHash)); err != nil {
		writeError(w, http.StatusInternalServerError, "save password: "+err.Error())
		return
	}

	// Any session created with the old password is no longer valid.
	s.auth.resetAll()
	s.clearSessionCookie(w)
	s.st.Log("info", "auth", "password updated for "+user)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleLoginPage serves the standalone login page. It is reachable without a
// session (the auth middleware exempts /login).
func (s *server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	page, err := fs.ReadFile(s.assets, loginFile)
	if err != nil {
		// The embedded assets normally contain login.html. Keep the recovery
		// path usable even if a custom asset filesystem does not.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "<!doctype html><meta charset=\"utf-8\"><title>登录 · NVR Sync</title>"+
			"<body style=\"font:16px system-ui;padding:2rem;background:#0f1115;color:#e7ebf3\">"+
			"<h1>NVR Sync</h1><p>登录页面资源缺失，请重新构建前端资源。</p>")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(page)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(page)
}

// --- middleware -------------------------------------------------------------

// authMiddleware enforces login protection when it is enabled. With
// AuthEnabled=false it is a no-op, which keeps the historical behaviour.
func (s *server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		set := s.st.Settings()
		if !set.AuthEnabled || isPublicRequest(r) {
			next.ServeHTTP(w, r)
			return
		}

		if token := sessionToken(r); token != "" {
			if user, ok := s.auth.validate(token); ok && user == effectiveAuthUser(set) {
				next.ServeHTTP(w, r)
				return
			}
		}

		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		// Pages and media redirect to the login form. 302 is deliberate: a
		// browser then lands on the form, while an API client uses /api/*.
		http.Redirect(w, r, "/login", http.StatusFound)
	})
}

// isPublicRequest reports whether r is reachable without a session.
func isPublicRequest(r *http.Request) bool {
	switch {
	case r.URL.Path == "/login":
		return true
	case strings.HasPrefix(r.URL.Path, "/assets/"):
		// The login page needs its stylesheet; these files are static and
		// contain no private data.
		return true
	case r.URL.Path == "/api/login" && r.Method == http.MethodPost:
		return true
	case r.URL.Path == "/api/health" && r.Method == http.MethodGet:
		return true
	}
	return false
}

func (s *server) setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   int(sessionTTL / time.Second),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

func sessionToken(r *http.Request) string {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return ""
	}
	return cookie.Value
}

// clientIP returns the remote IP without the port. X-Forwarded-For is
// deliberately ignored: the rate limit must not be bypassable by a header.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return strings.TrimSpace(r.RemoteAddr)
	}
	return host
}

// effectiveAuthUser returns the configured login name or the default.
func effectiveAuthUser(set store.Settings) string {
	if user := strings.TrimSpace(set.AuthUser); user != "" {
		return user
	}
	return store.DefaultAuthUser
}
