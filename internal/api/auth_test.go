package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/myml/nvrsync/internal/play"
	"github.com/myml/nvrsync/internal/store"
	"github.com/myml/nvrsync/internal/syncer"
)

const testClientIP = "192.0.2.10"

type testAPI struct {
	t  *testing.T
	st *store.Store
	h  http.Handler
}

func newTestAPI(t *testing.T) *testAPI {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	})
	return &testAPI{
		t:  t,
		st: st,
		h:  New(st, syncer.New(st), play.New(st), nil, "test"),
	}
}

// enableAuth seeds a bcrypt password and turns login protection on. MinCost is
// used because these tests care about the auth flow, not bcrypt's work factor.
func (a *testAPI) enableAuth(user, password string) {
	a.t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		a.t.Fatalf("bcrypt: %v", err)
	}
	if err := a.st.SetPassword(user, string(hash)); err != nil {
		a.t.Fatalf("SetPassword: %v", err)
	}
	set := a.st.Settings()
	set.AuthEnabled = true
	if err := a.st.SaveSettings(set); err != nil {
		a.t.Fatalf("SaveSettings: %v", err)
	}
}

func (a *testAPI) do(method, path string, body any, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	return a.doFrom(testClientIP, method, path, body, cookies...)
}

func (a *testAPI) doFrom(ip, method, path string, body any, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	a.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			a.t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, reader)
	req.RemoteAddr = ip + ":54321"
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, cookie := range cookies {
		if cookie != nil {
			req.AddCookie(cookie)
		}
	}
	rec := httptest.NewRecorder()
	a.h.ServeHTTP(rec, req)
	return rec
}

func (a *testAPI) login(user, password string) *http.Cookie {
	a.t.Helper()
	rec := a.do("POST", "/api/login", map[string]string{"user": user, "password": password})
	if rec.Code != http.StatusOK {
		a.t.Fatalf("login status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	return sessionCookie(a.t, rec)
}

func sessionCookie(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == sessionCookieName {
			return cookie
		}
	}
	t.Fatalf("response has no %s cookie; headers=%v", sessionCookieName, rec.Header())
	return nil
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
	return body
}

func TestAuthDisabledKeepsCurrentBehaviour(t *testing.T) {
	a := newTestAPI(t)

	for _, path := range []string{"/api/settings", "/", "/sync", "/recordings", "/settings", "/login"} {
		rec := a.do("GET", path, nil)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, rec.Code)
		}
	}
	if rec := a.do("GET", "/api/health", nil); rec.Code != http.StatusOK {
		t.Errorf("GET /api/health = %d, want 200", rec.Code)
	}
	// The missing media file is a 404, not a redirect: auth is off.
	if rec := a.do("GET", "/media/cam1/2026-10-21/missing.ts", nil); rec.Code != http.StatusNotFound {
		t.Errorf("GET /media = %d, want 404", rec.Code)
	}
	// Login is meaningless while protection is off.
	if rec := a.do("POST", "/api/login", map[string]string{"user": "admin", "password": "x"}); rec.Code != http.StatusBadRequest {
		t.Errorf("POST /api/login while disabled = %d, want 400", rec.Code)
	}
}

func TestSettingsAPIExposesNoAuthHash(t *testing.T) {
	a := newTestAPI(t)

	const hash = "a-very-secret-bcrypt-hash"
	if err := a.st.SetPassword("admin", hash); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}

	rec := a.do("GET", "/api/settings", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/settings = %d, want 200", rec.Code)
	}
	if strings.Contains(rec.Body.String(), hash) {
		t.Fatalf("GET /api/settings leaked the auth hash: %s", rec.Body.String())
	}
	body := decodeBody(t, rec)
	data, _ := body["data"].(map[string]any)
	if data == nil {
		t.Fatalf("response has no data object: %s", rec.Body.String())
	}
	if got, _ := data["auth_hash"].(string); got != "" {
		t.Errorf("auth_hash = %q, want empty", got)
	}
}

