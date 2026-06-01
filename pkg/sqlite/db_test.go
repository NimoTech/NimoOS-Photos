package sqlite_test

import (
	"database/sql"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	"github.com/stretchr/testify/require"
)

// TestOpenDB verifies that Open creates all required tables.
func TestOpenDB(t *testing.T) {
	path := t.TempDir() + "/test.db"
	db, err := sqlite.Open(path)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer db.Close()
	defer os.Remove(path)

	expectedTables := []string{
		"assets",
		"asset_exif",
		"face_detections",
		"face_person",
		"persons",
		"albums",
		"album_assets",
		"asset_clip_idx",
		"merge_rejections",
	}

	for _, table := range expectedTables {
		var name string
		err := db.QueryRow(
			"SELECT name FROM sqlite_master WHERE type IN ('table','shadow') AND name = ?",
			table,
		).Scan(&name)
		if err != nil {
			t.Errorf("table %q not found: %v", table, err)
		}
	}
}

// TestSQLiteVec verifies that the sqlite-vec virtual table is operational.
func TestSQLiteVec(t *testing.T) {
	path := t.TempDir() + "/vec_test.db"
	db, err := sqlite.Open(path)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer db.Close()
	defer os.Remove(path)

	// Insert a test asset first (required by FK)
	_, err = db.Exec(`INSERT INTO assets (id, file_path, status) VALUES (?, ?, ?)`,
		"asset-001", "/photos/test.jpg", "indexed")
	if err != nil {
		t.Fatalf("insert asset failed: %v", err)
	}

	// Build a 512-dim unit vector (all 1/sqrt(512))
	dim := 512
	vec := make([]float32, dim)
	val := float32(1.0 / math.Sqrt(float64(dim)))
	for i := range vec {
		vec[i] = val
	}
	blob := sqlite.SerializeFloat32(vec)

	// Insert into asset_clip_idx and clip_embeddings using same rowid
	_, err = db.Exec(`INSERT INTO asset_clip_idx (rowid, asset_id) VALUES (1, ?)`, "asset-001")
	if err != nil {
		t.Fatalf("insert asset_clip_idx failed: %v", err)
	}
	_, err = db.Exec(`INSERT INTO clip_embeddings (rowid, embedding) VALUES (1, ?)`, blob)
	if err != nil {
		t.Fatalf("insert clip_embeddings failed: %v", err)
	}

	// KNN query
	rows, err := db.Query(`
		SELECT idx.asset_id, vec.distance
		FROM clip_embeddings AS vec
		JOIN asset_clip_idx AS idx ON idx.rowid = vec.rowid
		WHERE vec.embedding MATCH ? AND k = 5
		ORDER BY vec.distance
	`, blob)
	if err != nil {
		t.Fatalf("KNN query failed: %v", err)
	}
	defer rows.Close()

	found := false
	for rows.Next() {
		var assetID string
		var distance float64
		if err := rows.Scan(&assetID, &distance); err != nil {
			t.Fatalf("scan failed: %v", err)
		}
		t.Logf("asset_id=%s distance=%f", assetID, distance)
		if assetID == "asset-001" && distance < 0.001 {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows error: %v", err)
	}
	if !found {
		t.Error("expected asset-001 with distance < 0.001, not found")
	}
}

// TestSerializeDeserialize verifies round-trip encoding of float32 slices.
func TestSerializeDeserialize(t *testing.T) {
	original := []float32{1.0, 2.5, -3.14, 0.0, 1e-7}
	blob := sqlite.SerializeFloat32(original)
	result := sqlite.DeserializeFloat32(blob)

	if len(result) != len(original) {
		t.Fatalf("length mismatch: got %d want %d", len(result), len(original))
	}
	for i, v := range original {
		if result[i] != v {
			t.Errorf("index %d: got %v want %v", i, result[i], v)
		}
	}
}

func TestMigrateAssetFavoritesTable(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	defer db.Close()

	var name string
	err = db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='asset_favorites'`).Scan(&name)
	require.NoError(t, err)
	require.Equal(t, "asset_favorites", name)

	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='index' AND tbl_name='asset_favorites' ORDER BY name`)
	require.NoError(t, err)
	defer rows.Close()
	var indices []string
	for rows.Next() {
		var n string
		require.NoError(t, rows.Scan(&n))
		indices = append(indices, n)
	}
	require.Contains(t, indices, "idx_fav_asset")
	require.Contains(t, indices, "idx_fav_user_time")
}

