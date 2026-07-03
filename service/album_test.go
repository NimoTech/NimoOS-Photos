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

// --- Default cover resolution (falls back to first-position asset when none set) ---

func TestAlbumListResolvesFirstPositionAsDefaultCover(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	defer db.Close()

	// Assets inserted with taken_at desc so the "newest" heuristic would pick 'new',
	// but position order (old=0, mid=1, new=2) means 'old' is the first item.
	_, err = db.Exec(`INSERT INTO assets(id, file_path, taken_at, status) VALUES
		('old','/g/1.jpg','2026-01-01','indexed'),
		('mid','/g/2.jpg','2026-02-01','indexed'),
		('new','/g/3.jpg','2026-03-01','indexed')`)
	require.NoError(t, err)
	svc := service.NewAlbumService(db)
	a, _ := svc.Create("No Cover Set")
	require.NoError(t, svc.AddAsset(a.ID, "old"))
	require.NoError(t, svc.AddAsset(a.ID, "mid"))
	require.NoError(t, svc.AddAsset(a.ID, "new"))

	albums, err := svc.List()
	require.NoError(t, err)
	require.Len(t, albums, 1)
	require.Equal(t, "old", albums[0].CoverAssetID, "default cover should be the first-position asset, not newest")
}

func TestAlbumListExplicitCoverWinsOverFirst(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	defer db.Close()

	_, err = db.Exec(`INSERT INTO assets(id, file_path, taken_at, status) VALUES
		('old','/g/1.jpg','2026-01-01','indexed'),
		('new','/g/2.jpg','2026-03-01','indexed')`)
	require.NoError(t, err)
	svc := service.NewAlbumService(db)
	a, _ := svc.Create("Explicit Cover")
	// old=pos0, new=pos1; implicit cover would be 'old'
	require.NoError(t, svc.AddAsset(a.ID, "old"))
	require.NoError(t, svc.AddAsset(a.ID, "new"))
	// explicitly pin 'new' as cover — must override the implicit first-item fallback
	require.NoError(t, svc.UpdateCover(a.ID, "new"))

	albums, err := svc.List()
	require.NoError(t, err)
	require.Len(t, albums, 1)
	require.Equal(t, "new", albums[0].CoverAssetID, "explicit cover must win over first-position fallback")
}

func TestAlbumListEmptyAlbumHasNoCover(t *testing.T) {
	svc, cleanup := openTestAlbumSvc(t)
	defer cleanup()

	a, _ := svc.Create("Empty")

	albums, err := svc.List()
	require.NoError(t, err)
	require.Len(t, albums, 1)
	require.Equal(t, a.ID, albums[0].ID)
	require.Empty(t, albums[0].CoverAssetID, "empty album must have no cover")
}

func TestAlbumGetResolvesFirstPositionAsDefaultCover(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	defer db.Close()

	// 'new' has a later taken_at; 'old' is added first so position=0.
	// Implicit cover must be 'old' (first by position), not 'new'.
	_, err = db.Exec(`INSERT INTO assets(id, file_path, taken_at, status) VALUES
		('old','/g/1.jpg','2026-01-01','indexed'),
		('new','/g/2.jpg','2026-03-01','indexed')`)
	require.NoError(t, err)
	svc := service.NewAlbumService(db)
	a, _ := svc.Create("Get Cover")
	require.NoError(t, svc.AddAsset(a.ID, "old"))
	require.NoError(t, svc.AddAsset(a.ID, "new"))

	got, err := svc.Get(a.ID)
	require.NoError(t, err)
	require.Equal(t, "old", got.CoverAssetID, "Get should resolve first-position asset as default cover")
}

// --- Cover stability: adding photos must not change an implicit cover ---

