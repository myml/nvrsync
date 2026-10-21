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

// Stats returns the dashboard aggregate, optionally limited to one channel.
func (s *Store) Stats(ch int) Stats {
	var st Stats

	filter := ""
	args := []any{}
	if ch > 0 {
		filter = " WHERE ch = ?"
		args = append(args, ch)
	}

	rows, err := s.db.Query("SELECT status, COUNT(*) FROM segments"+filter+" GROUP BY status", args...)
	if err == nil {
		for rows.Next() {
			var status string
			var n int
			if err := rows.Scan(&status, &n); err != nil {
				break
			}
			st.Total += n
			switch status {
			case "done":
				st.Done += n
			case "pending":
				st.Pending += n
			case "syncing":
				st.Syncing += n
			case "failed":
				st.Failed += n
			case "local_only":
				st.LocalOnly += n
			case "skipped":
				st.Skipped += n
			}
		}
		rows.Close()
	}

	where := " WHERE status IN ('done','local_only')"
	args2 := []any{}
	if ch > 0 {
		where += " AND ch = ?"
		args2 = append(args2, ch)
	}
	_ = s.db.QueryRow(`SELECT COALESCE(MIN(start_ts), 0), COALESCE(MAX(end_ts), 0)
		FROM segments`+where, args2...).Scan(&st.Earliest, &st.Latest)
	_ = s.db.QueryRow(`SELECT COALESCE(SUM(bytes), 0) FROM segments`+where, args2...).Scan(&st.Bytes)
	_ = s.db.QueryRow(`SELECT COUNT(DISTINCT date(start_ts, 'unixepoch', 'localtime'))
		FROM segments`+where, args2...).Scan(&st.Days)

	if dir := s.Settings().StorageDir; dir != "" {
		var fs syscall.Statfs_t
		if err := syscall.Statfs(dir, &fs); err == nil {
			st.StorageFree = int64(fs.Bavail) * int64(fs.Bsize)
			st.StorageTotal = int64(fs.Blocks) * int64(fs.Bsize)
		}
	}
	return st
}
