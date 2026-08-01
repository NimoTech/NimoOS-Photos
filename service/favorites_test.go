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
	require.Equal(t, t1.Unix(), t2.Unix(), "re-favoriting should keep the original time")

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

// TestFavoritesListExcludesOffline verifies List() hides a favorited asset
// whose removable drive is currently unplugged (offline=1).
func TestFavoritesListExcludesOffline(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	defer db.Close()
	_, err = db.Exec(`INSERT INTO assets(id, file_path, status) VALUES
		('online','/DATA/1.jpg','indexed'),('offline','/media/X/2.jpg','indexed')`)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE assets SET offline=1 WHERE id='offline'`)
	require.NoError(t, err)

	svc := service.NewFavoritesService(db)
	_, err = svc.Favorite("default", "online")
	require.NoError(t, err)
	_, err = svc.Favorite("default", "offline")
	require.NoError(t, err)

	list, err := svc.List("default", service.ListFavoritesOpts{})
	require.NoError(t, err)
	require.Len(t, list, 1, "offline assets must be hidden from the favorites list")
	require.Equal(t, "online", list[0].ID)
}

// TestFavoritesTopExcludesOfflineAndDeleted verifies Top() applies the same
// visibility filter as List(): favorited assets that are soft-deleted or on a
// currently-unplugged removable drive (offline=1) must not appear.
func TestFavoritesTopExcludesOfflineAndDeleted(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	defer db.Close()
	_, err = db.Exec(`INSERT INTO assets(id, file_path, status) VALUES
		('online','/DATA/1.jpg','indexed'),
		('offline','/media/X/2.jpg','indexed'),
		('trashed','/DATA/3.jpg','indexed')`)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE assets SET offline=1 WHERE id='offline'`)
	require.NoError(t, err)

	svc := service.NewFavoritesService(db)
	for _, id := range []string{"online", "offline", "trashed"} {
		_, e := svc.Favorite("default", id)
		require.NoError(t, e)
	}
	// Soft delete happens after favoriting: Favorite() errors on a
	// nonexistent asset, but for an already-soft-deleted asset, the real-world
	// order is also "favorite first, then goes to trash".
	_, err = db.Exec(`UPDATE assets SET deleted_at='2026-01-01 00:00:00' WHERE id='trashed'`)
	require.NoError(t, err)

	top, err := svc.Top("default", 5)
	require.NoError(t, err)
	require.Len(t, top, 1, "offline and trashed assets must not appear in top favorites")
	require.Equal(t, "online", top[0].ID)
}

func TestTopOrdersByViewCountThenFavoritedAt(t *testing.T) {
	// This test needs ViewsService on the same db to seed view counts, and openTestFavSvc doesn't expose db, so build one here.
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	defer db.Close()
	for _, id := range []string{"a1", "a2", "a3"} {
		_, e := db.Exec(`INSERT INTO assets(id, file_path, status) VALUES(?, ?, 'indexed')`, id, "/x/"+id+".jpg")
		require.NoError(t, e)
	}
	svc := service.NewFavoritesService(db)
	views := service.NewViewsService(db)

	// All three favorited (a1 earliest, a3 latest).
	_, err = svc.Favorite("default", "a1")
	require.NoError(t, err)
	_, err = svc.Favorite("default", "a2")
	require.NoError(t, err)
	_, err = svc.Favorite("default", "a3")
	require.NoError(t, err)

	// a2 viewed 3 times, a1 viewed 1 time, a3 viewed 0 times.
	require.NoError(t, views.Record("default", "a2"))
	require.NoError(t, views.Record("default", "a2"))
	require.NoError(t, views.Record("default", "a2"))
	require.NoError(t, views.Record("default", "a1"))

	top, err := svc.Top("default", 5)
	require.NoError(t, err)
	require.Len(t, top, 3)
	require.Equal(t, "a2", top[0].ID, "the most-viewed ranks first")
	require.Equal(t, "a1", top[1].ID, "viewed once ranks second")
	require.Equal(t, "a3", top[2].ID, "0 views falls back to favorited time and ranks last")
}

func TestTopRespectsLimit(t *testing.T) {
	svc, cleanup := openTestFavSvc(t)
	defer cleanup()

	for _, id := range []string{"a1", "a2", "a3"} {
		_, err := svc.Favorite("default", id)
		require.NoError(t, err)
	}

	top, err := svc.Top("default", 2)
	require.NoError(t, err)
	require.Len(t, top, 2)
}

func TestTopEmptyWhenNoFavorites(t *testing.T) {
	svc, cleanup := openTestFavSvc(t)
	defer cleanup()

	top, err := svc.Top("default", 5)
	require.NoError(t, err)
	require.Len(t, top, 0)
}

func TestListFavoritesPopulatesNamedPersonFaces(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	defer db.Close()

	// Two assets; a1 has Biden + an unnamed person, a2 has nobody.
	_, err = db.Exec(`INSERT INTO assets(id, file_path, status) VALUES
		('a1','/x/a1.jpg','indexed'), ('a2','/x/a2.jpg','indexed')`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO persons(id, name) VALUES ('p1','Biden'), ('p2','')`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO face_detections(id, asset_id, bbox, embedding) VALUES
		('f1','a1','{}',x'00'), ('f2','a1','{}',x'00')`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO face_person(face_id, person_id) VALUES ('f1','p1'), ('f2','p2')`)
	require.NoError(t, err)

	svc := service.NewFavoritesService(db)
	_, err = svc.Favorite("default", "a1")
	require.NoError(t, err)
	_, err = svc.Favorite("default", "a2")
	require.NoError(t, err)

	assets, err := svc.List("default", service.ListFavoritesOpts{})
	require.NoError(t, err)
	require.Len(t, assets, 2)

	byID := map[string][]string{}
	for _, a := range assets {
		byID[a.ID] = a.Faces
	}
	require.Equal(t, []string{"Biden"}, byID["a1"], "only the named person should appear")
	require.Empty(t, byID["a2"], "asset with no faces has no names")
}
