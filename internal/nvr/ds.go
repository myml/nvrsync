package nvr

// /ds management API subset: media.get_media_list (paged) on top of the
// login/session handling in client.go.
//
// Firmware notes (verified on TL-NVR6108C-L 1.0.26):
//   - channel must be a string array (["0"]); times must be strings;
//     numbers make the device answer -71112;
//   - the event_type filter is ignored server-side. We still send
//     [1,2,21] (continuous / activity / composite) and keep whatever the
//     device returns;
//   - results are column arrays (start_time[], end_time[], size[], ...),
//     not an array of objects;
//   - max_num is capped at 100, so the caller must page with start_index;
//   - -71104 (and -40101 on some firmware) means "no index for this range",
//     not a malformed request.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

const (
	mediaPageSize = 100
	mediaMaxIndex = 10000
)

// MediaSeg is one recording segment as indexed by the NVR.
// Start/End are unix seconds, Size is the NVR-side byte count.
type MediaSeg struct {
	Start     int64
	End       int64
	Size      int64
	EventType int
}

// MediaList returns every indexed segment of physical channel ch
// (1-based) in [from, to]. It pages through start_index/max_num and
// de-duplicates, because some firmware repeats the last page.
//
// ErrNoMedia is returned when the device reports no data for the range.
// An empty, non-error result is possible too (e.g. a range whose index is
// not flushed yet).
func (c *Client) MediaList(ch int, from, to int64) ([]MediaSeg, error) {
	return c.MediaListContext(context.Background(), ch, from, to)
}

// MediaListContext is MediaList with cancellation. Stop() uses it so a
// running sync can abort a slow media scan.
func (c *Client) MediaListContext(ctx context.Context, ch int, from, to int64) ([]MediaSeg, error) {
	if to < from {
		return nil, fmt.Errorf("nvr: media list: invalid range %d..%d", from, to)
	}
	uid, err := c.GetUserID()
	if err != nil {
		return nil, err
	}
	apiCh := ch - 1
	if apiCh < 0 {
		apiCh = 0
	}

	var out []MediaSeg
	seen := make(map[MediaSeg]bool)
	for idx := 0; idx < mediaMaxIndex; idx += mediaPageSize {
		raw, err := c.callContext(ctx, map[string]any{
			"media": map[string]any{"get_media_list": map[string]any{
				"channel":     []string{strconv.Itoa(apiCh)},
				"start_time":  strconv.FormatInt(from, 10),
				"end_time":    strconv.FormatInt(to, 10),
				"event_type":  []int{1, 2, 21},
				"media_type":  []int{0},
				"start_index": idx,
				"max_num":     mediaPageSize,
				"user_id":     uid,
			}},
			"method": "do",
		})
		if err != nil {
			return nil, fmt.Errorf("nvr: media list: %w", err)
		}
		page, code, err := parseMediaList(raw)
		if err != nil {
			return nil, err
		}
		if code == -71104 || code == -40101 {
			if len(out) == 0 {
				return nil, ErrNoMedia
			}
			break
		}
		if code != 0 {
			return nil, fmt.Errorf("nvr: media list: error_code=%d (raw %.200s)", code, raw)
		}
		if len(page) == 0 {
			break
		}
		added := 0
		for _, seg := range page {
			if seg.End < seg.Start || seg.Start <= 0 {
				continue
			}
			if seen[seg] {
				continue
			}
			seen[seg] = true
			out = append(out, seg)
			added++
		}
		if len(page) < mediaPageSize || added == 0 {
			break
		}
	}

	sortSegs(out)
	return out, nil
}

func sortSegs(segs []MediaSeg) {
	sort.Slice(segs, func(i, j int) bool {
		if segs[i].Start != segs[j].Start {
			return segs[i].Start < segs[j].Start
		}
		return segs[i].End < segs[j].End
	})
}

