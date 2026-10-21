// Package play builds HLS playlists for locally synced NVR recordings.
package play

import (
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/myml/nvrsync/internal/store"
)

const (
	dateLayout = "2006-01-02"

	// gapTolerance is the largest gap between two consecutive segments that
	// is still treated as continuous. One second absorbs the rounding used by
	// NVRs that report times at whole-second precision.
	gapTolerance = int64(1)
)

var (
	// ErrNotSupported is returned by EnsureProxy while ffmpeg transcoding is
	// not implemented. Playback uses the original TS files directly.
	ErrNotSupported = errors.New("play: transcode proxy is not supported")

	// ErrInvalidDate is returned when a playlist is requested for a malformed
	// or impossible calendar date.
	ErrInvalidDate = errors.New("play: invalid date")

	// ErrInvalidChannel is returned when a playlist is requested for a
	// non-positive channel number.
	ErrInvalidChannel = errors.New("play: invalid channel")
)

// Player produces playlists from the store. It is safe for concurrent use as
// long as the store is.
type Player struct {
	st *store.Store
}

// New returns a Player backed by st.
func New(st *store.Store) *Player {
	return &Player{st: st}
}

// DayPlaylist renders a VOD HLS playlist for every finished segment of ch
// whose start time falls on date (local time, YYYY-MM-DD).
//
// Segments are referenced directly as /media/camN/<date>/<file>; no caching
// or transcoding happens here. Discontinuities are inserted only when there is
// a real time gap between neighbouring segments.
func (p *Player) DayPlaylist(ch int, date string) ([]byte, error) {
	if ch <= 0 {
		return nil, fmt.Errorf("%w: %d", ErrInvalidChannel, ch)
	}
	day, err := time.ParseInLocation(dateLayout, date, time.Local)
	if err != nil {
		return nil, fmt.Errorf("%w: %q", ErrInvalidDate, date)
	}
	from := day.Unix()
	to := day.AddDate(0, 0, 1).Unix()

	// The store filter is deliberately broad (whole local day); the loop below
	// defensively re-checks channel, status and bounds so a store whose
	// from/to semantics are inclusive/exclusive cannot spill other days in.
	found, _ := p.st.Segments(store.SegFilter{
		Ch:     ch,
		From:   from,
		To:     to,
		Status: "done",
	})

	segments := make([]store.Segment, 0, len(found))
	for _, seg := range found {
		if seg.Ch != ch || seg.Status != "done" {
			continue
		}
		if seg.Start < from || seg.Start >= to || seg.End <= seg.Start {
			continue
		}
		segments = append(segments, seg)
	}
	sort.SliceStable(segments, func(i, j int) bool {
		if segments[i].Start == segments[j].Start {
			return segments[i].End < segments[j].End
		}
		return segments[i].Start < segments[j].Start
	})

	var maxDuration int64
	for _, seg := range segments {
		if d := seg.End - seg.Start; d > maxDuration {
			maxDuration = d
		}
	}
	targetDuration := maxDuration
	if targetDuration < 1 {
		targetDuration = 1
	}

	var b strings.Builder
	b.Grow(128 + len(segments)*96)
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:3\n")
	b.WriteString("#EXT-X-PLAYLIST-TYPE:VOD\n")
	b.WriteString("#EXT-X-TARGETDURATION:")
	b.WriteString(strconv.FormatInt(targetDuration, 10))
	b.WriteByte('\n')
	b.WriteString("#EXT-X-MEDIA-SEQUENCE:0\n")

	var prev *store.Segment
	for i := range segments {
		seg := &segments[i]
		if prev != nil && seg.Start > prev.End+gapTolerance {
			b.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		b.WriteString("#EXTINF:")
		b.WriteString(strconv.FormatFloat(float64(seg.End-seg.Start), 'f', 3, 64))
		b.WriteString(",\n")
		b.WriteString(mediaURL(ch, date, segmentFile(*seg)))
		b.WriteByte('\n')
		prev = seg
	}

	b.WriteString("#EXT-X-ENDLIST\n")
	return []byte(b.String()), nil
}

// EnsureProxy prepares an optional H.264/HLS proxy for ch on date. Transcoding
// is not implemented yet, so callers should fall back to the original TS
// playlist returned by DayPlaylist.
func (p *Player) EnsureProxy(ch int, date string) error {
	if ch <= 0 {
		return fmt.Errorf("%w: %d", ErrInvalidChannel, ch)
	}
	if _, err := time.ParseInLocation(dateLayout, date, time.Local); err != nil {
		return fmt.Errorf("%w: %q", ErrInvalidDate, date)
	}
	return ErrNotSupported
}

// mediaURL is the URL the browser/hls.js uses for a raw segment. The media
// handler validates every component again before touching the filesystem.
func mediaURL(ch int, date, file string) string {
	return "/media/cam" + strconv.Itoa(ch) + "/" + date + "/" + url.PathEscape(file)
}

// segmentFile derives the on-disk file name from the segment path recorded by
// the syncer. When a path is missing (for example an old store), the canonical
// HH-MM-SS_HH-MM-SS.ts name used by the syncer is reconstructed.
func segmentFile(seg store.Segment) string {
	if seg.Path != "" {
		base := filepath.Base(filepath.Clean(seg.Path))
		if base != "." && base != ".." && base != string(filepath.Separator) {
			return base
		}
	}
	start := time.Unix(seg.Start, 0).Format("15-04-05")
	end := time.Unix(seg.End, 0).Format("15-04-05")
	return start + "_" + end + ".ts"
}
