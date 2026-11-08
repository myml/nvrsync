package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	camPattern  = regexp.MustCompile(`^(?:cam)?([1-9][0-9]*)$`)
	datePattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
)

const (
	dateLayout = "2006-01-02"

	maxBodyBytes = 1 << 20 // 1 MiB is plenty for settings and sync requests
)

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
	// A non-JSON content type is a good sign of a cross-site form post. The
	// browser JSON API always sends application/json.
	if ct := strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]); ct != "" &&
		!strings.EqualFold(ct, "application/json") {
		return errors.New("Content-Type must be application/json")
	}
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
