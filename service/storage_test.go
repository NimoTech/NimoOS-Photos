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

	// Capture the walk timestamp before Prune so the post-Prune assertion
	// below can require a genuinely new completed walk, not just the nil
	// zero-value that a not-yet-refreshed fsCache would also satisfy.
	s.mu.Lock()
	beforePrune := s.fsCachedAt
	s.mu.Unlock()

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
		if err != nil {
			return false
		}
		s.mu.Lock()
		refreshed := s.fsCache != nil && s.fsCachedAt.After(beforePrune)
		s.mu.Unlock()
		// Require an actual completed post-Prune walk (fsCachedAt advanced),
		// not merely the nil-fsCache zero value Invalidate() alone produces —
		// that would pass on the very first tick before refreshFS ever runs.
		return refreshed && st.PrunableBytes == 0
	}, 5*time.Second, 20*time.Millisecond, "PrunableBytes must reflect a real post-Prune walk, not a nil-cache zero value")
}

// TestStorageRefreshFSDiscardsStaleResultOnConcurrentInvalidate covers the overlap
// where a Prune() (Invalidate()) completes while a refreshFS() launched
// before it is still walking. The stale walk carries a pre-Prune filesystem
// snapshot and must not resurrect it into fsCache once it finishes — that
// would republish PrunableBytes/CacheBytes Prune just cleared for a full
// storageCacheTTL. refreshFS is unexported but this test file is in package
// service, so the overlap is orchestrated directly rather than raced with
// real goroutine timing (see the fsGen doc comment on the struct).
func TestStorageRefreshFSDiscardsStaleResultOnConcurrentInvalidate(t *testing.T) {
	db := makeTestDB(t)
	_, err := db.Exec(`INSERT INTO assets(id, file_path, file_size, mime_type, status)
		VALUES('a1','/x/a.jpg',1000,'image/jpeg','indexed')`)
	require.NoError(t, err)

	thumbDir := t.TempDir()
	writeFileOfSize(t, thumbDir, "a1/small.jpg", 100)
	writeFileOfSize(t, thumbDir, "ghost/small.jpg", 300)

	s := NewStorageService(db, filepath.Join(t.TempDir(), "photos.db"), thumbDir, t.TempDir(), t.TempDir())

	// Simulate a refreshFS() that was launched (as Stats()/WarmFS() would,
	// capturing fsGen under s.mu at launch time) while fsGen was still 0.
	launchGen := s.fsGen

	// A concurrent Prune() finishes and calls Invalidate() while that walk is
	// still in flight, bumping fsGen.
	s.Invalidate()

	// The in-flight walk (still holding the pre-Invalidate generation) now
	// finishes and tries to publish its (stale) snapshot.
	s.refreshFS(launchGen)

	s.mu.Lock()
	cache := s.fsCache
	s.mu.Unlock()
	require.Nil(t, cache, "a walk started before Invalidate() must discard its result, not resurrect stale bytes")

	// A subsequent real refresh (current generation) must still publish
	// normally — the discard above must not wedge future refreshes.
	require.Eventually(t, func() bool {
		st, err := s.Stats()
		return err == nil && st.PrunableBytes == 300
	}, 5*time.Second, 20*time.Millisecond, "a fresh refresh after the discard should publish normally")
}

// TestStatsReturnsBeforeSlowWalkCompletes discriminates Stats()'s async
// contract for real: it injects a walkDirFn that blocks on a channel, then
// asserts Stats() already returned (DB buckets populated, cache buckets at
// their pre-walk zero value) while the walk is still stuck — proving Stats()
// does not itself wait on the filesystem walk. Releasing the channel then
// lets the walk finish and publish, observed via Eventually.
func TestStatsReturnsBeforeSlowWalkCompletes(t *testing.T) {
	db := makeTestDB(t)
	_, err := db.Exec(`INSERT INTO assets(id, file_path, file_size, mime_type, status)
		VALUES('a1','/x/a.jpg',1000,'image/jpeg','indexed')`)
	require.NoError(t, err)

	thumbDir := t.TempDir()
	writeFileOfSize(t, thumbDir, "a1/small.jpg", 100)

	s := NewStorageService(db, filepath.Join(t.TempDir(), "photos.db"), thumbDir, t.TempDir(), t.TempDir())

	release := make(chan struct{})
	entered := make(chan struct{}, 8)
	// test-only seam: blocks every walk call until release is closed, so the
	// background refreshFS goroutine provably cannot have finished yet when
	// the assertions below run.
	s.walkDirFn = func(root string) int64 {
		entered <- struct{}{}
		<-release
		return dirSize(root)
	}

	st, err := s.Stats() // kicks the background walk, must not block on it
	require.NoError(t, err)
	require.Equal(t, int64(1000), st.PhotosBytes, "DB-derived buckets are always fresh")
	require.Equal(t, int64(0), st.CacheBytes, "cache bucket must still be at its pre-walk zero value")

	// Confirm the walk actually started (avoids a vacuously-true assertion
	// above if refreshFS's goroutine simply hadn't been scheduled yet).
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("walkDirFn was never called")
	}

	close(release)
	require.Eventually(t, func() bool {
		st, err := s.Stats()
		return err == nil && st.CacheBytes == 100
	}, 5*time.Second, 20*time.Millisecond, "cache bytes should land once the slow walk is released")
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
