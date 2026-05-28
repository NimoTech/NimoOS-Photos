package service_test

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
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

	// Insert one asset first
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
	// Renaming to the same name must succeed (not a conflict)
	svc, cleanup := openTestAlbumSvc(t)
	defer cleanup()

	a, _ := svc.Create("Same")
	require.NoError(t, svc.UpdateName(a.ID, "Same"))
}

func TestAlbumUpdateCoverSuccess(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	defer db.Close()

	_, err = db.Exec(`INSERT INTO assets(id, file_path, status) VALUES('a1','/g/1.jpg','indexed')`)
	require.NoError(t, err)
	svc := service.NewAlbumService(db)
	a, _ := svc.Create("With Cover")
	require.NoError(t, svc.AddAsset(a.ID, "a1"))

	require.NoError(t, svc.UpdateCover(a.ID, "a1"))

	got, _ := svc.Get(a.ID)
	require.Equal(t, "a1", got.CoverAssetID)
}

func TestAlbumUpdateCoverNotInAlbum(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	defer db.Close()

	_, err = db.Exec(`INSERT INTO assets(id, file_path, status) VALUES('a1','/g/1.jpg','indexed'),('a2','/g/2.jpg','indexed')`)
	require.NoError(t, err)
	svc := service.NewAlbumService(db)
	a, _ := svc.Create("Album")
	require.NoError(t, svc.AddAsset(a.ID, "a1"))

	require.ErrorIs(t, svc.UpdateCover(a.ID, "a2"), service.ErrCoverNotInAlbum)
}

func TestAlbumUpdateCoverAlbumNotFound(t *testing.T) {
	svc, cleanup := openTestAlbumSvc(t)
	defer cleanup()

	require.ErrorIs(t, svc.UpdateCover("nope", "a1"), service.ErrNotFound)
}

func TestAlbumUpdateCoverAssetNotExist(t *testing.T) {
	// Non-existent asset also returns ErrCoverNotInAlbum (semantics: this album doesn't contain it)
	svc, cleanup := openTestAlbumSvc(t)
	defer cleanup()

	a, _ := svc.Create("X")
	require.ErrorIs(t, svc.UpdateCover(a.ID, "ghost-id"), service.ErrCoverNotInAlbum)
}

func TestAlbumAddAssetAssignsIncrementalPosition(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	defer db.Close()

	_, err = db.Exec(`INSERT INTO assets(id, file_path, status) VALUES
		('a1','/g/1.jpg','indexed'),
		('a2','/g/2.jpg','indexed'),
		('a3','/g/3.jpg','indexed')`)
	require.NoError(t, err)
	svc := service.NewAlbumService(db)
	a, _ := svc.Create("X")

	require.NoError(t, svc.AddAsset(a.ID, "a1"))
	require.NoError(t, svc.AddAsset(a.ID, "a2"))
	require.NoError(t, svc.AddAsset(a.ID, "a3"))

	positions := queryPositions(t, db, a.ID)
	require.Equal(t, map[string]int{"a1": 0, "a2": 1, "a3": 2}, positions)
}

func TestAlbumAddAssetAfterRemoveContinuesMaxPlusOne(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	defer db.Close()

	_, err = db.Exec(`INSERT INTO assets(id, file_path, status) VALUES
		('a1','/g/1.jpg','indexed'),('a2','/g/2.jpg','indexed'),('a3','/g/3.jpg','indexed')`)
	require.NoError(t, err)
	svc := service.NewAlbumService(db)
	a, _ := svc.Create("X")

	svc.AddAsset(a.ID, "a1") // pos=0
	svc.AddAsset(a.ID, "a2") // pos=1
	require.NoError(t, svc.RemoveAsset(a.ID, "a1"))
	require.NoError(t, svc.AddAsset(a.ID, "a3")) // pos=2 (MAX+1)

	positions := queryPositions(t, db, a.ID)
	require.Equal(t, map[string]int{"a2": 1, "a3": 2}, positions)
}

func TestAlbumBatchAddAssetsAssignsContiguousPosition(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	defer db.Close()

	_, err = db.Exec(`INSERT INTO assets(id, file_path, status) VALUES
		('a1','/g/1.jpg','indexed'),('a2','/g/2.jpg','indexed'),
		('a3','/g/3.jpg','indexed'),('a4','/g/4.jpg','indexed'),
		('a5','/g/5.jpg','indexed')`)
	require.NoError(t, err)
	svc := service.NewAlbumService(db)
	a, _ := svc.Create("X")

	require.NoError(t, svc.BatchAddAssets(a.ID, []string{"a1", "a2", "a3"}))
	require.NoError(t, svc.BatchAddAssets(a.ID, []string{"a4", "a5"}))

	positions := queryPositions(t, db, a.ID)
	require.Equal(t, map[string]int{"a1": 0, "a2": 1, "a3": 2, "a4": 3, "a5": 4}, positions)
}

func TestAlbumListAssetsOrderedByPosition(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	defer db.Close()

	// Intentionally insert assets in taken_at desc order while position order is a3,a1,a2
	_, err = db.Exec(`INSERT INTO assets(id, file_path, taken_at, status) VALUES
		('a1','/g/1.jpg','2026-01-01','indexed'),
		('a2','/g/2.jpg','2026-02-01','indexed'),
		('a3','/g/3.jpg','2026-03-01','indexed')`)
	require.NoError(t, err)
	svc := service.NewAlbumService(db)
	a, _ := svc.Create("X")

	// Insert album_assets directly to control position for the test
	_, err = db.Exec(`INSERT INTO album_assets(album_id, asset_id, position) VALUES
		(?, 'a3', 0), (?, 'a1', 1), (?, 'a2', 2)`, a.ID, a.ID, a.ID)
	require.NoError(t, err)

	assets, err := svc.ListAssets(a.ID)
	require.NoError(t, err)
	require.Len(t, assets, 3)
	require.Equal(t, "a3", assets[0].ID)
	require.Equal(t, "a1", assets[1].ID)
	require.Equal(t, "a2", assets[2].ID)
}