// mediaColumns mirrors the column-array response.
type mediaColumns struct {
	StartTime stringList `json:"start_time"`
	EndTime   stringList `json:"end_time"`
	Size      int64List  `json:"size"`
	EventType intList    `json:"event_type"`
	MediaType intList    `json:"media_type"`
	Channel   intList    `json:"channel"`
	ErrorCode int        `json:"error_code"`
}

// parseMediaList accepts either {"error_code":...,"media":{...}} or
// {"media":{"error_code":...,...}}.
func parseMediaList(raw []byte) ([]MediaSeg, int, error) {
	var r struct {
		ErrorCode int          `json:"error_code"`
		Result    int          `json:"result"`
		Media     mediaColumns `json:"media"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, 0, fmt.Errorf("nvr: media list: bad response: %w (raw %.200s)", err, raw)
	}
	code := r.ErrorCode
	if code == 0 {
		code = r.Media.ErrorCode
	}
	if code != 0 {
		return nil, code, nil
	}

	n := len(r.Media.StartTime)
	segs := make([]MediaSeg, 0, n)
	for i := 0; i < n; i++ {
		st, err := strconv.ParseInt(strings.TrimSpace(string(r.Media.StartTime[i])), 10, 64)
		if err != nil {
			continue
		}
		var et int64
		if i < len(r.Media.EndTime) {
			if v, err := strconv.ParseInt(strings.TrimSpace(string(r.Media.EndTime[i])), 10, 64); err == nil {
				et = v
			}
		}
		seg := MediaSeg{Start: st, End: et}
		if i < len(r.Media.Size) {
			seg.Size = r.Media.Size[i]
		}
		if i < len(r.Media.EventType) {
			seg.EventType = r.Media.EventType[i]
		}
		segs = append(segs, seg)
	}
	return segs, 0, nil
}

// ---------------------------------------------------------------------------
// Tolerant column decoders: firmware has been seen returning both numbers
// and decimal strings for times/sizes.
// ---------------------------------------------------------------------------

type stringList []string

func (l *stringList) UnmarshalJSON(b []byte) error {
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		*l = nil
		return nil
	}
	var vals []json.RawMessage
	if err := json.Unmarshal(b, &vals); err != nil {
		return err
	}
	out := make([]string, len(vals))
	for i, v := range vals {
		s, err := rawString(v)
		if err != nil {
			return err
		}
		out[i] = s
	}
	*l = out
	return nil
}

func rawString(b []byte) (string, error) {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		return s, nil
	}
	var n json.Number
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err == nil {
		if num, ok := v.(json.Number); ok {
			n = num
			return n.String(), nil
		}
	}
	return "", fmt.Errorf("expected string/number, got %.80s", b)
}

type int64List []int64

func (l *int64List) UnmarshalJSON(b []byte) error {
	vals, err := parseIntList(b)
	if err != nil {
		return err
	}
	out := make([]int64, len(vals))
	for i, v := range vals {
		out[i] = int64(v)
	}
	*l = out
	return nil
}

type intList []int

func (l *intList) UnmarshalJSON(b []byte) error {
	vals, err := parseIntList(b)
	if err != nil {
		return err
	}
	out := make([]int, len(vals))
	for i, v := range vals {
		out[i] = int(v)
	}
	*l = out
	return nil
}

func parseIntList(b []byte) ([]int64, error) {
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		return nil, nil
	}
	var vals []json.RawMessage
	if err := json.Unmarshal(b, &vals); err != nil {
		return nil, err
	}
	out := make([]int64, len(vals))
	for i, v := range vals {
		s, err := rawString(v)
		if err != nil {
			return nil, err
		}
		n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if err != nil {
			return nil, err
		}
		out[i] = n
	}
	return out, nil
}

// IsBusy reports the device's "too many sessions / busy" reply (-40401). It is
// transient: the request should be skipped or retried instead of treated as a
// configuration error.
func IsBusy(err error) bool {
	return err != nil && strings.Contains(err.Error(), "error_code=-40401")
}