func TestAssetFavoritesCascadeOnAssetDelete(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	defer db.Close()

	_, err = db.Exec(`INSERT INTO assets(id, file_path, status) VALUES('a1','/x.jpg','indexed')`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO asset_favorites(user_id, asset_id) VALUES('default','a1')`)
	require.NoError(t, err)

	_, err = db.Exec(`DELETE FROM assets WHERE id='a1'`)
	require.NoError(t, err)

	var cnt int
	err = db.QueryRow(`SELECT COUNT(*) FROM asset_favorites WHERE asset_id='a1'`).Scan(&cnt)
	require.NoError(t, err)
	require.Equal(t, 0, cnt, "expected cascade delete to clear favorite row")
}

func TestMigrateAddsTrashColumns(t *testing.T) {
	dir := t.TempDir()
	db, err := sqlite.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	// 应能写入并读回 deleted_at / original_path
	_, err = db.Exec(`INSERT INTO assets (id, file_path, status, deleted_at, original_path)
		VALUES ('x1', '/DATA/Gallery/.trash/x1/a.jpg', 'indexed', CURRENT_TIMESTAMP, '/DATA/Gallery/a.jpg')`)
	if err != nil {
		t.Fatalf("insert with trash cols: %v", err)
	}
	var op string
	if err := db.QueryRow(`SELECT original_path FROM assets WHERE id='x1'`).Scan(&op); err != nil {
		t.Fatalf("select original_path: %v", err)
	}
	if op != "/DATA/Gallery/a.jpg" {
		t.Fatalf("original_path = %q, want /DATA/Gallery/a.jpg", op)
	}
}

func TestMigrateAddsNewColumns(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	db, err := sqlite.Open(dbPath)
	require.NoError(t, err)
	db.Close()

	// Second open must be idempotent (no error, no duplicate column attempts).
	db2, err := sqlite.Open(dbPath)
	require.NoError(t, err)
	defer db2.Close()

	rows, err := db2.Query(`PRAGMA table_info(asset_exif)`)
	require.NoError(t, err)
	defer rows.Close()

	cols := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		require.NoError(t, rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk))
		cols[name] = true
	}

	for _, c := range []string{
		"iso", "shutter_speed", "aperture", "focal_length", "orientation",
		"video_codec", "audio_codec", "frame_rate", "bit_rate", "rotation",
	} {
		require.True(t, cols[c], "expected column %s to exist", c)
	}
}

func TestPersonsExtendedColumns(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "m.db"))
	require.NoError(t, err)
	defer db.Close()

	cols := map[string]bool{}
	rows, err := db.Query(`PRAGMA table_info(persons)`)
	require.NoError(t, err)
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt sql.NullString
		require.NoError(t, rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk))
		cols[name] = true
	}
	rows.Close()
	for _, c := range []string{"cover_face_id", "favorite", "relation", "hidden", "confidence", "centroid", "created_at", "updated_at"} {
		require.True(t, cols[c], "persons 缺列 %s", c)
	}
}

func TestMergeRejectionsTable(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "r.db"))
	require.NoError(t, err)
	defer db.Close()

	// Insert two persons so FK constraints are satisfied.
	_, err = db.Exec(`INSERT INTO persons(id) VALUES('p-a'),('p-b')`)
	require.NoError(t, err)

	// Valid ordered pair must succeed.
	_, err = db.Exec(`INSERT INTO merge_rejections(person_a, person_b) VALUES('p-a','p-b')`)
	require.NoError(t, err)

	// Reversed pair (b, a) must be rejected by the CHECK (person_a < person_b) constraint.
	_, err = db.Exec(`INSERT INTO merge_rejections(person_a, person_b) VALUES('p-b','p-a')`)
	require.Error(t, err, "expected CHECK constraint to reject reversed pair")
}

func TestMigrateAlbumAssetsHasPositionColumn(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	defer db.Close()

	rows, err := db.Query(`PRAGMA table_info(album_assets)`)
	require.NoError(t, err)
	defer rows.Close()

	cols := map[string]struct {
		ctype   string
		notnull int
	}{}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		require.NoError(t, rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk))
		cols[name] = struct {
			ctype   string
			notnull int
		}{ctype, notnull}
	}
	pos, ok := cols["position"]
	require.True(t, ok, "album_assets.position column should exist")
	require.Equal(t, "INTEGER", pos.ctype)
	require.Equal(t, 1, pos.notnull)

	var idxName string
	err = db.QueryRow(`SELECT name FROM sqlite_master WHERE type='index' AND name='idx_album_assets_position'`).Scan(&idxName)
	require.NoError(t, err, "idx_album_assets_position index should exist")
	require.Equal(t, "idx_album_assets_position", idxName)
}

func TestMigrateAlbumAssetsBackfillsPosition(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")

	// Phase 1: seed DB to simulate legacy (all position=0) state.
	func() {
		db, err := sqlite.Open(dbPath)
		require.NoError(t, err)
		defer db.Close()

		_, err = db.Exec(`INSERT INTO assets(id, file_path, status) VALUES('a1','/g/1.jpg','indexed'),('a2','/g/2.jpg','indexed'),('a3','/g/3.jpg','indexed')`)
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO albums(id, name) VALUES('al1','A')`)
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO album_assets(album_id, asset_id, added_at, position) VALUES
			('al1','a1','2026-01-01',0),
			('al1','a2','2026-01-02',0),
			('al1','a3','2026-01-03',0)`)
		require.NoError(t, err)
	}()

	// Phase 2: re-open should trigger backfill.
	db, err := sqlite.Open(dbPath)
	require.NoError(t, err)
	defer db.Close()

	rows, err := db.Query(`SELECT asset_id, position FROM album_assets WHERE album_id='al1' ORDER BY position`)
	require.NoError(t, err)
	defer rows.Close()
	var got [][2]any
	for rows.Next() {
		var aid string
		var pos int
		require.NoError(t, rows.Scan(&aid, &pos))
		got = append(got, [2]any{aid, pos})
	}
	require.Equal(t, [][2]any{{"a1", 0}, {"a2", 1}, {"a3", 2}}, got)
}

func TestMigrateAlbumAssetsBackfillIsIdempotent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")

	// Phase 1: seed legacy state.
	func() {
		db, err := sqlite.Open(dbPath)
		require.NoError(t, err)
		defer db.Close()

		_, err = db.Exec(`INSERT INTO assets(id, file_path, status) VALUES('a1','/g/1.jpg','indexed'),('a2','/g/2.jpg','indexed')`)
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO albums(id, name) VALUES('al1','A')`)
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO album_assets(album_id, asset_id, added_at, position) VALUES
			('al1','a1','2026-01-01',0),
			('al1','a2','2026-01-02',0)`)
		require.NoError(t, err)
	}()

	// Phase 2: first re-open triggers backfill (0,1).
	func() {
		db, err := sqlite.Open(dbPath)
		require.NoError(t, err)
		defer db.Close()
	}()

	// Phase 3: second re-open should be a no-op. Assert positions still 0,1.
	db, err := sqlite.Open(dbPath)
	require.NoError(t, err)
	defer db.Close()

	rows, err := db.Query(`SELECT asset_id, position FROM album_assets WHERE album_id='al1' ORDER BY position`)
	require.NoError(t, err)
	defer rows.Close()
	got := map[string]int{}
	for rows.Next() {
		var aid string
		var pos int
		require.NoError(t, rows.Scan(&aid, &pos))
		got[aid] = pos
	}
	require.Equal(t, map[string]int{"a1": 0, "a2": 1}, got)
}

func TestMigrateAlbumAssetsBackfillSkipsSingleItemAlbum(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")

	func() {
		db, err := sqlite.Open(dbPath)
		require.NoError(t, err)
		defer db.Close()

		_, err = db.Exec(`INSERT INTO assets(id, file_path, status) VALUES('solo','/g/s.jpg','indexed')`)
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO albums(id, name) VALUES('al-solo','Solo')`)
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO album_assets(album_id, asset_id, added_at, position) VALUES
			('al-solo','solo','2026-01-01',0)`)
		require.NoError(t, err)
	}()

	db, err := sqlite.Open(dbPath)
	require.NoError(t, err)
	defer db.Close()

	var pos int
	require.NoError(t, db.QueryRow(`SELECT position FROM album_assets WHERE album_id='al-solo' AND asset_id='solo'`).Scan(&pos))
	require.Equal(t, 0, pos, "single-item album position must remain 0 (not falsely backfilled)")
}

func TestMigrateCreatesGeoTables(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "t.db"))
	require.NoError(t, err)
	defer db.Close()

	var n int
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='asset_geo'`).Scan(&n))
	require.Equal(t, 1, n)
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='place_cover_overrides'`).Scan(&n))
	require.Equal(t, 1, n)

	db2, err := sqlite.Open(filepath.Join(t.TempDir(), "t2.db"))
	require.NoError(t, err)
	db2.Close()
}