func TestAlbumReorderAssetsSuccess(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	defer db.Close()

	_, err = db.Exec(`INSERT INTO assets(id, file_path, status) VALUES
		('a1','/g/1.jpg','indexed'),('a2','/g/2.jpg','indexed'),('a3','/g/3.jpg','indexed')`)
	require.NoError(t, err)
	svc := service.NewAlbumService(db)
	a, _ := svc.Create("X")
	svc.AddAsset(a.ID, "a1")
	svc.AddAsset(a.ID, "a2")
	svc.AddAsset(a.ID, "a3")

	require.NoError(t, svc.ReorderAssets(a.ID, []string{"a3", "a1", "a2"}))

	require.Equal(t, map[string]int{"a3": 0, "a1": 1, "a2": 2}, queryPositions(t, db, a.ID))
}

func TestAlbumReorderRejectsMismatchedSets(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	defer db.Close()

	_, err = db.Exec(`INSERT INTO assets(id, file_path, status) VALUES
		('a1','/g/1.jpg','indexed'),('a2','/g/2.jpg','indexed'),('a3','/g/3.jpg','indexed')`)
	require.NoError(t, err)
	svc := service.NewAlbumService(db)
	a, _ := svc.Create("X")
	svc.AddAsset(a.ID, "a1")
	svc.AddAsset(a.ID, "a2")

	// more than current
	require.ErrorIs(t, svc.ReorderAssets(a.ID, []string{"a1", "a2", "a3"}), service.ErrInvalidInput)
	// fewer than current
	require.ErrorIs(t, svc.ReorderAssets(a.ID, []string{"a1"}), service.ErrInvalidInput)
	// same length but different set
	require.ErrorIs(t, svc.ReorderAssets(a.ID, []string{"a1", "a3"}), service.ErrInvalidInput)
}

func TestAlbumReorderRejectsEmpty(t *testing.T) {
	svc, cleanup := openTestAlbumSvc(t)
	defer cleanup()
	a, _ := svc.Create("X")

	require.ErrorIs(t, svc.ReorderAssets(a.ID, []string{}), service.ErrInvalidInput)
}

func TestAlbumReorderAlbumNotFound(t *testing.T) {
	svc, cleanup := openTestAlbumSvc(t)
	defer cleanup()

	require.ErrorIs(t, svc.ReorderAssets("nope", []string{"a1"}), service.ErrNotFound)
}

func TestAlbumReorderRejectsDuplicateIDs(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	defer db.Close()

	_, err = db.Exec(`INSERT INTO assets(id, file_path, status) VALUES
		('a1','/g/1.jpg','indexed'),('a2','/g/2.jpg','indexed')`)
	require.NoError(t, err)
	svc := service.NewAlbumService(db)
	a, _ := svc.Create("X")
	svc.AddAsset(a.ID, "a1")
	svc.AddAsset(a.ID, "a2")

	// duplicate IDs in the array
	require.ErrorIs(t, svc.ReorderAssets(a.ID, []string{"a1", "a1"}), service.ErrInvalidInput)
}

// queryPositions is a test helper that reads the (asset_id -> position) map for an album.
func queryPositions(t *testing.T, db *sql.DB, albumID string) map[string]int {
	t.Helper()
	rows, err := db.Query(`SELECT asset_id, position FROM album_assets WHERE album_id=?`, albumID)
	require.NoError(t, err)
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var aid string
		var pos int
		require.NoError(t, rows.Scan(&aid, &pos))
		out[aid] = pos
	}
	return out
}

func TestAlbumBatchAddAssetsConcurrentNoPositionConflict(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	defer db.Close()
	// Limit to one open connection so concurrent transactions are serialised
	// by the connection pool rather than hitting SQLite's writer-lock limit.
	// This is realistic: production also uses a single writer connection.
	db.SetMaxOpenConns(1)

	// Pre-insert 20 assets to give all goroutines distinct asset_ids
	const total = 20
	for i := 0; i < total; i++ {
		_, err := db.Exec(
			`INSERT INTO assets(id, file_path, status) VALUES(?, ?, 'indexed')`,
			fmt.Sprintf("a%d", i), fmt.Sprintf("/g/%d.jpg", i),
		)
		require.NoError(t, err)
	}

	svc := service.NewAlbumService(db)
	a, _ := svc.Create("X")

	// Fire 4 goroutines each adding 5 distinct assets concurrently
	const goroutines = 4
	const perGoroutine = 5
	var wg sync.WaitGroup
	errCh := make(chan error, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(start int) {
			defer wg.Done()
			ids := make([]string, perGoroutine)
			for i := 0; i < perGoroutine; i++ {
				ids[i] = fmt.Sprintf("a%d", start+i)
			}
			if err := svc.BatchAddAssets(a.ID, ids); err != nil {
				errCh <- err
			}
		}(g * perGoroutine)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		require.NoError(t, err)
	}

	// Verify: no position conflicts (all positions unique within this album)
	// and positions form a contiguous 0..N-1 range with N=20.
	positions := queryPositions(t, db, a.ID)
	require.Len(t, positions, total, "all 20 assets should be in the album")
	seen := map[int]string{}
	for aid, pos := range positions {
		if other, dup := seen[pos]; dup {
			t.Fatalf("position %d collision: %s and %s", pos, aid, other)
		}
		seen[pos] = aid
	}
	for i := 0; i < total; i++ {
		_, ok := seen[i]
		require.True(t, ok, "position %d should be assigned", i)
	}
}
