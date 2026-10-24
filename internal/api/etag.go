package api

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"path"
	"strings"
	"sync"
)

// assetETagCache stores the content hash of embedded assets. The embedded
// filesystem has no meaningful modification time, so without this the browser
// had to download app.js/style.css in full on every reload.
var assetETagCache sync.Map // name -> etag

// assetETag returns a strong ETag for a name inside the embedded assets.
func (s *server) assetETag(name string) (string, bool) {
	name = strings.TrimPrefix(path.Clean("/"+name), "/")
	if name == "" || name == "." {
		return "", false
	}
	if v, ok := assetETagCache.Load(name); ok {
		return v.(string), true
	}
	data, err := fs.ReadFile(s.assets, name)
	if err != nil {
		return "", false
	}
	sum := sha256.Sum256(data)
	etag := `"` + hex.EncodeToString(sum[:16]) + `"`
	assetETagCache.Store(name, etag)
	return etag, true
}

// etagMatches handles the common If-None-Match forms: a single tag, a list, a
// weak tag, or "*".
func etagMatches(header, etag string) bool {
	header = strings.TrimSpace(header)
	if header == "" {
		return false
	}
	if header == "*" {
		return true
	}
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		part = strings.TrimPrefix(part, "W/")
		if part == etag {
			return true
		}
	}
	return false
}
