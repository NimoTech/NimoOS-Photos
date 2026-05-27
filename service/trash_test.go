package service

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
)

// newTrashFixture 建一个临时库 + gallery/thumb 目录，插入一个磁盘上真实存在的资产。
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

func TestTrashThenRestore(t *testing.T) {
	ts, gallery, _ := newTrashFixture(t)
	orig := filepath.Join(gallery, "a.jpg")

	if err := ts.TrashAsset("a1"); err != nil {
		t.Fatalf("TrashAsset: %v", err)
	}
	if _, err := os.Stat(orig); !os.IsNotExist(err) {
		t.Fatalf("original file should be moved out of gallery root")
	}
	items, err := ts.ListTrash("u1")
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
	items, _ = ts.ListTrash("u1")
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
	items, _ := ts.ListTrash("u1")
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
	items, _ := ts.ListTrash("u1")
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
	items, _ := ts.ListTrash("u1")
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
	items, _ := ts.ListTrash("u1")
	if len(items) != 1 {
		t.Fatalf("recent item should be kept, got %d", len(items))
	}
}