func TestAuthMiddleware(t *testing.T) {
	a := newTestAPI(t)
	a.enableAuth("admin", "correct horse")

	pages := []string{
		"/", "/sync", "/recordings", "/settings",
		"/media/cam1/2026-10-21/missing.ts",
	}
	for _, path := range pages {
		rec := a.do("GET", path, nil)
		if rec.Code != http.StatusFound {
			t.Errorf("GET %s without session = %d, want 302", path, rec.Code)
		}
		if loc := rec.Header().Get("Location"); loc != "/login" {
			t.Errorf("GET %s redirect Location = %q, want /login", path, loc)
		}
	}

	for _, path := range []string{
		"/api/settings", "/api/segments", "/api/sync/status", "/api/logs", "/api/days",
	} {
		rec := a.do("GET", path, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without session = %d, want 401", path, rec.Code)
			continue
		}
		body := decodeBody(t, rec)
		if body["ok"] != false || body["error"] != "unauthorized" {
			t.Errorf("GET %s body = %v, want {ok:false,error:unauthorized}", path, body)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
			t.Errorf("GET %s Content-Type = %q, want JSON", path, ct)
		}
	}

	// Public endpoints stay reachable.
	for _, path := range []string{"/login", "/assets/login.html", "/assets/style.css", "/api/health"} {
		if rec := a.do("GET", path, nil); rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, rec.Code)
		}
	}
	loginPage := a.do("GET", "/login", nil)
	if !strings.Contains(loginPage.Body.String(), "/api/login") {
		t.Error("login page does not post to /api/login")
	}

	// After logging in, API and pages work; a missing media file is a 404.
	cookie := a.login("admin", "correct horse")
	if rec := a.do("GET", "/api/settings", nil, cookie); rec.Code != http.StatusOK {
		t.Errorf("GET /api/settings with session = %d, want 200", rec.Code)
	}
	if rec := a.do("GET", "/settings", nil, cookie); rec.Code != http.StatusOK {
		t.Errorf("GET /settings with session = %d, want 200", rec.Code)
	}
	if rec := a.do("GET", "/media/cam1/2026-10-21/missing.ts", nil, cookie); rec.Code != http.StatusNotFound {
		t.Errorf("GET /media with session = %d, want 404", rec.Code)
	}

	// A token that was never issued does not authenticate.
	bogus := &http.Cookie{Name: sessionCookieName, Value: "deadbeef"}
	if rec := a.do("GET", "/api/settings", nil, bogus); rec.Code != http.StatusUnauthorized {
		t.Errorf("GET /api/settings with bogus session = %d, want 401", rec.Code)
	}
}

func TestAuthLogsNeverContainPasswords(t *testing.T) {
	a := newTestAPI(t)
	a.enableAuth("admin", "super-secret-pw")

	a.do("POST", "/api/login", map[string]string{"user": "admin", "password": "wrong-password-xyz"})
	a.login("admin", "super-secret-pw")

	sawFailure, sawSuccess := false, false
	for _, entry := range a.st.Logs(0, "") {
		if entry.Kind != "auth" {
			continue
		}
		if strings.Contains(entry.Msg, "wrong-password-xyz") ||
			strings.Contains(entry.Msg, "super-secret-pw") {
			t.Fatalf("auth log leaked a password: %q", entry.Msg)
		}
		if strings.Contains(entry.Msg, "login failed") {
			sawFailure = true
		}
		if strings.Contains(entry.Msg, "login succeeded") {
			sawSuccess = true
		}
	}
	if !sawFailure || !sawSuccess {
		t.Errorf("auth logs missing events: failure=%v success=%v", sawFailure, sawSuccess)
	}
}

