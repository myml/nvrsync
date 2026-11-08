package syncer

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/myml/nvrsync/internal/store"
)

// ---------------------------------------------------------------------------
// Retention
// ---------------------------------------------------------------------------

// retentionErrPrefix tags rows that retention retired, so
// shouldPromoteSkipped never queues them again.
const retentionErrPrefix = "retention:"

// cleanupRetention removes recordings older than Settings.RetentionD and
// retires their index rows.
//
// The two steps are deliberately conservative, because this is the only code
// that deletes user data:
//
//   - a file is removed only when the row is "done"/"local_only", has a path
//     that resolves inside StorageDir, and the segment start is older than the
//     cutoff;
//   - the row is deleted only when its file is confirmed gone. A row whose
//     file could not be removed stays as it was, so a later run retries
//     instead of orphaning bytes on disk;
//   - rows are only ever deleted outside the catalogue window and outside the
//     sync cutoff. Inside those windows the catalogue would see the footage on
//     the NVR again, re-add it as "pending" and download it a second time.
//
// Deleting rather than marking rows "skipped" keeps the index from growing
// forever: the table is scanned in full by Stats, Days and QueueSegments.
func (s *Syncer) cleanupRetention(settings store.Settings) {
	if settings.RetentionD <= 0 {
		return
	}
	now := time.Now()
	cutoff := now.Unix() - int64(settings.RetentionD)*86400
	// keepFrom is the oldest start retention must leave alone: anything the
	// catalogue can still re-list, or the sync cutoff can still queue.
	catalogDays := settings.CatalogDays
	if catalogDays <= 0 {
		catalogDays = defaultCatalogDays
	}
	keepFrom := now.AddDate(0, 0, -catalogDays).Unix()
	if syncLimit := syncCutoff(settings, now.Unix()); syncLimit < keepFrom {
		keepFrom = syncLimit
	}
	if cutoff < keepFrom {
		// Retention is shorter than the window the catalogue/sync still walk.
		// Deleting here would make the catalogue re-download what was just
		// deleted, so the window wins.
		keepFrom = cutoff
	}

	removed, keptRows, dirs := 0, 0, 0
	// local_only rows have a local file too; omitting them leaked every file
	// whose segment the NVR had already overwritten.
	for _, status := range []string{"done", "local_only"} {
		for _, seg := range s.listStatus(status) {
			if seg.Start >= cutoff || seg.Path == "" {
				continue
			}
			abs, ok := pathUnderStorage(settings.StorageDir, seg.Path)
			if !ok {
				s.st.Log("warn", "scan", fmt.Sprintf("保留清理跳过越界路径: %q", seg.Path))
				continue
			}
			if err := os.Remove(abs); err != nil && !errors.Is(err, os.ErrNotExist) {
				s.st.Log("warn", "scan", fmt.Sprintf("删除过期文件 %s 失败: %v", seg.Path, err))
				continue
			}
			if err := os.Remove(abs + ".json"); err != nil && !errors.Is(err, os.ErrNotExist) {
				s.st.Log("warn", "scan", fmt.Sprintf("删除 sidecar %s.json 失败: %v", seg.Path, err))
			}
			removed++

			if seg.Start >= keepFrom {
				// The catalogue could still re-list this footage; keep the row
				// so it is not treated as new (and downloaded again).
				keptRows++
				continue
			}
			if err := s.st.DeleteSegment(seg.Ch, seg.Start, seg.End); err != nil {
				s.st.Log("warn", "scan", fmt.Sprintf("删除过期段索引失败: %v", err))
			}
		}
	}
	if removed > 0 {
		dirs = s.cleanupEmptyDirs(settings.StorageDir)
	}
	s.st.Log("info", "scan", fmt.Sprintf("保留清理：删除 %d 个过期文件、清理 %d 个空目录（%d 天）",
		removed, dirs, settings.RetentionD))
	if keptRows > 0 {
		s.st.Log("info", "scan", fmt.Sprintf("保留清理：%d 个过期文件的索引仍在对账/同步窗口内，行予以保留以免重复下载", keptRows))
	}
}

// cleanupEmptyDirs removes date directories left empty after retention deleted
// a whole day of footage. It only descends into <storage>/rec, never removes
// the camN directories themselves, and stops at the first directory that still
// holds a file. Errors are ignored: this is housekeeping.
func (s *Syncer) cleanupEmptyDirs(storageDir string) int {
	if strings.TrimSpace(storageDir) == "" {
		return 0
	}
	root := filepath.Join(storageDir, "rec")
	cams, err := os.ReadDir(root)
	if err != nil {
		return 0
	}
	removed := 0
	for _, cam := range cams {
		if !cam.IsDir() {
			continue
		}
		camDir := filepath.Join(root, cam.Name())
		days, err := os.ReadDir(camDir)
		if err != nil {
			continue
		}
		for _, day := range days {
			if !day.IsDir() {
				continue
			}
			dayDir := filepath.Join(camDir, day.Name())
			entries, err := os.ReadDir(dayDir)
			if err != nil || len(entries) != 0 {
				continue
			}
			// Confirm the directory is still inside the storage root before
			// removing it: a symlinked day directory could point anywhere.
			if _, ok := pathUnderStorage(storageDir, filepath.ToSlash(filepath.Join("rec", cam.Name(), day.Name()))); !ok {
				continue
			}
			if err := os.Remove(dayDir); err == nil {
				removed++
			}
		}
	}
	return removed
}

// freeBytes reports the space available to an unprivileged writer.
func freeBytes(dir string) (int64, bool) {
	if strings.TrimSpace(dir) == "" {
		return 0, false
	}
	var fs syscall.Statfs_t
	if err := syscall.Statfs(dir, &fs); err != nil {
		return 0, false
	}
	return int64(fs.Bavail) * int64(fs.Bsize), true
}

// cleanupStaleParts removes interrupted downloads left behind by a crash or a
// container restart. Only files older than an hour are touched, so an active
// download from a concurrent process is never removed.
func (s *Syncer) cleanupStaleParts(storageDir string) {
	root := filepath.Join(strings.TrimSpace(storageDir), "rec")
	if strings.TrimSpace(storageDir) == "" {
		return
	}
	removed := 0
	_ = filepath.Walk(root, func(name string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(name, ".part") && !strings.HasSuffix(name, ".json.tmp") {
			return nil
		}
		if time.Since(info.ModTime()) < time.Hour {
			return nil
		}
		if err := os.Remove(name); err == nil {
			removed++
		}
		return nil
	})
	if removed > 0 {
		s.st.Log("info", "scan", fmt.Sprintf("清理了 %d 个残留的未完成下载文件", removed))
	}
}

// pathUnderStorage resolves a relative segment path and confirms it stays
// inside the storage root. Retention only ever deletes such paths.
func pathUnderStorage(storageDir, rel string) (string, bool) {
	root, err := filepath.Abs(storageDir)
	if err != nil {
		return "", false
	}
	p := filepath.Join(root, filepath.FromSlash(rel))
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", false
	}
	r, err := filepath.Rel(root, abs)
	if err != nil || r == "." || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) || filepath.IsAbs(r) {
		return "", false
	}
	return abs, true
}
