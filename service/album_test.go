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