func TestAuthLoginLogout(t *testing.T) {
	a := newTestAPI(t)
	a.enableAuth("admin", "s3cret-pass")

	rec := a.do("POST", "/api/login", map[string]string{"user": "admin", "password": "wrong"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password login = %d, want 401", rec.Code)
	}
	body := decodeBody(t, rec)
	if body["ok"] != false || body["error"] != "invalid username or password" {
		t.Errorf("wrong password body = %v", body)
	}

	rec = a.do("POST", "/api/login", map[string]string{"user": "nobody", "password": "s3cret-pass"})
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong username login = %d, want 401", rec.Code)
	}

	rec = a.do("POST", "/api/login", map[string]string{"user": "admin", "password": "s3cret-pass"})
	if rec.Code != http.StatusOK {
		t.Fatalf("login = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body = decodeBody(t, rec)
	if body["ok"] != true {
		t.Errorf("login body = %v, want {ok:true}", body)
	}
	cookie := sessionCookie(t, rec)
	if !cookie.HttpOnly {
		t.Error("session cookie is not HttpOnly")
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("session cookie SameSite = %v, want Lax", cookie.SameSite)
	}
	if cookie.Path != "/" {
		t.Errorf("session cookie Path = %q, want /", cookie.Path)
	}
	if cookie.MaxAge != int(sessionTTL/time.Second) {
		t.Errorf("session cookie MaxAge = %d, want %d", cookie.MaxAge, int(sessionTTL/time.Second))
	}
	if len(cookie.Value) != 64 {
		t.Errorf("session token length = %d, want 64 hex chars", len(cookie.Value))
	}

	if rec := a.do("GET", "/api/settings", nil, cookie); rec.Code != http.StatusOK {
		t.Fatalf("GET /api/settings with session = %d, want 200", rec.Code)
	}

	rec = a.do("POST", "/api/logout", nil, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("logout = %d, want 200", rec.Code)
	}
	if body := decodeBody(t, rec); body["ok"] != true {
		t.Errorf("logout body = %v", body)
	}
	cleared := sessionCookie(t, rec)
	if cleared.MaxAge >= 0 || cleared.Value != "" {
		t.Errorf("logout did not clear the cookie: %+v", cleared)
	}
	if rec := a.do("GET", "/api/settings", nil, cookie); rec.Code != http.StatusUnauthorized {
		t.Errorf("GET /api/settings after logout = %d, want 401", rec.Code)
	}
}

func TestAuthRateLimit(t *testing.T) {
	a := newTestAPI(t)
	a.enableAuth("admin", "right-password")

	const ip = "198.51.100.7"
	for i := 0; i < maxLoginFailures; i++ {
		rec := a.doFrom(ip, "POST", "/api/login", map[string]string{"user": "admin", "password": "bad"})
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d = %d, want 401", i+1, rec.Code)
		}
	}

	// Even the right password is rejected while the IP is locked.
	rec := a.doFrom(ip, "POST", "/api/login", map[string]string{"user": "admin", "password": "right-password"})
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("locked login = %d, want 429", rec.Code)
	}
	if retry := rec.Header().Get("Retry-After"); retry == "" {
		t.Error("429 response has no Retry-After header")
	}

	// Another IP is unaffected.
	if cookie := a.doFrom("203.0.113.9", "POST", "/api/login",
		map[string]string{"user": "admin", "password": "right-password"}); cookie.Code != http.StatusOK {
		t.Errorf("login from another IP = %d, want 200", cookie.Code)
	}
}

func TestAuthRateLimitExpiry(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	m := newAuthManager()
	m.now = func() time.Time { return now }

	const ip = "192.0.2.99"
	for i := 0; i < maxLoginFailures; i++ {
		m.recordFailure(ip)
	}
	if wait := m.lockedFor(ip); wait <= 0 || wait > loginLockout {
		t.Fatalf("lockedFor = %v, want (0, %v]", wait, loginLockout)
	}

	// Four successes worth of failures reset the counter.
	m.recordSuccess(ip)
	for i := 0; i < maxLoginFailures-1; i++ {
		m.recordFailure(ip)
	}
	if wait := m.lockedFor(ip); wait != 0 {
		t.Fatalf("lockedFor after success reset = %v, want 0", wait)
	}

	// Recreate the lock and let it expire.
	for i := 0; i < maxLoginFailures; i++ {
		m.recordFailure(ip)
	}
	now = now.Add(loginLockout + time.Second)
	if wait := m.lockedFor(ip); wait != 0 {
		t.Fatalf("lockedFor after expiry = %v, want 0", wait)
	}
	// A single new failure must not relock immediately.
	m.recordFailure(ip)
	if wait := m.lockedFor(ip); wait != 0 {
		t.Errorf("lockedFor after one post-expiry failure = %v, want 0", wait)
	}
}

func TestSessionExpiry(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	m := newAuthManager()
	m.now = func() time.Time { return now }

	token, err := m.newSession("admin")
	if err != nil {
		t.Fatalf("newSession: %v", err)
	}
	if user, ok := m.validate(token); !ok || user != "admin" {
		t.Fatalf("validate fresh session = %q, %v", user, ok)
	}

	now = now.Add(sessionTTL - time.Second)
	if _, ok := m.validate(token); !ok {
		t.Error("session expired one second early")
	}
	now = now.Add(time.Second)
	if _, ok := m.validate(token); ok {
		t.Error("session still valid after its TTL")
	}
}