// TestAlbumImplicitCoverIsStableWhenAddingPhotos verifies that the implicit
// cover is always the first-position item; subsequent additions never change it.
func TestAlbumImplicitCoverIsStableWhenAddingPhotos(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	defer db.Close()

	_, err = db.Exec(`INSERT INTO assets(id, file_path, taken_at, status) VALUES
		('a','/g/a.jpg','2026-01-01','indexed'),
		('b','/g/b.jpg','2026-02-01','indexed'),
		('c','/g/c.jpg','2026-03-01','indexed')`)
	require.NoError(t, err)
	svc := service.NewAlbumService(db)
	al, _ := svc.Create("Stability")

	// Add A first — it becomes position=0 (the first item).
	require.NoError(t, svc.AddAsset(al.ID, "a"))

	got, err := svc.Get(al.ID)
	require.NoError(t, err)
	require.Equal(t, "a", got.CoverAssetID, "after adding A, cover should be A")

	// Add B (newer taken_at, position=1) — cover must remain A.
	require.NoError(t, svc.AddAsset(al.ID, "b"))
	got, err = svc.Get(al.ID)
	require.NoError(t, err)
	require.Equal(t, "a", got.CoverAssetID, "after adding B, implicit cover must still be A")

	// Add C (newest taken_at, position=2) — cover must still be A.
	require.NoError(t, svc.AddAsset(al.ID, "c"))
	got, err = svc.Get(al.ID)
	require.NoError(t, err)
	require.Equal(t, "a", got.CoverAssetID, "after adding C, implicit cover must still be A")

	// Confirm via List() as well.
	albums, err := svc.List()
	require.NoError(t, err)
	require.Len(t, albums, 1)
	require.Equal(t, "a", albums[0].CoverAssetID, "List cover must also be A")
}

// --- Bug 2: removing the explicit cover falls back correctly ---

// TestAlbumRemoveExplicitCoverFallsBackToFirst verifies that when the asset
// pinned as cover is removed from the album, the cover falls back to the next
// first-position item and the dangling cover_asset_id is cleared in the DB.
func TestAlbumRemoveExplicitCoverFallsBackToFirst(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	defer db.Close()

	_, err = db.Exec(`INSERT INTO assets(id, file_path, status) VALUES
		('a','/g/a.jpg','indexed'),('b','/g/b.jpg','indexed')`)
	require.NoError(t, err)
	svc := service.NewAlbumService(db)
	al, _ := svc.Create("ExplicitCoverRemove")
	require.NoError(t, svc.AddAsset(al.ID, "a")) // pos=0
	require.NoError(t, svc.AddAsset(al.ID, "b")) // pos=1
	require.NoError(t, svc.UpdateCover(al.ID, "b"))

	// Remove the pinned cover; cover must fall back to 'a'.
	require.NoError(t, svc.RemoveAsset(al.ID, "b"))

	got, err := svc.Get(al.ID)
	require.NoError(t, err)
	require.Equal(t, "a", got.CoverAssetID, "after removing explicit cover B, cover should fall back to A")

	// Write-side hygiene: cover_asset_id must be cleared in the DB (not a dangling pointer).
	var rawCover string
	require.NoError(t, db.QueryRow(`SELECT COALESCE(cover_asset_id,'') FROM albums WHERE id=?`, al.ID).Scan(&rawCover))
	require.Empty(t, rawCover, "cover_asset_id must be cleared in albums row after removing the pinned cover")
}

// TestAlbumRemoveNonCoverKeepsExplicitCover verifies that removing an asset
// that is NOT the pinned cover leaves the explicit cover intact.
func TestAlbumRemoveNonCoverKeepsExplicitCover(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	defer db.Close()

	_, err = db.Exec(`INSERT INTO assets(id, file_path, status) VALUES
		('a','/g/a.jpg','indexed'),('b','/g/b.jpg','indexed')`)
	require.NoError(t, err)
	svc := service.NewAlbumService(db)
	al, _ := svc.Create("NonCoverRemove")
	require.NoError(t, svc.AddAsset(al.ID, "a")) // pos=0, cover pinned to a
	require.NoError(t, svc.AddAsset(al.ID, "b")) // pos=1
	require.NoError(t, svc.UpdateCover(al.ID, "a"))

	// Remove B (not the cover) — cover must remain A.
	require.NoError(t, svc.RemoveAsset(al.ID, "b"))

	got, err := svc.Get(al.ID)
	require.NoError(t, err)
	require.Equal(t, "a", got.CoverAssetID, "removing non-cover B must not change explicit cover A")
}

