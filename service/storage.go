package service

import (
	"database/sql"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// StorageStats is the payload returned by GET /v1/photos/storage.
type StorageStats struct {
	DiskTotalBytes int64 `json:"diskTotalBytes"`
	DiskFreeBytes  int64 `json:"diskFreeBytes"`
	PhotosBytes    int64 `json:"photosBytes"`
	VideosBytes    int64 `json:"videosBytes"`
	RawBytes       int64 `json:"rawBytes"`
	CacheBytes     int64 `json:"cacheBytes"`
	AIBytes        int64 `json:"aiBytes"`
	PrunableBytes  int64 `json:"prunableBytes"`
}

// rawExts are extensions counted as "RAW originals" in the storage breakdown.
// The indexer does not ingest RAW yet, so this bucket is 0 today; the set is
// kept so the API shape survives future RAW support.
var rawExts = map[string]bool{
	".dng": true, ".cr2": true, ".cr3": true, ".nef": true,
	".arw": true, ".orf": true, ".rw2": true, ".raf": true,
}

// storageCacheTTL bounds how often the thumbs-dir walk runs.
const storageCacheTTL = 60 * time.Second

// StorageService computes disk usage and library breakdown for the settings
// page. Stats() results are cached for storageCacheTTL.
type StorageService struct {
	db        *sql.DB
	dbPath    string // photos.db path (AI bucket = db + -wal + -shm)
	thumbDir  string
	faceDir   string
	statfsDir string // any path on the volume that holds the library

	mu       sync.Mutex
	cached   *StorageStats
	cachedAt time.Time
}

func NewStorageService(db *sql.DB, dbPath, thumbDir, faceDir, statfsDir string) *StorageService {
	return &StorageService{db: db, dbPath: dbPath, thumbDir: thumbDir, faceDir: faceDir, statfsDir: statfsDir}
}

// Stats returns storage statistics, recomputing at most once per storageCacheTTL.
func (s *StorageService) Stats() (*StorageStats, error) {
	s.mu.Lock()
	if s.cached != nil && time.Since(s.cachedAt) < storageCacheTTL {
		c := *s.cached
		s.mu.Unlock()
		return &c, nil
	}
	s.mu.Unlock()

	st, err := s.compute()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.cached, s.cachedAt = st, time.Now()
	s.mu.Unlock()
	c := *st
	return &c, nil
}

// Invalidate drops the cached stats (e.g. after a prune).
func (s *StorageService) Invalidate() {
	s.mu.Lock()
	s.cached = nil
	s.mu.Unlock()
}

func (s *StorageService) compute() (*StorageStats, error) {
	st := &StorageStats{}

	// 1. Volume capacity via statfs.
	var fsStat syscall.Statfs_t
	if err := syscall.Statfs(s.statfsDir, &fsStat); err == nil {
		st.DiskTotalBytes = int64(fsStat.Blocks) * int64(fsStat.Bsize)
		st.DiskFreeBytes = int64(fsStat.Bavail) * int64(fsStat.Bsize)
	}

	// 2. Library breakdown from the assets table. Trashed assets still occupy
	//    disk until purged, so deleted_at is intentionally NOT filtered.
	// The same scan also collects asset IDs for orphan detection below,
	// avoiding a second full-table pass.
	ids := map[string]bool{}
	rows, err := s.db.Query(`SELECT id, COALESCE(file_path,''), COALESCE(file_size,0), COALESCE(mime_type,'') FROM assets`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, path, mime string
		var size int64
		if err := rows.Scan(&id, &path, &size, &mime); err != nil {
			rows.Close()
			return nil, err
		}
		ids[id] = true
		switch {
		case rawExts[strings.ToLower(filepath.Ext(path))]:
			st.RawBytes += size
		case strings.HasPrefix(mime, "video/"):
			st.VideosBytes += size
		default:
			st.PhotosBytes += size
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// 3. Thumbnail cache + orphans (keep in sync with Prune()).
	entries, _ := os.ReadDir(s.thumbDir)
	for _, e := range entries {
		size := dirSize(filepath.Join(s.thumbDir, e.Name()))
		st.CacheBytes += size
		if e.IsDir() && !ids[e.Name()] {
			st.PrunableBytes += size
		}
	}
	// Face thumbnails count as cache but are keyed by person, not asset —
	// orphan detection for this bucket lives in Prune()'s face_detections
	// diff instead (删人/删照片留下的孤儿 jpg 由 Prune 统一回收).
	st.CacheBytes += dirSize(s.faceDir)

	// 4. AI bucket = SQLite database files.
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if fi, err := os.Stat(s.dbPath + suffix); err == nil {
			st.AIBytes += fi.Size()
		}
	}
	return st, nil
}

// PruneResult reports what Prune removed.
type PruneResult struct {
	FreedBytes   int64 `json:"freedBytes"`
	RemovedCount int   `json:"removedCount"`
}

// Prune removes thumbnail directories whose asset no longer exists, plus
// stale TUS staging files, then invalidates the stats cache. Pass an empty
// stagingDir to skip the staging pass (tests).
func (s *StorageService) Prune(stagingDir string, stagingMaxAge time.Duration) (*PruneResult, error) {
	res := &PruneResult{}

	ids := map[string]bool{}
	rows, err := s.db.Query(`SELECT id FROM assets`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids[id] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	entries, _ := os.ReadDir(s.thumbDir)
	for _, e := range entries {
		if !e.IsDir() || ids[e.Name()] {
			continue
		}
		p := filepath.Join(s.thumbDir, e.Name())
		size := dirSize(p)
		if err := os.RemoveAll(p); err == nil {
			res.FreedBytes += size
			res.RemovedCount++
		}
	}

	// face-thumbs 孤儿：按 face_detections 差集清理（删人/删照片不会清这里，
	// 见 Stats 中的注释；此处是唯一的回收路径）。
	faceIDs := map[string]bool{}
	frows, err := s.db.Query(`SELECT id FROM face_detections`)
	if err != nil {
		return nil, err
	}
	for frows.Next() {
		var id string
		if err := frows.Scan(&id); err != nil {
			frows.Close()
			return nil, err
		}
		faceIDs[id] = true
	}
	frows.Close()
	if err := frows.Err(); err != nil {
		return nil, err
	}
	fentries, _ := os.ReadDir(s.faceDir)
	for _, e := range fentries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".jpg") {
			continue
		}
		if faceIDs[strings.TrimSuffix(name, ".jpg")] {
			continue
		}
		var size int64
		if fi, ierr := e.Info(); ierr == nil {
			size = fi.Size()
		}
		if err := os.Remove(filepath.Join(s.faceDir, name)); err == nil {
			res.FreedBytes += size
			res.RemovedCount++
		}
	}

	// Stale TUS staging files (PruneStaging only reports count, so measure
	// the directory before/after for freed bytes).
	if stagingDir != "" {
		before := dirSize(stagingDir)
		if n, err := PruneStaging(stagingDir, stagingMaxAge); err == nil && n > 0 {
			res.RemovedCount += n
			// 清理期间可能有新上传写入 staging，差值取非负，宁可少报不报负。
			if freed := before - dirSize(stagingDir); freed > 0 {
				res.FreedBytes += freed
			}
		}
	}

	s.Invalidate()
	return res, nil
}

// dirSize returns the total size of all regular files under root (0 if missing).
func dirSize(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr — best-effort walk
		}
		if fi, e := d.Info(); e == nil {
			total += fi.Size()
		}
		return nil
	})
	return total
}
