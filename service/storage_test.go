package service

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// writeFileOfSize writes a file of the given size in bytes at dir/name.
func writeFileOfSize(t *testing.T, dir, name string, size int) string {
	t.Helper()
	p := filepath.Join(dir, name)
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
	require.NoError(t, os.WriteFile(p, make([]byte, size), 0o644))
	return p
}

func TestStorageStats(t *testing.T) {
	db := makeTestDB(t)
	// Two assets: one image + one video; plus one orphan thumbnail directory.
	_, err := db.Exec(`INSERT INTO assets(id, file_path, file_size, mime_type, status)
		VALUES('a1','/x/a.jpg',1000,'image/jpeg','indexed'),
		      ('a2','/x/b.mp4',5000,'video/mp4','indexed')`)
	require.NoError(t, err)

	thumbDir := t.TempDir()
	writeFileOfSize(t, thumbDir, "a1/small.jpg", 100)    // valid cache
	writeFileOfSize(t, thumbDir, "ghost/small.jpg", 300) // orphan cache
	faceDir := t.TempDir()
	writeFileOfSize(t, faceDir, "p1.jpg", 50)

	dbFile := writeFileOfSize(t, t.TempDir(), "photos.db", 700)

	s := NewStorageService(db, dbFile, thumbDir, faceDir, t.TempDir())
	st, err := s.Stats()
	require.NoError(t, err)

	// DB-derived buckets are correct immediately (single aggregate query).
	require.Equal(t, int64(1000), st.PhotosBytes)
	require.Equal(t, int64(5000), st.VideosBytes)
	require.Equal(t, int64(0), st.RawBytes)
	require.Equal(t, int64(700), st.AIBytes)
	require.Greater(t, st.DiskTotalBytes, int64(0))
	require.Greater(t, st.DiskFreeBytes, int64(0))

	// Filesystem-derived buckets land asynchronously: the first call kicks the
	// walk, a later call observes the completed result.
	require.Eventually(t, func() bool {
		st, err := s.Stats()
		return err == nil && st.CacheBytes == 450 && st.PrunableBytes == 300
	}, 5*time.Second, 20*time.Millisecond, "cache/prunable bytes should land after the background walk")
}

func TestStorageStatsDBBucketsAlwaysFresh(t *testing.T) {
	db := makeTestDB(t)
	s := NewStorageService(db, filepath.Join(t.TempDir(), "photos.db"), t.TempDir(), t.TempDir(), t.TempDir())
	st1, err := s.Stats()
	require.NoError(t, err)
	require.Equal(t, int64(0), st1.PhotosBytes)

	// DB-derived buckets are recomputed on every call now — no 60s cache to
	// wait out, unlike the filesystem-derived buckets below.
	_, err = db.Exec(`INSERT INTO assets(id, file_path, file_size, mime_type, status)
		VALUES('a9','/x/c.jpg',1234,'image/jpeg','indexed')`)
	require.NoError(t, err)
	st2, err := s.Stats()
	require.NoError(t, err)
	require.Equal(t, int64(1234), st2.PhotosBytes)
}

func TestStoragePruneRemovesOnlyOrphans(t *testing.T) {
	db := makeTestDB(t)
	_, err := db.Exec(`INSERT INTO assets(id, file_path, file_size, mime_type, status)
		VALUES('a1','/x/a.jpg',1000,'image/jpeg','indexed')`)
	require.NoError(t, err)

	thumbDir := t.TempDir()
	valid := writeFileOfSize(t, thumbDir, "a1/small.jpg", 100)
	writeFileOfSize(t, thumbDir, "ghost/small.jpg", 300)

	s := NewStorageService(db, filepath.Join(t.TempDir(), "photos.db"), thumbDir, t.TempDir(), t.TempDir())
	_, err = s.Stats() // kicks the background walk
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		st, err := s.Stats()
		return err == nil && st.PrunableBytes == 300 // populate the fs cache, to verify Prune invalidates it
	}, 5*time.Second, 20*time.Millisecond)

	res, err := s.Prune("", 0) // scenario with no staging directory
	require.NoError(t, err)
	require.Equal(t, int64(300), res.FreedBytes)
	require.Equal(t, 1, res.RemovedCount)

	_, statErr := os.Stat(valid)
	require.NoError(t, statErr) // valid thumbnail retained
	_, statErr = os.Stat(filepath.Join(thumbDir, "ghost"))
	require.True(t, os.IsNotExist(statErr)) // orphan directory removed

	require.Eventually(t, func() bool {
		st, err := s.Stats()
		return err == nil && st.PrunableBytes == 0 // fs cache invalidated and recomputed
	}, 5*time.Second, 20*time.Millisecond)
}

func TestPruneRemovesOrphanFaceThumbs(t *testing.T) {
	db := makeTestDB(t)
	_, err := db.Exec(`INSERT INTO assets(id, file_path, file_size, mime_type, status)
		VALUES('a1','/x/a.jpg',1000,'image/jpeg','indexed')`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO face_detections(id, asset_id, bbox, embedding)
		VALUES('keep','a1','{}',x'00')`)
	require.NoError(t, err)

	faceDir := t.TempDir()
	keep := writeFileOfSize(t, faceDir, "keep.jpg", 50)
	orphan := writeFileOfSize(t, faceDir, "orphan.jpg", 80)

	s := NewStorageService(db, filepath.Join(t.TempDir(), "photos.db"), t.TempDir(), faceDir, t.TempDir())
	res, err := s.Prune("", 0)
	require.NoError(t, err)

	_, statErr := os.Stat(keep)
	require.NoError(t, statErr) // has a matching face_detections row, retained
	_, statErr = os.Stat(orphan)
	require.True(t, os.IsNotExist(statErr)) // orphan face thumbnail removed

	require.Equal(t, int64(80), res.FreedBytes)
	require.Equal(t, 1, res.RemovedCount)
}