func TestAuthPassword(t *testing.T) {
	a := newTestAPI(t)

	// While auth is disabled the password may be set without an old one.
	rec := a.do("POST", "/api/auth/password", map[string]string{
		"user": "admin", "new_password": "first-password",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("set password = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if hash := a.st.AuthHash(); hash == "" {
		t.Fatal("password was not stored")
	} else if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte("first-password")); err != nil {
		t.Fatalf("stored hash does not match the new password: %v", err)
	}
	if enabled := a.st.Settings().AuthEnabled; enabled {
		t.Error("setting a password must not enable login protection by itself")
	}

	// Enabling without a password is rejected; with one it succeeds. Here the
	// password exists, so this is the successful path.
	set := a.st.Settings()
	set.AuthEnabled = true
	if rec := a.do("PUT", "/api/settings", set); rec.Code != http.StatusOK {
		t.Fatalf("enable auth = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	cookie := a.login("admin", "first-password")

	// Wrong current password is rejected and does not disturb the session.
	rec = a.do("POST", "/api/auth/password", map[string]string{
		"user": "admin", "old_password": "nope", "new_password": "second-password",
	}, cookie)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("password change with wrong old password = %d, want 400", rec.Code)
	}
	if rec := a.do("GET", "/api/settings", nil, cookie); rec.Code != http.StatusOK {
		t.Errorf("session was disturbed by a rejected password change: %d", rec.Code)
	}

	// A valid change succeeds and invalidates every session.
	rec = a.do("POST", "/api/auth/password", map[string]string{
		"user": "admin", "old_password": "first-password", "new_password": "second-password",
	}, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("password change = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if rec := a.do("GET", "/api/settings", nil, cookie); rec.Code != http.StatusUnauthorized {
		t.Errorf("old session still valid after password change: %d", rec.Code)
	}
	if rec := a.do("POST", "/api/login", map[string]string{
		"user": "admin", "password": "first-password",
	}); rec.Code != http.StatusUnauthorized {
		t.Errorf("old password still works after change: %d", rec.Code)
	}
	a.login("admin", "second-password")

	// Basic input validation.
	bad := []map[string]string{
		{"user": "admin", "old_password": "second-password", "new_password": ""},
		{"user": "admin", "old_password": "second-password", "new_password": strings.Repeat("x", bcryptMaxPasswordBytes+1)},
	}
	for _, body := range bad {
		fresh := a.login("admin", "second-password")
		if rec := a.do("POST", "/api/auth/password", body, fresh); rec.Code != http.StatusBadRequest {
			t.Errorf("invalid new password %q = %d, want 400", body["new_password"], rec.Code)
		}
	}
}

func TestEnableAuthRequiresPassword(t *testing.T) {
	a := newTestAPI(t)

	set := a.st.Settings()
	set.AuthEnabled = true
	rec := a.do("PUT", "/api/settings", set)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("enable auth without password = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "password") {
		t.Errorf("error does not explain the missing password: %s", rec.Body.String())
	}
	if a.st.Settings().AuthEnabled {
		t.Error("settings were saved despite the validation error")
	}
}

func TestDisablingAuthResetsSessions(t *testing.T) {
	a := newTestAPI(t)
	a.enableAuth("admin", "pass-1234")
	cookie := a.login("admin", "pass-1234")

	set := a.st.Settings()
	set.AuthEnabled = false
	if rec := a.do("PUT", "/api/settings", set, cookie); rec.Code != http.StatusOK {
		t.Fatalf("disable auth = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if a.st.Settings().AuthEnabled {
		t.Fatal("auth is still enabled")
	}
	// Disabling keeps the hash, so re-enabling does not need a new password.
	if a.st.AuthHash() == "" {
		t.Fatal("auth hash was discarded while disabling")
	}
	// No login is needed now.
	if rec := a.do("GET", "/api/settings", nil); rec.Code != http.StatusOK {
		t.Errorf("GET settings with auth disabled = %d, want 200", rec.Code)
	}

	// Re-enable and make sure the pre-disable cookie was not resurrected.
	set = a.st.Settings()
	set.AuthEnabled = true
	if rec := a.do("PUT", "/api/settings", set); rec.Code != http.StatusOK {
		t.Fatalf("re-enable auth = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if rec := a.do("GET", "/api/settings", nil, cookie); rec.Code != http.StatusUnauthorized {
		t.Errorf("session from before disabling came back to life: %d", rec.Code)
	}
}

func TestAuthUserChangeInvalidatesSession(t *testing.T) {
	a := newTestAPI(t)
	a.enableAuth("admin", "pass-1234")
	cookie := a.login("admin", "pass-1234")

	set := a.st.Settings()
	set.AuthUser = "operator"
	if rec := a.do("PUT", "/api/settings", set, cookie); rec.Code != http.StatusOK {
		t.Fatalf("change user = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if rec := a.do("GET", "/api/settings", nil, cookie); rec.Code != http.StatusUnauthorized {
		t.Errorf("old session valid after login name change: %d", rec.Code)
	}
	a.login("operator", "pass-1234")
}