// TestAlbumDanglingCoverFallsBackToFirstMember simulates a dangling cover_asset_id
// (asset removed from album_assets by external means, bypassing RemoveAsset).
// The read-side membership guard must ignore the dangling id and return the first member.
func TestAlbumDanglingCoverFallsBackToFirstMember(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	defer db.Close()

	_, err = db.Exec(`INSERT INTO assets(id, file_path, status) VALUES
		('a','/g/a.jpg','indexed'),('b','/g/b.jpg','indexed')`)
	require.NoError(t, err)
	svc := service.NewAlbumService(db)
	al, _ := svc.Create("DanglingCover")
	require.NoError(t, svc.AddAsset(al.ID, "a")) // pos=0
	require.NoError(t, svc.AddAsset(al.ID, "b")) // pos=1
	require.NoError(t, svc.UpdateCover(al.ID, "b"))

	// Simulate purge / external removal: delete album_assets row directly,
	// bypassing RemoveAsset so cover_asset_id in albums still points to 'b'.
	_, err = db.Exec(`DELETE FROM album_assets WHERE album_id=? AND asset_id='b'`, al.ID)
	require.NoError(t, err)

	// Read-side guard must detect 'b' is no longer a member and fall back to 'a'.
	got, err := svc.Get(al.ID)
	require.NoError(t, err)
	require.Equal(t, "a", got.CoverAssetID,
		"dangling cover_asset_id must be ignored; cover should fall back to first member")

	// Also verify via List().
	albums, err := svc.List()
	require.NoError(t, err)
	require.Len(t, albums, 1)
	require.Equal(t, "a", albums[0].CoverAssetID,
		"List must also apply the membership guard and return the first member")
}

// --- Taken-at span (min/max of album assets' taken_at) ---

func TestAlbumListReturnsTakenAtSpan(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	defer db.Close()

	_, err = db.Exec(`INSERT INTO assets(id, file_path, taken_at, status) VALUES
		('a1','/g/1.jpg','2023-05-01','indexed'),
		('a2','/g/2.jpg','2024-08-15','indexed'),
		('a3','/g/3.jpg','2025-02-20','indexed')`)
	require.NoError(t, err)
	svc := service.NewAlbumService(db)
	a, _ := svc.Create("Spanning")
	require.NoError(t, svc.AddAsset(a.ID, "a1"))
	require.NoError(t, svc.AddAsset(a.ID, "a2"))
	require.NoError(t, svc.AddAsset(a.ID, "a3"))

	albums, err := svc.List()
	require.NoError(t, err)
	require.Len(t, albums, 1)
	require.Equal(t, "2023-05-01", albums[0].DateStart)
	require.Equal(t, "2025-02-20", albums[0].DateEnd)
}

func TestAlbumListSpanEmptyWhenNoDatedAssets(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	defer db.Close()

	// Asset has no taken_at; empty album below has nothing at all.
	_, err = db.Exec(`INSERT INTO assets(id, file_path, status) VALUES('a1','/g/1.jpg','indexed')`)
	require.NoError(t, err)
	svc := service.NewAlbumService(db)
	a, _ := svc.Create("No Dates")
	require.NoError(t, svc.AddAsset(a.ID, "a1"))

	albums, err := svc.List()
	require.NoError(t, err)
	require.Len(t, albums, 1)
	require.Empty(t, albums[0].DateStart)
	require.Empty(t, albums[0].DateEnd)
}

