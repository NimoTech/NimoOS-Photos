package service

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	"github.com/stretchr/testify/require"
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

// newTrashFixtureWithLive 在 newTrashFixture 基础上追加一个 Live Photo 视频
// 伴随资产 "a1v"（磁盘上真实存在），并把 "a1" 的 live_photo_video_id 指向它，
// 供 TrashAsset/RestoreAsset 的 Live Photo caption 联动测试用。
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

// TestTrashAsset_TriggersCaptionDelete：软删移动成功后应调用 SetCaptionDelete
// 注入的回调（Task 4 caption 联动），携带正确的 assetID。
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

// TestRestoreAsset_TriggersCaptionRestore：恢复成功后应调用 SetCaptionRestore
// 注入的回调，携带正确的 assetID（供 caption_synced 复位重投）。
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

// TestTrashAsset_TriggersCaptionDeleteForLivePhoto：带 Live Photo 伴随资产的
// 软删应对主资产和伴随资产各触发一次 caption 删除回调（照 PurgeAsset 的
// liveID 处理样式补齐 TrashAsset 一侧）。
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

// TestRestoreAsset_TriggersCaptionRestoreForLivePhoto：带 Live Photo 伴随资产
// 的恢复应对主资产和伴随资产各触发一次 caption 复位回调。
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

// TestPurgeAsset_TriggersCaptionDelete：物理删除（永久删除单项）成功后应调用
// caption 删除回调，紧邻 dropClipVector 的两处调用点之一（本项测主资产项）。
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

// TestEmptyTrash_TriggersCaptionDelete：清空回收站（EmptyTrash → PurgeAsset）
// 每一项都应触发 caption 删除回调。
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
