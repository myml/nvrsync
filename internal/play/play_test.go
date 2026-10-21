package play

import (
	"strings"
	"testing"
	"time"

	"github.com/myml/nvrsync/internal/store"
)

func TestDayPlaylistDropsContainedClips(t *testing.T) {
	base := time.Date(2026, 10, 21, 0, 0, 0, 0, time.Local).Unix()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	// A long continuous recording plus a motion clip that falls inside it, and
	// a separate clip outside the recording.
	segs := []store.Segment{
		{Ch: 1, Start: base + 1000, End: base + 4600, Status: "done", Path: "rec/cam1/2026-10-21/a.ts"},
		{Ch: 1, Start: base + 1200, End: base + 1210, Status: "done", Path: "rec/cam1/2026-10-21/b.ts"},
		{Ch: 1, Start: base + 5000, End: base + 5010, Status: "done", Path: "rec/cam1/2026-10-21/c.ts"},
	}
	for _, s := range segs {
		if err := st.UpsertSegment(s); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}

	p := New(st)
	out, err := p.DayPlaylist(1, "2026-10-21")
	if err != nil {
		t.Fatalf("playlist: %v", err)
	}
	body := string(out)
	if !strings.Contains(body, "/media/cam1/2026-10-21/a.ts") {
		t.Errorf("continuous segment missing:\n%s", body)
	}
	if strings.Contains(body, "/media/cam1/2026-10-21/b.ts") {
		t.Errorf("contained clip should be dropped:\n%s", body)
	}
	if !strings.Contains(body, "/media/cam1/2026-10-21/c.ts") {
		t.Errorf("later clip should be kept:\n%s", body)
	}
	if !strings.Contains(body, "#EXT-X-DISCONTINUITY") {
		t.Errorf("gap between 4600 and 5000 should be a discontinuity:\n%s", body)
	}
	if !strings.Contains(body, "#EXT-X-ENDLIST") {
		t.Errorf("missing ENDLIST:\n%s", body)
	}
}

func TestDropContainedKeepsIdenticalPairs(t *testing.T) {
	segs := []store.Segment{
		{Start: 0, End: 10},
		{Start: 0, End: 10},
	}
	if got := dropContained(segs); len(got) != 1 {
		t.Fatalf("duplicate intervals should collapse to one, got %d", len(got))
	}
}
