package service_test

import (
	"path/filepath"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/stretchr/testify/require"
)

func openTestFavSvc(t *testing.T) (*service.FavoritesService, func()) {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	for _, id := range []string{"a1", "a2", "a3"} {
		_, err := db.Exec(`INSERT INTO assets(id, file_path, status) VALUES(?, ?, 'indexed')`, id, "/x/"+id+".jpg")
		require.NoError(t, err)
	}
	return service.NewFavoritesService(db), func() { db.Close() }
}

func TestFavoriteIdempotent(t *testing.T) {
	svc, cleanup := openTestFavSvc(t)
	defer cleanup()

	t1, err := svc.Favorite("default", "a1")
	require.NoError(t, err)
	require.False(t, t1.IsZero())

	t2, err := svc.Favorite("default", "a1")
	require.NoError(t, err)
	require.Equal(t, t1.Unix(), t2.Unix(), "二次收藏应保留原时间")

	ids, err := svc.ListIDs("default")
	require.NoError(t, err)
	require.Len(t, ids, 1)
}

func TestFavoriteUnknownAsset(t *testing.T) {
	svc, cleanup := openTestFavSvc(t)
	defer cleanup()

	_, err := svc.Favorite("default", "nonexistent")
	require.ErrorIs(t, err, service.ErrNotFound)
}

func TestUnfavoriteIdempotent(t *testing.T) {
	svc, cleanup := openTestFavSvc(t)
	defer cleanup()

	_, err := svc.Favorite("default", "a1")
	require.NoError(t, err)

	require.NoError(t, svc.Unfavorite("default", "a1"))
	require.NoError(t, svc.Unfavorite("default", "a1"))
	require.NoError(t, svc.Unfavorite("default", "nonexistent"))
}

func TestListFavoritesOrderedByFavoritedAtDesc(t *testing.T) {
	svc, cleanup := openTestFavSvc(t)
	defer cleanup()

	_, err := svc.Favorite("default", "a1")
	require.NoError(t, err)
	_, err = svc.Favorite("default", "a2")
	require.NoError(t, err)
	_, err = svc.Favorite("default", "a3")
	require.NoError(t, err)

	assets, err := svc.List("default", service.ListFavoritesOpts{})
	require.NoError(t, err)
	require.Len(t, assets, 3)
	require.Equal(t, "a3", assets[0].ID)
	require.Equal(t, "a1", assets[2].ID)
	for _, a := range assets {
		require.NotNil(t, a.FavoritedAt, "asset %s missing FavoritedAt", a.ID)
	}
}

func TestListFavoritesPerUserIsolation(t *testing.T) {
	svc, cleanup := openTestFavSvc(t)
	defer cleanup()

	_, err := svc.Favorite("alice", "a1")
	require.NoError(t, err)
	_, err = svc.Favorite("bob", "a2")
	require.NoError(t, err)

	aliceList, _ := svc.ListIDs("alice")
	bobList, _ := svc.ListIDs("bob")
	require.Equal(t, []string{"a1"}, aliceList)
	require.Equal(t, []string{"a2"}, bobList)
}

func TestListFavoritesLimitOffset(t *testing.T) {
	svc, cleanup := openTestFavSvc(t)
	defer cleanup()

	_, _ = svc.Favorite("default", "a1")
	_, _ = svc.Favorite("default", "a2")
	_, _ = svc.Favorite("default", "a3")

	page, err := svc.List("default", service.ListFavoritesOpts{Limit: 2, Offset: 0})
	require.NoError(t, err)
	require.Len(t, page, 2)
	require.Equal(t, "a3", page[0].ID)

	page2, err := svc.List("default", service.ListFavoritesOpts{Limit: 2, Offset: 2})
	require.NoError(t, err)
	require.Len(t, page2, 1)
	require.Equal(t, "a1", page2[0].ID)
}
