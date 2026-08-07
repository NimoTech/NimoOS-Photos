package service

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

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
