package service

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	"github.com/stretchr/testify/require"
)

// newTrashFixture builds a temp DB + gallery/thumb directories and inserts an asset that really exists on disk.
func newTrashFixture(t *testing.T) (*TrashService, string, string) {
	t.Helper()
	dir := t.TempDir()
	db, err := sqlite.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	gallery := filepath.Join(dir, "Gallery")
	thumb := filepath.Join(dir, "thumbs")
	if err := os.MkdirAll(gallery, 0755); err != nil {
		t.Fatal(err)
	}
	orig := filepath.Join(gallery, "a.jpg")
	if err := os.WriteFile(orig, []byte("photo-bytes"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(thumb, "a1"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(thumb, "a1", "small.jpg"), []byte("t"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		`INSERT INTO assets(id, file_path, file_size, status) VALUES('a1', ?, 11, 'indexed')`, orig,
	); err != nil {
		t.Fatal(err)
	}
	return NewTrashService(db, gallery, thumb), gallery, thumb
}

// newTrashFixtureWithLive builds on newTrashFixture by adding a Live Photo
// video companion asset "a1v" (which really exists on disk), and points
// "a1"'s live_photo_video_id at it, for use by TrashAsset/RestoreAsset's Live
// Photo caption hand-off tests.
func newTrashFixtureWithLive(t *testing.T) (*TrashService, string, string) {
	t.Helper()
	ts, gallery, thumb := newTrashFixture(t)
	liveOrig := filepath.Join(gallery, "a.mov")
	if err := os.WriteFile(liveOrig, []byte("live-bytes"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.db.Exec(
		`INSERT INTO assets(id, file_path, file_size, status, is_live_photo_video) VALUES('a1v', ?, 5, 'indexed', 1)`,
		liveOrig,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.db.Exec(`UPDATE assets SET live_photo_video_id='a1v' WHERE id='a1'`); err != nil {
		t.Fatal(err)
	}
	return ts, gallery, thumb
}

func TestTrashThenRestore(t *testing.T) {
	ts, gallery, _ := newTrashFixture(t)
	orig := filepath.Join(gallery, "a.jpg")

	if err := ts.TrashAsset("a1"); err != nil {
		t.Fatalf("TrashAsset: %v", err)
	}
	if _, err := os.Stat(orig); !os.IsNotExist(err) {
		t.Fatalf("original file should be moved out of gallery root")
	}
	items, err := ts.ListTrash("u1", 500, 0)
	if err != nil {
		t.Fatalf("ListTrash: %v", err)
	}
	if len(items) != 1 || items[0].ID != "a1" {
		t.Fatalf("ListTrash got %+v", items)
	}
	if items[0].DeletedAt == nil {
		t.Fatalf("DeletedAt should be set")
	}
	if items[0].OriginalPath != orig {
		t.Fatalf("OriginalPath = %q, want %q", items[0].OriginalPath, orig)
	}

	if err := ts.RestoreAsset("a1"); err != nil {
		t.Fatalf("RestoreAsset: %v", err)
	}
	if _, err := os.Stat(orig); err != nil {
		t.Fatalf("file should be restored to original path: %v", err)
	}
	items, _ = ts.ListTrash("u1", 500, 0)
	if len(items) != 0 {
		t.Fatalf("trash should be empty after restore, got %d", len(items))
	}
}

func TestPurgeRemovesFileAndThumb(t *testing.T) {
	ts, _, thumb := newTrashFixture(t)
	if err := ts.TrashAsset("a1"); err != nil {
		t.Fatal(err)
	}
	if err := ts.PurgeAsset("a1"); err != nil {
		t.Fatalf("PurgeAsset: %v", err)
	}
	if _, err := os.Stat(filepath.Join(thumb, "a1")); !os.IsNotExist(err) {
		t.Fatalf("thumb dir should be removed")
	}
	items, _ := ts.ListTrash("u1", 500, 0)
	if len(items) != 0 {
		t.Fatalf("trash should be empty after purge")
	}
	var n int
	ts.db.QueryRow(`SELECT COUNT(*) FROM assets WHERE id='a1'`).Scan(&n)
	if n != 0 {
		t.Fatalf("asset row should be deleted, got %d", n)
	}
}

func TestEmptyTrash(t *testing.T) {
	ts, _, _ := newTrashFixture(t)
	if err := ts.TrashAsset("a1"); err != nil {
		t.Fatal(err)
	}
	if err := ts.EmptyTrash(); err != nil {
		t.Fatalf("EmptyTrash: %v", err)
	}
	items, _ := ts.ListTrash("u1", 500, 0)
	if len(items) != 0 {
		t.Fatalf("trash should be empty")
	}
}

func TestTrashNotFound(t *testing.T) {
	ts, _, _ := newTrashFixture(t)
	if err := ts.TrashAsset("nope"); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestPurgeExpired(t *testing.T) {
	ts, _, _ := newTrashFixture(t)
	if err := ts.TrashAsset("a1"); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.db.Exec(
		`UPDATE assets SET deleted_at = datetime('now','-40 days') WHERE id='a1'`); err != nil {
		t.Fatal(err)
	}
	if err := ts.PurgeExpired(30); err != nil {
		t.Fatalf("PurgeExpired: %v", err)
	}
	items, _ := ts.ListTrash("u1", 500, 0)
	if len(items) != 0 {
		t.Fatalf("expired item should be purged")
	}
}

func TestPurgeExpiredKeepsRecent(t *testing.T) {
	ts, _, _ := newTrashFixture(t)
	if err := ts.TrashAsset("a1"); err != nil {
		t.Fatal(err)
	}
	if err := ts.PurgeExpired(30); err != nil {
		t.Fatal(err)
	}
	items, _ := ts.ListTrash("u1", 500, 0)
	if len(items) != 1 {
		t.Fatalf("recent item should be kept, got %d", len(items))
	}
}

// TestTrashAsset_TriggersCaptionDelete: after a successful soft-delete move,
// the SetCaptionDelete-injected callback (Task 4 caption hand-off) should
// fire with the correct assetID.
func TestTrashAsset_TriggersCaptionDelete(t *testing.T) {
	ts, _, _ := newTrashFixture(t)

	var mu sync.Mutex
	var got []string
	ts.SetCaptionDelete(func(id string) {
		mu.Lock()
		got = append(got, id)
		mu.Unlock()
	})

	if err := ts.TrashAsset("a1"); err != nil {
		t.Fatalf("TrashAsset: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0] != "a1" {
		t.Fatalf("caption delete callback got %+v, want [a1]", got)
	}
}

// TestRestoreAsset_TriggersCaptionRestore: after a successful restore, the
// SetCaptionRestore-injected callback should fire with the correct assetID
// (so caption_synced can be reset and re-submitted).
func TestRestoreAsset_TriggersCaptionRestore(t *testing.T) {
	ts, _, _ := newTrashFixture(t)
	if err := ts.TrashAsset("a1"); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var got []string
	ts.SetCaptionRestore(func(id string) {
		mu.Lock()
		got = append(got, id)
		mu.Unlock()
	})

	if err := ts.RestoreAsset("a1"); err != nil {
		t.Fatalf("RestoreAsset: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0] != "a1" {
		t.Fatalf("caption restore callback got %+v, want [a1]", got)
	}
}

// TestTrashAsset_TriggersCaptionDeleteForLivePhoto: soft-deleting an asset
// with a Live Photo companion should fire the caption-delete callback once
// each for the primary asset and its companion (mirroring PurgeAsset's liveID
// handling, filled in on the TrashAsset side).
func TestTrashAsset_TriggersCaptionDeleteForLivePhoto(t *testing.T) {
	ts, _, _ := newTrashFixtureWithLive(t)

	var mu sync.Mutex
	var got []string
	ts.SetCaptionDelete(func(id string) {
		mu.Lock()
		got = append(got, id)
		mu.Unlock()
	})

	if err := ts.TrashAsset("a1"); err != nil {
		t.Fatalf("TrashAsset: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("caption delete callback got %+v, want 2 calls (a1 + a1v)", got)
	}
	require.ElementsMatch(t, []string{"a1", "a1v"}, got)
}

// TestRestoreAsset_TriggersCaptionRestoreForLivePhoto: restoring an asset
// with a Live Photo companion should fire the caption-restore callback once
// each for the primary asset and its companion.
func TestRestoreAsset_TriggersCaptionRestoreForLivePhoto(t *testing.T) {
	ts, _, _ := newTrashFixtureWithLive(t)
	if err := ts.TrashAsset("a1"); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var got []string
	ts.SetCaptionRestore(func(id string) {
		mu.Lock()
		got = append(got, id)
		mu.Unlock()
	})

	if err := ts.RestoreAsset("a1"); err != nil {
		t.Fatalf("RestoreAsset: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("caption restore callback got %+v, want 2 calls (a1 + a1v)", got)
	}
	require.ElementsMatch(t, []string{"a1", "a1v"}, got)
}

// TestPurgeAsset_TriggersCaptionDelete: after a successful physical delete
// (permanently deleting a single item), the caption-delete callback should
// fire — one of the two call sites next to dropClipVector (this test covers
// the primary-asset one).
func TestPurgeAsset_TriggersCaptionDelete(t *testing.T) {
	ts, _, _ := newTrashFixture(t)
	if err := ts.TrashAsset("a1"); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var got []string
	ts.SetCaptionDelete(func(id string) {
		mu.Lock()
		got = append(got, id)
		mu.Unlock()
	})

	if err := ts.PurgeAsset("a1"); err != nil {
		t.Fatalf("PurgeAsset: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0] != "a1" {
		t.Fatalf("caption delete callback got %+v, want [a1]", got)
	}
}

// TestEmptyTrash_TriggersCaptionDelete: emptying the trash (EmptyTrash →
// PurgeAsset) should fire the caption-delete callback for every item.
func TestEmptyTrash_TriggersCaptionDelete(t *testing.T) {
	ts, _, _ := newTrashFixture(t)
	if err := ts.TrashAsset("a1"); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var got []string
	ts.SetCaptionDelete(func(id string) {
		mu.Lock()
		got = append(got, id)
		mu.Unlock()
	})

	if err := ts.EmptyTrash(); err != nil {
		t.Fatalf("EmptyTrash: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0] != "a1" {
		t.Fatalf("caption delete callback got %+v, want [a1]", got)
	}
}

// TestMoveToTrashFollowsAssetVolume is the regression test for the
// 2026-08-18 delete-chain diagnosis: an asset living on a volume other than
// the configured galleryDir fallback must get its own .trash directory on
// THAT volume, not under galleryDir — otherwise the rename crosses devices
// and fails with EXDEV in production (galleryDir and the asset's real volume
// are frequently different filesystems, e.g. /DATA vs /media/RAID_0).
func TestMoveToTrashFollowsAssetVolume(t *testing.T) {
	dir := t.TempDir()
	db, err := sqlite.Open(filepath.Join(dir, "t.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	// galleryDir is the legacy fallback root — deliberately NOT where the
	// asset lives, so a pass only proves the fix picks the asset's own
	// volume instead of always defaulting here.
	gallery := filepath.Join(dir, "Gallery")
	thumb := filepath.Join(dir, "thumbs")
	require.NoError(t, os.MkdirAll(gallery, 0755))
	require.NoError(t, os.MkdirAll(thumb, 0755))

	volA := filepath.Join(dir, "volA") // decoy scan root the asset is NOT under
	volB := filepath.Join(dir, "volB") // the asset's actual volume
	require.NoError(t, os.MkdirAll(filepath.Join(volB, "sub"), 0755))
	orig := filepath.Join(volB, "sub", "a.jpg")
	require.NoError(t, os.WriteFile(orig, []byte("bytes"), 0644))

	_, err = db.Exec(`INSERT INTO assets(id, file_path, file_size, status) VALUES('a1', ?, 5, 'indexed')`, orig)
	require.NoError(t, err)

	ts := NewTrashService(db, gallery, thumb)
	// Fixed, test-only root set — this MUST NOT be the production
	// EnumerateScanRoots default, which would enumerate real mounts.
	ts.scanRoots = func() []string { return []string{volA, volB} }

	require.NoError(t, ts.TrashAsset("a1"))

	wantDir := filepath.Join(volB, ".trash", "a1")
	require.DirExists(t, wantDir)
	require.FileExists(t, filepath.Join(wantDir, "a.jpg"))
	require.NoDirExists(t, filepath.Join(gallery, ".trash"), "must not fall back to galleryDir when the asset's own volume is known")

	var filePath string
	require.NoError(t, db.QueryRow(`SELECT file_path FROM assets WHERE id='a1'`).Scan(&filePath))
	require.Equal(t, filepath.Join(wantDir, "a.jpg"), filePath)

	// Restore must move it back to volB, its original location — proving the
	// symmetric restore path also works per-volume (restoreFile leaves the
	// now-empty .trash/<id>/ dir behind; that's swept later by
	// CleanupOrphanTrashDirs, not RestoreAsset's job).
	require.NoError(t, ts.RestoreAsset("a1"))
	require.FileExists(t, orig)
}

// TestMoveToTrashEXDEVFallback verifies the defensive copy+delete fallback:
// when the injected rename returns EXDEV (simulating a cross-device rename
// that trashDirFor's volume matching failed to prevent), moveToTrash must
// still succeed by copying the file into place and removing the source,
// instead of failing the whole soft-delete outright.
func TestMoveToTrashEXDEVFallback(t *testing.T) {
	ts, gallery, _ := newTrashFixture(t)
	orig := filepath.Join(gallery, "a.jpg")

	calls := 0
	ts.osRename = func(oldpath, newpath string) error {
		calls++
		return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: syscall.EXDEV}
	}

	require.NoError(t, ts.TrashAsset("a1"))
	require.Equal(t, 1, calls, "moveToTrash should have attempted the rename exactly once before falling back")

	_, err := os.Stat(orig)
	require.True(t, os.IsNotExist(err), "source file should be removed after the copy fallback completes")

	wantDst := filepath.Join(gallery, ".trash", "a1", "a.jpg")
	data, err := os.ReadFile(wantDst)
	require.NoError(t, err)
	require.Equal(t, "photo-bytes", string(data), "copied file content must match the original")

	var deletedAt sql.NullString
	require.NoError(t, ts.db.QueryRow(`SELECT deleted_at FROM assets WHERE id='a1'`).Scan(&deletedAt))
	require.True(t, deletedAt.Valid, "asset should be marked deleted after a successful EXDEV fallback")
}

// TestMoveToTrashFailureCleansUpOrphanDir is the regression test for the
// second half of the diagnosis: when the move genuinely fails (not just
// EXDEV-and-recovered), the empty ".trash/<id>/" directory created by mkdir
// must be cleaned up instead of leaking on disk forever, and the DB row must
// roll back to point at the still-present original file.
func TestMoveToTrashFailureCleansUpOrphanDir(t *testing.T) {
	ts, gallery, _ := newTrashFixture(t)
	orig := filepath.Join(gallery, "a.jpg")

	wantErr := errors.New("simulated permanent rename failure")
	ts.osRename = func(oldpath, newpath string) error { return wantErr }

	err := ts.TrashAsset("a1")
	require.Error(t, err)

	require.NoDirExists(t, filepath.Join(gallery, ".trash", "a1"), "failed move must not leave an orphaned trash directory")

	var filePath string
	var deletedAt sql.NullString
	require.NoError(t, ts.db.QueryRow(`SELECT file_path, deleted_at FROM assets WHERE id='a1'`).Scan(&filePath, &deletedAt))
	require.Equal(t, orig, filePath, "DB row must roll back to the original path")
	require.False(t, deletedAt.Valid, "DB row must roll back deleted_at to NULL")
	require.FileExists(t, orig, "original file must still be in place")
}

// TestCleanupOrphanTrashDirs verifies the startup sweep only removes trash
// directories that are BOTH empty AND older than orphanTrashDirAge, leaving
// fresh empty dirs (a moveToTrash possibly still in flight) and non-empty
// dirs (real trashed files) untouched.
func TestCleanupOrphanTrashDirs(t *testing.T) {
	ts, gallery, _ := newTrashFixture(t)
	// Isolate from the real mount table entirely: CleanupOrphanTrashDirs
	// always includes galleryDir too, which is all this test needs.
	ts.scanRoots = func() []string { return nil }

	oldOrphan := filepath.Join(gallery, ".trash", "old-empty")
	freshOrphan := filepath.Join(gallery, ".trash", "fresh-empty")
	nonEmpty := filepath.Join(gallery, ".trash", "has-file")
	require.NoError(t, os.MkdirAll(oldOrphan, 0755))
	require.NoError(t, os.MkdirAll(freshOrphan, 0755))
	require.NoError(t, os.MkdirAll(nonEmpty, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(nonEmpty, "x.jpg"), []byte("x"), 0644))

	old := time.Now().Add(-2 * orphanTrashDirAge)
	require.NoError(t, os.Chtimes(oldOrphan, old, old))

	ts.CleanupOrphanTrashDirs()

	require.NoDirExists(t, oldOrphan, "old empty orphan dir should be removed")
	require.DirExists(t, freshOrphan, "fresh empty dir should be kept (might be a move still in flight)")
	require.DirExists(t, nonEmpty, "non-empty dir (real trashed file) should never be removed")
}
