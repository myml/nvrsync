package api

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSettingsNeverReturnsNVRPassword(t *testing.T) {
	a := newTestAPI(t)
	set := a.st.Settings()
	set.NVRPass = "s3cret"
	set.Channels = []int{1}
	if err := a.st.SaveSettings(set); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}

	rec := a.do("GET", "/api/settings", nil)
	if rec.Code != 200 {
		t.Fatalf("GET settings = %d; body=%s", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	data, _ := body["data"].(map[string]any)
	if data == nil {
		t.Fatalf("settings response has no data: %s", rec.Body.String())
	}
	if got := data["nvr_pass"]; got != "" {
		t.Errorf("nvr_pass = %q, want empty", got)
	}
	if got := data["nvr_pass_set"]; got != true {
		t.Errorf("nvr_pass_set = %v, want true", got)
	}

	// An empty nvr_pass on PUT means "keep the stored password".
	set = a.st.Settings()
	set.NVRPass = ""
	set.AutoMinutes = 5
	rec = a.do("PUT", "/api/settings", set)
	if rec.Code != 200 {
		t.Fatalf("PUT settings = %d; body=%s", rec.Code, rec.Body.String())
	}
	if got := a.st.Settings().NVRPass; got != "s3cret" {
		t.Errorf("stored NVR password = %q, want it preserved", got)
	}
	if got := a.st.Settings().AutoMinutes; got != 5 {
		t.Errorf("auto_minutes = %d, want 5", got)
	}
}

func TestSettingsRejectsInvalidCutoff(t *testing.T) {
	a := newTestAPI(t)
	set := a.st.Settings()
	set.Cutoff = "sometimes"
	rec := a.do("PUT", "/api/settings", set)
	if rec.Code != 400 {
		t.Fatalf("invalid cutoff = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestCrossOriginMutationRejected(t *testing.T) {
	a := newTestAPI(t)
	req := httptest.NewRequest("PUT", "/api/settings", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	a.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin PUT = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

func TestSecurityHeadersAndCompression(t *testing.T) {
	a := newTestAPI(t)
	rec := a.do("GET", "/api/health", nil)
	if rec.Code != 200 {
		t.Fatalf("GET health = %d", rec.Code)
	}
	for _, h := range []string{"X-Content-Type-Options", "X-Frame-Options", "Content-Security-Policy"} {
		if rec.Header().Get(h) == "" {
			t.Errorf("missing security header %s", h)
		}
	}

	req := httptest.NewRequest("GET", "/api/health", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec = httptest.NewRecorder()
	a.h.ServeHTTP(rec, req)
	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read gzip body: %v", err)
	}
	if !strings.Contains(string(raw), `"ok":true`) {
		t.Errorf("decoded body = %q", raw)
	}
}
