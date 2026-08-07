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

// storageCacheTTL bounds how often the thumbs-dir walk runs.
const storageCacheTTL = 60 * time.Second

// StorageService computes disk usage and library breakdown for the settings
// page. DB-derived buckets are recomputed on every Stats() call (a single
// aggregate query, cheap even at gallery scale); filesystem-derived buckets
// are refreshed in the background at most once per storageCacheTTL.
type StorageService struct {
	db        *sql.DB
	dbPath    string // photos.db path (AI bucket = db + -wal + -shm)
	thumbDir  string
	faceDir   string
	statfsDir string // any path on the volume that holds the library

	mu           sync.Mutex
	fsCache      *fsStats // cache/prunable bytes from the last completed walk
	fsCachedAt   time.Time
	fsRefreshing bool
	// fsGen is bumped by Invalidate(). A refreshFS() run captures the
	// generation in effect when it was launched and compares it again right
	// before publishing: if Invalidate() (e.g. from Prune()) ran while the
	// walk was in flight, the snapshot it collected reflects a filesystem
	// state that predates the invalidation and must be discarded instead of
	// resurrecting stale PrunableBytes/CacheBytes for a full storageCacheTTL.
	fsGen int
}

// fsStats is the filesystem-derived half of StorageStats: it requires
// walking the whole thumbnail tree (one directory per asset), which at
// gallery scale is hundreds of thousands of stat calls — never do it on
// the request path.
type fsStats struct {
	CacheBytes    int64
	PrunableBytes int64
}

func NewStorageService(db *sql.DB, dbPath, thumbDir, faceDir, statfsDir string) *StorageService {
	return &StorageService{db: db, dbPath: dbPath, thumbDir: thumbDir, faceDir: faceDir, statfsDir: statfsDir}
}

// Stats returns storage statistics. DB-derived buckets are computed fresh on
// every call (a single aggregate query); filesystem-derived buckets come from
// a background walk refreshed at most every storageCacheTTL — stale values
// are served immediately rather than blocking the request.
func (s *StorageService) Stats() (*StorageStats, error) {
	st, err := s.computeDB()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.fsCache != nil {
		st.CacheBytes = s.fsCache.CacheBytes
		st.PrunableBytes = s.fsCache.PrunableBytes
	}
	if !s.fsRefreshing && (s.fsCache == nil || time.Since(s.fsCachedAt) >= storageCacheTTL) {
		s.fsRefreshing = true
		gen := s.fsGen
		go s.refreshFS(gen)
	}
	s.mu.Unlock()
	return st, nil
}

// Invalidate clears the cached filesystem-derived stats so the next Stats()
// call kicks a fresh background walk (e.g. after a prune). DB-derived
// buckets are always fresh already, so there is nothing else to drop.
// Bumping fsGen also tells any refreshFS() already in flight that its
// eventual result is stale and must be discarded (see fsGen's doc comment).
func (s *StorageService) Invalidate() {
	s.mu.Lock()
	s.fsCache = nil
	s.fsGen++
	s.mu.Unlock()
}

// WarmFS kicks one background walk so the settings page has cache numbers
// soon after boot. Safe to call more than once.
func (s *StorageService) WarmFS() {
	s.mu.Lock()
	if !s.fsRefreshing {
		s.fsRefreshing = true
		gen := s.fsGen
		go s.refreshFS(gen)
	}
	s.mu.Unlock()
}

func (s *StorageService) computeDB() (*StorageStats, error) {
	st := &StorageStats{}
	var fsStat syscall.Statfs_t
	if err := syscall.Statfs(s.statfsDir, &fsStat); err == nil {
		st.DiskTotalBytes = int64(fsStat.Blocks) * int64(fsStat.Bsize)
		st.DiskFreeBytes = int64(fsStat.Bavail) * int64(fsStat.Bsize)
	}
	// Trashed assets still occupy disk until purged: deleted_at intentionally
	// not filtered (same contract as the old per-row scan).
	err := s.db.QueryRow(`
SELECT
  COALESCE(SUM(CASE WHEN `+rawExtCase+` THEN file_size ELSE 0 END), 0),
  COALESCE(SUM(CASE WHEN NOT `+rawExtCase+` AND COALESCE(mime_type,'') LIKE 'video/%' THEN file_size ELSE 0 END), 0),
  COALESCE(SUM(CASE WHEN NOT `+rawExtCase+` AND COALESCE(mime_type,'') NOT LIKE 'video/%' THEN file_size ELSE 0 END), 0)
FROM assets`).Scan(&st.RawBytes, &st.VideosBytes, &st.PhotosBytes)
	if err != nil {
		return nil, err
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if fi, err := os.Stat(s.dbPath + suffix); err == nil {
			st.AIBytes += fi.Size()
		}
	}
	return st, nil
}

// rawExtCase mirrors the rawExts map in SQL so the aggregate stays in one
// round-trip. Keep the two lists in sync.
const rawExtCase = `(lower(COALESCE(file_path,'')) LIKE '%.dng' OR lower(COALESCE(file_path,'')) LIKE '%.cr2'
	OR lower(COALESCE(file_path,'')) LIKE '%.cr3' OR lower(COALESCE(file_path,'')) LIKE '%.nef'
	OR lower(COALESCE(file_path,'')) LIKE '%.arw' OR lower(COALESCE(file_path,'')) LIKE '%.orf'
	OR lower(COALESCE(file_path,'')) LIKE '%.rw2' OR lower(COALESCE(file_path,'')) LIKE '%.raf')`

// refreshFS runs the expensive thumbnail-tree walk off the request path and
// publishes the result. Orphan detection reuses the old logic (dir name not
// present in assets.id). gen is the fsGen captured by the caller at launch
// time (under s.mu); if Invalidate() bumps fsGen before this walk finishes
// (e.g. a concurrent Prune() completed mid-walk), the snapshot collected
// here is stale and is discarded instead of being published — the next
// Stats() call already sees fsCache == nil (from Invalidate) and kicks a
// fresh walk on its own, so no extra self-retrigger is needed here.
func (s *StorageService) refreshFS(gen int) {
	defer func() {
		s.mu.Lock()
		s.fsRefreshing = false
		s.mu.Unlock()
	}()
	ids := map[string]bool{}
	rows, err := s.db.Query(`SELECT id FROM assets`)
	if err != nil {
		return
	}
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids[id] = true
		}
	}
	rows.Close()

	out := &fsStats{}
	entries, _ := os.ReadDir(s.thumbDir)
	for _, e := range entries {
		size := dirSize(filepath.Join(s.thumbDir, e.Name()))
		out.CacheBytes += size
		if e.IsDir() && !ids[e.Name()] {
			out.PrunableBytes += size
		}
	}
	out.CacheBytes += dirSize(s.faceDir)

	s.mu.Lock()
	if s.fsGen != gen {
		// Stale: an Invalidate() (Prune()) ran while we were walking.
		s.mu.Unlock()
		return
	}
	s.fsCache, s.fsCachedAt = out, time.Now()
	s.mu.Unlock()
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

	// face-thumbs orphans: cleaned up by diffing against face_detections
	// (deleting a person/photo doesn't clean this up; see the comment in
	// refreshFS — this is the only reclamation path).
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
			// New uploads may write to staging during cleanup; clamp the diff to
			// non-negative — better to under-report than report negative.
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
