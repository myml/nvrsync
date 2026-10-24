package api

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// contentSecurityPolicy is intentionally strict but still allows the two
// things the UI needs inline: the login page's small style/script block and
// element style attributes set by app.js. hls.js runs its transmuxer in a blob
// worker, hence worker-src blob:.
const contentSecurityPolicy = "default-src 'self'; " +
	"base-uri 'self'; " +
	"form-action 'self'; " +
	"frame-ancestors 'none'; " +
	"object-src 'none'; " +
	"img-src 'self' data:; " +
	"style-src 'self' 'unsafe-inline'; " +
	"script-src 'self' 'unsafe-inline'; " +
	"worker-src 'self' blob:; " +
	"media-src 'self' blob:; " +
	"connect-src 'self'"

// withSecurityHeaders adds the headers browsers use to disable MIME sniffing,
// framing, referrer leakage and unwanted powerful APIs. HSTS is only sent when
// the original request arrived over HTTPS (directly or through the proxy).
func withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		if isHTTPS(r) {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

// withCompression gzips text responses. Media files are deliberately excluded:
// they already dominate bandwidth and Range requests must stay byte-exact.
func withCompression(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead ||
			!acceptsGzip(r) ||
			strings.HasPrefix(r.URL.Path, "/media/") ||
			r.Header.Get("Range") != "" {
			next.ServeHTTP(w, r)
			return
		}
		gw := &gzipResponseWriter{ResponseWriter: w}
		defer gw.Close()
		next.ServeHTTP(gw, r)
	})
}

var gzipPool = sync.Pool{
	New: func() any {
		gz, _ := gzip.NewWriterLevel(io.Discard, gzip.BestSpeed)
		return gz
	},
}

type gzipResponseWriter struct {
	http.ResponseWriter
	gz       *gzip.Writer
	started  bool
	compress bool
}

func (w *gzipResponseWriter) WriteHeader(status int) {
	if w.started {
		return
	}
	w.started = true
	// 204/304 and pre-encoded responses must not be wrapped.
	w.compress = status != http.StatusNoContent &&
		status != http.StatusNotModified &&
		w.Header().Get("Content-Encoding") == ""
	if !w.compress {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	w.Header().Del("Content-Length")
	w.Header().Set("Content-Encoding", "gzip")
	addVary(w.Header(), "Accept-Encoding")
	w.gz = gzipPool.Get().(*gzip.Writer)
	w.gz.Reset(w.ResponseWriter)
	w.ResponseWriter.WriteHeader(status)
}

func (w *gzipResponseWriter) Write(p []byte) (int, error) {
	if !w.started {
		w.WriteHeader(http.StatusOK)
	}
	if !w.compress {
		return w.ResponseWriter.Write(p)
	}
	return w.gz.Write(p)
}

// Close flushes and returns the pooled gzip writer. It is safe to call when
// the handler never wrote a body.
func (w *gzipResponseWriter) Close() {
	if w.gz == nil {
		return
	}
	_ = w.gz.Close()
	gzipPool.Put(w.gz)
	w.gz = nil
}

// Unwrap keeps http.ResponseController (for example Flush or deadlines)
// working through the wrapper.
func (w *gzipResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		part = strings.TrimSpace(part)
		if part == "gzip" || strings.HasPrefix(part, "gzip;") || part == "*" {
			return true
		}
	}
	return false
}

func addVary(h http.Header, value string) {
	for _, existing := range h.Values("Vary") {
		for _, item := range strings.Split(existing, ",") {
			if strings.EqualFold(strings.TrimSpace(item), value) {
				return
			}
		}
	}
	h.Add("Vary", value)
}

// isHTTPS reports whether the request reached us over TLS, either directly or
// through a proxy that sets the conventional forwarding header.
func isHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https")
}

// crossOriginMutation rejects state-changing browser requests whose Origin
// does not match the Host. SameSite=Lax already blocks most cross-site POSTs;
// this is the explicit second line of defence and also catches form posts.
func crossOriginMutation(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	if strings.EqualFold(r.Header.Get("Sec-Fetch-Site"), "cross-site") {
		return true
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return true
	}
	return !strings.EqualFold(u.Host, r.Host)
}

// isQuietRequest suppresses the per-request log line for successful polling
// and static-asset traffic. Errors and slow requests are still logged.
func isQuietRequest(r *http.Request) bool {
	if strings.HasPrefix(r.URL.Path, "/assets/") || strings.HasPrefix(r.URL.Path, "/media/") {
		return true
	}
	switch r.URL.Path {
	case "/api/health", "/api/sync/status":
		return true
	}
	return false
}
