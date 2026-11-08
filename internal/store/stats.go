package store

import "syscall"

// Stats is the aggregate shown on the sync page dashboard. All numbers are
// computed from the local index in one pass.
type Stats struct {
	Total     int   `json:"total"`
	Done      int   `json:"done"`
	Pending   int   `json:"pending"`
	Syncing   int   `json:"syncing"`
	Failed    int   `json:"failed"`
	LocalOnly int   `json:"local_only"`
	Skipped   int   `json:"skipped"`
	Earliest  int64 `json:"earliest"` // unix seconds, 0 when the index is empty
	Latest    int64 `json:"latest"`
	Bytes     int64 `json:"bytes"` // bytes on disk for done + local_only
	Days      int   `json:"days"`  // distinct local days with playable footage
	// Storage figures for the configured storage directory (0 when unknown).
	StorageFree  int64 `json:"storage_free"`
	StorageTotal int64 `json:"storage_total"`
}

// Stats returns the dashboard aggregate for the segments matching f, using the
// same channel/time semantics as Segments: From/To form a half-open interval
// and a segment counts when it overlaps it. Other filter fields are ignored so
// the dashboard can stay independent of the status filter.
// It intentionally uses one conditional-aggregation query: the previous
// implementation issued five separate full scans (one per status group plus
// MIN/MAX/SUM/COUNT(DISTINCT)).
func (s *Store) Stats(f SegFilter) Stats {
	var st Stats

	// An explicitly inverted range matches nothing; Segments reports zero for
	// it, so the dashboard must agree rather than silently aggregate it all.
	if f.From > 0 && f.To > 0 && f.From > f.To {
		return s.withStorage(st)
	}

	f.Status, f.Limit, f.Offset = "", 0, 0
	where, args := segmentWhere(f)

	_ = s.db.QueryRow(`SELECT
			COUNT(*),
			COALESCE(SUM(CASE WHEN status = 'done' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status = 'pending' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status = 'syncing' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status = 'failed' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status = 'local_only' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status = 'skipped' THEN 1 ELSE 0 END), 0),
			COALESCE(MIN(CASE WHEN status IN ('done','local_only') THEN start_ts END), 0),
			COALESCE(MAX(CASE WHEN status IN ('done','local_only') THEN end_ts END), 0),
			COALESCE(SUM(CASE WHEN status IN ('done','local_only') THEN bytes ELSE 0 END), 0),
			COUNT(DISTINCT CASE WHEN status IN ('done','local_only')
				THEN date(start_ts, 'unixepoch', 'localtime') END)
		FROM segments`+where,
		args...,
	).Scan(
		&st.Total, &st.Done, &st.Pending, &st.Syncing, &st.Failed,
		&st.LocalOnly, &st.Skipped, &st.Earliest, &st.Latest, &st.Bytes, &st.Days,
	)

	return s.withStorage(st)
}

// withStorage fills the free/total figures of the configured storage dir.
func (s *Store) withStorage(st Stats) Stats {
	if dir := s.Settings().StorageDir; dir != "" {
		var fs syscall.Statfs_t
		if err := syscall.Statfs(dir, &fs); err == nil {
			st.StorageFree = int64(fs.Bavail) * int64(fs.Bsize)
			st.StorageTotal = int64(fs.Blocks) * int64(fs.Bsize)
		}
	}
	return st
}
