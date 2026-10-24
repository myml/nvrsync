package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestAssetETagReturns304(t *testing.T) {
	a := newTestAPI(t)
	rec := a.do("GET", "/assets/app.js", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET asset = %d", rec.Code)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("asset response has no ETag")
	}

	req := httptest.NewRequest("GET", "/assets/app.js", nil)
	req.Header.Set("If-None-Match", etag)
	rec = httptest.NewRecorder()
	a.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotModified {
		t.Fatalf("If-None-Match = %d, want 304; body=%q", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Errorf("304 body is not empty: %q", rec.Body.String())
	}
}

func TestReadyEndpoint(t *testing.T) {
	a := newTestAPI(t)
	rec := a.do("GET", "/api/ready", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/ready = %d; body=%s", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	data, _ := body["data"].(map[string]any)
	if data == nil || data["ready"] != true || data["db"] != true {
		t.Fatalf("ready response = %s", rec.Body.String())
	}
}

func TestMediaETagAndCanonicalPath(t *testing.T) {
	a := newTestAPI(t)
	dir := t.TempDir()
	set := a.st.Settings()
	set.StorageDir = dir
	if err := a.st.SaveSettings(set); err != nil {
		t.Fatal(err)
	}
	name := "00-00-00_00-01-00.ts"
	full := filepath.Join(dir, "rec", "cam1", "2026-10-24", name)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	packet := make([]byte, 188)
	packet[0] = 0x47
	if err := os.WriteFile(full, packet, 0o644); err != nil {
		t.Fatal(err)
	}

	rec := a.do("GET", "/media/cam1/2026-10-24/"+name, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET media = %d; body=%s", rec.Code, rec.Body.String())
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("media response has no ETag")
	}

	req := httptest.NewRequest("GET", "/media/cam1/2026-10-24/"+name, nil)
	req.Header.Set("If-None-Match", etag)
	rec = httptest.NewRecorder()
	a.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotModified {
		t.Fatalf("media If-None-Match = %d, want 304; body=%q", rec.Code, rec.Body.String())
	}
}
