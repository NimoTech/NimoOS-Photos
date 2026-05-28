package service_test

import (
	"path/filepath"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/stretchr/testify/require"
)

func openTestAlbumSvc(t *testing.T) (*service.AlbumService, func()) {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	return service.NewAlbumService(db), func() { db.Close() }
}

func TestAlbumCreate(t *testing.T) {
	svc, cleanup := openTestAlbumSvc(t)
	defer cleanup()

	album, err := svc.Create("Vacation 2025")
	require.NoError(t, err)
	require.NotEmpty(t, album.ID)
	require.Equal(t, "Vacation 2025", album.Name)
	require.False(t, album.CreatedAt.IsZero())
}

func TestAlbumList(t *testing.T) {
	svc, cleanup := openTestAlbumSvc(t)
	defer cleanup()

	svc.Create("Album A")
	svc.Create("Album B")

	albums, err := svc.List()
	require.NoError(t, err)
	require.Len(t, albums, 2)
}

func TestAlbumDelete(t *testing.T) {
	svc, cleanup := openTestAlbumSvc(t)
	defer cleanup()

	album, _ := svc.Create("To Delete")
	require.NoError(t, svc.Delete(album.ID))

	albums, _ := svc.List()
	require.Empty(t, albums)
}

func TestAlbumDeleteNotFound(t *testing.T) {
	svc, cleanup := openTestAlbumSvc(t)
	defer cleanup()

	err := svc.Delete("nonexistent-id")
	require.ErrorIs(t, err, service.ErrNotFound)
}

func TestAlbumAddRemoveAsset(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	defer db.Close()

	// 先插入一个 asset
	_, err = db.Exec(`INSERT INTO assets(id, file_path, status) VALUES('a1','/DATA/Gallery/test.jpg','indexed')`)
	require.NoError(t, err)

	svc := service.NewAlbumService(db)
	album, _ := svc.Create("Test Album")

	require.NoError(t, svc.AddAsset(album.ID, "a1"))

	got, err := svc.Get(album.ID)
	require.NoError(t, err)
	require.Equal(t, album.ID, got.ID)

	require.NoError(t, svc.RemoveAsset(album.ID, "a1"))
}

func TestAlbumCreateDuplicateName(t *testing.T) {
	svc, cleanup := openTestAlbumSvc(t)
	defer cleanup()

	_, err := svc.Create("MyAlbum")
	require.NoError(t, err)

	_, err = svc.Create("MyAlbum")
	require.ErrorIs(t, err, service.ErrAlbumNameExists)
}

func TestAlbumCreateTrimsAndRejectsBlank(t *testing.T) {
	svc, cleanup := openTestAlbumSvc(t)
	defer cleanup()

	_, err := svc.Create("   ")
	require.ErrorIs(t, err, service.ErrInvalidInput)

	a, err := svc.Create("  Trimmed  ")
	require.NoError(t, err)
	require.Equal(t, "Trimmed", a.Name)
}

func TestBatchAddAssetsToAlbum(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	defer db.Close()

	for _, id := range []string{"a1", "a2", "a3"} {
		_, err := db.Exec(`INSERT INTO assets(id, file_path, status) VALUES(?, ?, 'indexed')`, id, "/x/"+id+".jpg")
		require.NoError(t, err)
	}

	svc := service.NewAlbumService(db)
	album, err := svc.Create("Batch")
	require.NoError(t, err)

	require.NoError(t, svc.BatchAddAssets(album.ID, []string{"a1", "a2", "a3"}))

	assets, err := svc.ListAssets(album.ID)
	require.NoError(t, err)
	require.Len(t, assets, 3)
}

func TestBatchAddAssetsIdempotent(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	defer db.Close()

	_, _ = db.Exec(`INSERT INTO assets(id, file_path, status) VALUES('a1', '/x.jpg', 'indexed')`)
	svc := service.NewAlbumService(db)
	album, _ := svc.Create("X")

	require.NoError(t, svc.BatchAddAssets(album.ID, []string{"a1", "a1", "a1"}))

	assets, _ := svc.ListAssets(album.ID)
	require.Len(t, assets, 1)
}

func TestBatchAddUnknownAlbum(t *testing.T) {
	svc, cleanup := openTestAlbumSvc(t)
	defer cleanup()
	err := svc.BatchAddAssets("nonexistent-id", []string{"a1"})
	require.ErrorIs(t, err, service.ErrNotFound)
}

func TestAlbumUpdateName(t *testing.T) {
	svc, cleanup := openTestAlbumSvc(t)
	defer cleanup()

	a, _ := svc.Create("Original")
	require.NoError(t, svc.UpdateName(a.ID, "Renamed"))

	got, _ := svc.Get(a.ID)
	require.Equal(t, "Renamed", got.Name)
}

func TestAlbumUpdateNameTrimsAndRejectsBlank(t *testing.T) {
	svc, cleanup := openTestAlbumSvc(t)
	defer cleanup()

	a, _ := svc.Create("X")

	require.ErrorIs(t, svc.UpdateName(a.ID, ""), service.ErrInvalidInput)
	require.ErrorIs(t, svc.UpdateName(a.ID, "   "), service.ErrInvalidInput)

	require.NoError(t, svc.UpdateName(a.ID, "  Trimmed  "))
	got, _ := svc.Get(a.ID)
	require.Equal(t, "Trimmed", got.Name)
}

func TestAlbumUpdateNameConflict(t *testing.T) {
	svc, cleanup := openTestAlbumSvc(t)
	defer cleanup()

	svc.Create("A")
	b, _ := svc.Create("B")

	require.ErrorIs(t, svc.UpdateName(b.ID, "A"), service.ErrAlbumNameExists)
}

func TestAlbumUpdateNameNotFound(t *testing.T) {
	svc, cleanup := openTestAlbumSvc(t)
	defer cleanup()

	require.ErrorIs(t, svc.UpdateName("nope", "X"), service.ErrNotFound)
}

func TestAlbumUpdateNameSameValue(t *testing.T) {
	// 自己改成自己的名字应成功（不算冲突）
	svc, cleanup := openTestAlbumSvc(t)
	defer cleanup()

	a, _ := svc.Create("Same")
	require.NoError(t, svc.UpdateName(a.ID, "Same"))
}