func TestAlbumGetReturnsTakenAtSpan(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	defer db.Close()

	_, err = db.Exec(`INSERT INTO assets(id, file_path, taken_at, status) VALUES
		('a1','/g/1.jpg','2025-06-03','indexed'),
		('a2','/g/2.jpg','2025-09-30','indexed')`)
	require.NoError(t, err)
	svc := service.NewAlbumService(db)
	a, _ := svc.Create("Get Span")
	require.NoError(t, svc.AddAsset(a.ID, "a1"))
	require.NoError(t, svc.AddAsset(a.ID, "a2"))

	got, err := svc.Get(a.ID)
	require.NoError(t, err)
	require.Equal(t, "2025-06-03", got.DateStart)
	require.Equal(t, "2025-09-30", got.DateEnd)
}

// --- Per-album photo / video counts (live-photo videos and trashed excluded) ---

func TestAlbumListReturnsPhotoVideoCounts(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	defer db.Close()

	_, err = db.Exec(`INSERT INTO assets(id, file_path, mime_type, is_live_photo_video, status) VALUES
		('p1','/g/1.jpg','image/jpeg',0,'indexed'),
		('p2','/g/2.png','image/png',0,'indexed'),
		('v1','/g/3.mp4','video/mp4',0,'indexed')`)
	require.NoError(t, err)
	svc := service.NewAlbumService(db)
	a, _ := svc.Create("Mixed")
	require.NoError(t, svc.AddAsset(a.ID, "p1"))
	require.NoError(t, svc.AddAsset(a.ID, "p2"))
	require.NoError(t, svc.AddAsset(a.ID, "v1"))

	albums, err := svc.List()
	require.NoError(t, err)
	require.Len(t, albums, 1)
	require.Equal(t, 2, albums[0].PhotoCount)
	require.Equal(t, 1, albums[0].VideoCount)
}

// TestAlbumListAssetsExcludesOffline verifies that AlbumService.ListAssets hides
// assets whose removable drive is currently unplugged (offline=1), while
// PhotoCount/VideoCount from List() also stop counting them.
func TestAlbumListAssetsExcludesOffline(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	defer db.Close()

	_, err = db.Exec(`INSERT INTO assets(id, file_path, mime_type, is_live_photo_video, status) VALUES
		('online','/g/1.jpg','image/jpeg',0,'indexed'),
		('offline','/media/X/2.jpg','image/jpeg',0,'indexed')`)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE assets SET offline=1 WHERE id='offline'`)
	require.NoError(t, err)

	svc := service.NewAlbumService(db)
	a, err := svc.Create("Offline Test")
	require.NoError(t, err)
	require.NoError(t, svc.AddAsset(a.ID, "online"))
	require.NoError(t, svc.AddAsset(a.ID, "offline"))

	assets, err := svc.ListAssets(a.ID)
	require.NoError(t, err)
	require.Len(t, assets, 1, "offline 资产必须从相册内容中隐藏")
	require.Equal(t, "online", assets[0].ID)

	albums, err := svc.List()
	require.NoError(t, err)
	require.Len(t, albums, 1)
	require.Equal(t, 1, albums[0].PhotoCount, "photo_cnt 不应计入 offline 资产")
}

func TestAlbumListPhotoVideoCountsExcludeLiveAndTrashed(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	defer db.Close()

	_, err = db.Exec(`INSERT INTO assets(id, file_path, mime_type, is_live_photo_video, deleted_at, status) VALUES
		('p1','/g/1.jpg','image/jpeg',0,NULL,'indexed'),
		('lv','/g/2.mov','video/quicktime',1,NULL,'indexed'),
		('del','/g/3.jpg','image/jpeg',0,'2026-01-01','indexed')`)
	require.NoError(t, err)
	svc := service.NewAlbumService(db)
	a, _ := svc.Create("Filtered")
	require.NoError(t, svc.AddAsset(a.ID, "p1"))
	require.NoError(t, svc.AddAsset(a.ID, "lv"))
	require.NoError(t, svc.AddAsset(a.ID, "del"))

	albums, err := svc.List()
	require.NoError(t, err)
	require.Len(t, albums, 1)
	require.Equal(t, 1, albums[0].PhotoCount, "live-photo video and trashed asset excluded")
	require.Equal(t, 0, albums[0].VideoCount, "the only video is a live-photo companion")
}
