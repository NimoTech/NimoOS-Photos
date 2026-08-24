package sqlite_test

import (
	"database/sql"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/common"
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

	// Build a common.CLIPDim-dim unit vector (all 1/sqrt(dim))
	dim := common.CLIPDim
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

	// Should be able to write and read back deleted_at / original_path
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
		require.True(t, cols[c], "persons missing column %s", c)
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

func TestMigrateClipDimUpgrade(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "photos.db")

	// Manually build a legacy 512-dim DB (sqlite_vec.Auto() is already registered in the package init)
	raw, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE VIRTUAL TABLE clip_embeddings USING vec0(embedding float[512])`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE asset_clip_idx (rowid INTEGER PRIMARY KEY, asset_id TEXT UNIQUE NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO asset_clip_idx(rowid, asset_id) VALUES (1, 'a1')`); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	// Open should recognize the dimension mismatch, DROP+rebuild, and clear the mapping table
	db, err := sqlite.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	var ddl string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE name='clip_embeddings'`).Scan(&ddl); err != nil {
		t.Fatalf("read ddl: %v", err)
	}
	want := fmt.Sprintf("float[%d]", common.CLIPDim)
	if !strings.Contains(ddl, want) {
		t.Errorf("ddl %q missing %q", ddl, want)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM asset_clip_idx`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("asset_clip_idx not cleared, %d rows left", n)
	}
}

// TestMigrateClipDimIdempotent verifies that migrateClipDim is a no-op when the
// existing clip_embeddings table already matches common.CLIPDim: reopening must
// NOT drop the vec0 table or clear asset_clip_idx / clip_embeddings rows.
func TestMigrateClipDimIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "photos.db")

	db, err := sqlite.Open(path)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	if _, err := db.Exec(`INSERT INTO assets (id, file_path, status) VALUES (?, ?, ?)`,
		"asset-001", "/photos/idempotent.jpg", "indexed"); err != nil {
		t.Fatalf("insert asset failed: %v", err)
	}

	dim := common.CLIPDim
	vec := make([]float32, dim)
	val := float32(1.0 / math.Sqrt(float64(dim)))
	for i := range vec {
		vec[i] = val
	}
	blob := sqlite.SerializeFloat32(vec)

	if _, err := db.Exec(`INSERT INTO asset_clip_idx (rowid, asset_id) VALUES (1, ?)`, "asset-001"); err != nil {
		t.Fatalf("insert asset_clip_idx failed: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO clip_embeddings (rowid, embedding) VALUES (1, ?)`, blob); err != nil {
		t.Fatalf("insert clip_embeddings failed: %v", err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("close failed: %v", err)
	}

	// Reopen: dimension already matches common.CLIPDim, migrateClipDim must
	// take the no-op branch and leave the vec0 table + mapping untouched.
	db2, err := sqlite.Open(path)
	if err != nil {
		t.Fatalf("reopen failed: %v", err)
	}
	defer db2.Close()

	var idxCount int
	if err := db2.QueryRow(`SELECT COUNT(*) FROM asset_clip_idx`).Scan(&idxCount); err != nil {
		t.Fatalf("count asset_clip_idx failed: %v", err)
	}
	if idxCount != 1 {
		t.Errorf("expected 1 asset_clip_idx row to survive reopen, got %d", idxCount)
	}

	var vecCount int
	if err := db2.QueryRow(`SELECT COUNT(*) FROM clip_embeddings`).Scan(&vecCount); err != nil {
		t.Fatalf("count clip_embeddings failed: %v", err)
	}
	if vecCount != 1 {
		t.Errorf("expected 1 clip_embeddings row to survive reopen, got %d", vecCount)
	}

	var gotBlob []byte
	if err := db2.QueryRow(`SELECT embedding FROM clip_embeddings WHERE rowid = 1`).Scan(&gotBlob); err != nil {
		t.Fatalf("read back embedding failed: %v", err)
	}
	gotVec := sqlite.DeserializeFloat32(gotBlob)
	if len(gotVec) != dim {
		t.Fatalf("expected %d-dim vector, got %d", dim, len(gotVec))
	}
	for i, f := range gotVec {
		if math.Abs(float64(f-val)) > 1e-6 {
			t.Errorf("vector element %d mismatch: got %f want %f", i, f, val)
			break
		}
	}
}

// TestMigrateFaceScannedFreshDB verifies that a brand-new database has the
// assets.face_scanned column, defaulting to 0 for newly-inserted rows.
func TestMigrateFaceScannedFreshDB(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "t.db"))
	require.NoError(t, err)
	defer db.Close()

	_, err = db.Exec(`INSERT INTO assets(id, file_path, status) VALUES('a1','/g/1.jpg','pending')`)
	require.NoError(t, err)

	var scanned int
	require.NoError(t, db.QueryRow(`SELECT face_scanned FROM assets WHERE id='a1'`).Scan(&scanned))
	require.Equal(t, 0, scanned, "face_scanned should default to 0 for a new asset")
}

// TestMigrateFaceScannedBackfillOnUpgrade simulates a legacy DB created before
// assets.face_scanned existed: assets table has no such column. Opening it
// through sqlite.Open must add the column and, in the same pass, mark
// already-indexed assets as scanned (smooth upgrade, no mass rescan) while
// leaving non-indexed assets at 0.
func TestMigrateFaceScannedBackfillOnUpgrade(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy.db")

	// Phase 1: manually build a "legacy DB" -- the assets table has no face_scanned column.
	func() {
		raw, err := sql.Open("sqlite3", dbPath)
		require.NoError(t, err)
		defer raw.Close()
		_, err = raw.Exec(`CREATE TABLE assets (
			id                   TEXT PRIMARY KEY,
			file_path            TEXT UNIQUE NOT NULL,
			file_size            INTEGER,
			mime_type            TEXT,
			original_name        TEXT,
			taken_at             DATETIME,
			duration_ms          INTEGER,
			live_photo_video_id  TEXT,
			is_live_photo_video  INTEGER NOT NULL DEFAULT 0,
			indexed_at           DATETIME,
			status               TEXT NOT NULL DEFAULT 'pending',
			checksum             TEXT
		)`)
		require.NoError(t, err)
		_, err = raw.Exec(`INSERT INTO assets(id, file_path, status) VALUES
			('done1', '/g/1.jpg', 'indexed'),
			('done2', '/g/2.jpg', 'indexed'),
			('todo1', '/g/3.jpg', 'pending')`)
		require.NoError(t, err)
	}()

	// Phase 2: sqlite.Open triggers the migration, which should add the column and backfill indexed assets in one shot.
	db, err := sqlite.Open(dbPath)
	require.NoError(t, err)
	defer db.Close()

	got := map[string]int{}
	rows, err := db.Query(`SELECT id, face_scanned FROM assets`)
	require.NoError(t, err)
	for rows.Next() {
		var id string
		var fs int
		require.NoError(t, rows.Scan(&id, &fs))
		got[id] = fs
	}
	require.NoError(t, rows.Err())
	rows.Close()
	require.Equal(t, map[string]int{"done1": 1, "done2": 1, "todo1": 0}, got)
}

// TestMigrateFaceScannedBackfillIsIdempotent verifies that once the column
// exists, reopening the DB never re-runs the one-time backfill — a
// face_scanned value that legitimate app logic reset to 0 (e.g. reprocessing)
// must not be silently flipped back to 1 by a later migrate() pass.
func TestMigrateFaceScannedBackfillIsIdempotent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy2.db")
	func() {
		raw, err := sql.Open("sqlite3", dbPath)
		require.NoError(t, err)
		defer raw.Close()
		_, err = raw.Exec(`CREATE TABLE assets (
			id                   TEXT PRIMARY KEY,
			file_path            TEXT UNIQUE NOT NULL,
			file_size            INTEGER,
			mime_type            TEXT,
			original_name        TEXT,
			taken_at             DATETIME,
			duration_ms          INTEGER,
			live_photo_video_id  TEXT,
			is_live_photo_video  INTEGER NOT NULL DEFAULT 0,
			indexed_at           DATETIME,
			status               TEXT NOT NULL DEFAULT 'pending',
			checksum             TEXT
		)`)
		require.NoError(t, err)
		_, err = raw.Exec(`INSERT INTO assets(id, file_path, status) VALUES('a1','/g/1.jpg','indexed')`)
		require.NoError(t, err)
	}()

	// First open: column doesn't exist, triggers a one-time backfill (indexed -> face_scanned=1).
	func() {
		db, err := sqlite.Open(dbPath)
		require.NoError(t, err)
		defer db.Close()
	}()

	// Simulate normal business logic resetting face_scanned to 0 (e.g. reprocessing this asset).
	raw2, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = raw2.Exec(`UPDATE assets SET face_scanned=0 WHERE id='a1'`)
	require.NoError(t, err)
	require.NoError(t, raw2.Close())

	// Reopen: column already exists, should not re-trigger the backfill; face_scanned should keep the app-written 0.
	db2, err := sqlite.Open(dbPath)
	require.NoError(t, err)
	defer db2.Close()

	var fs int
	require.NoError(t, db2.QueryRow(`SELECT face_scanned FROM assets WHERE id='a1'`).Scan(&fs))
	require.Equal(t, 0, fs, "repeated migration should not re-trigger the backfill")
}

// TestMigrateOCRLinesUpgrade verifies: after a legacy DB (asset_ocr exists
// with no boxes_ver column, no asset_ocr_lines table) is migrated via Open,
// the new column defaults to 0, the new table is writable, deleting an
// assets row cascades to clear asset_ocr_lines via the foreign key, and
// repeated Open is idempotent.
func TestMigrateOCRLinesUpgrade(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy.db")

	// Phase 1: manually build a legacy DB -- asset_ocr has only the old columns.
	func() {
		raw, err := sql.Open("sqlite3", dbPath)
		require.NoError(t, err)
		defer raw.Close()
		_, err = raw.Exec(`CREATE TABLE assets (
			id                   TEXT PRIMARY KEY,
			file_path            TEXT UNIQUE NOT NULL,
			file_size            INTEGER,
			mime_type            TEXT,
			original_name        TEXT,
			taken_at             DATETIME,
			duration_ms          INTEGER,
			live_photo_video_id  TEXT,
			is_live_photo_video  INTEGER NOT NULL DEFAULT 0,
			indexed_at           DATETIME,
			status               TEXT NOT NULL DEFAULT 'pending',
			checksum             TEXT
		)`)
		require.NoError(t, err)
		_, err = raw.Exec(`CREATE TABLE asset_ocr (
			asset_id TEXT PRIMARY KEY REFERENCES assets(id) ON DELETE CASCADE,
			text     TEXT NOT NULL DEFAULT '',
			ocr_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`)
		require.NoError(t, err)
		_, err = raw.Exec(`INSERT INTO assets(id, file_path, status) VALUES('a1','/g/1.jpg','indexed')`)
		require.NoError(t, err)
		_, err = raw.Exec(`INSERT INTO asset_ocr(asset_id, text) VALUES('a1','hello world')`)
		require.NoError(t, err)
	}()

	// Phase 2: Open triggers the migration.
	db, err := sqlite.Open(dbPath)
	require.NoError(t, err)

	var ver int
	require.NoError(t, db.QueryRow(`SELECT boxes_ver FROM asset_ocr WHERE asset_id='a1'`).Scan(&ver))
	require.Equal(t, 0, ver, "legacy data's boxes_ver should default to 0 (pending backfill)")

	_, err = db.Exec(`INSERT INTO asset_ocr_lines(asset_id, line_no, text, box, score)
		VALUES('a1', 0, 'hello world', '[0.1,0.1,0.5,0.1,0.5,0.2,0.1,0.2]', 0.97)`)
	require.NoError(t, err)

	// Foreign key cascade: delete the assets row, the lines table should empty out.
	_, err = db.Exec(`DELETE FROM assets WHERE id='a1'`)
	require.NoError(t, err)
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM asset_ocr_lines`).Scan(&n))
	require.Equal(t, 0, n, "asset_ocr_lines should cascade-clear after deleting the asset")
	db.Close()

	// Phase 3: a second Open is idempotent.
	db2, err := sqlite.Open(dbPath)
	require.NoError(t, err)
	db2.Close()
}

// TestMigrateDocClassifyColumns verifies the doc-classification migration:
// asset_ocr's four new columns' defaults, clip_text_cache is writable, and
// repeated Open is idempotent.
func TestMigrateDocClassifyColumns(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "doc.db")
	db, err := sqlite.Open(dbPath)
	require.NoError(t, err)

	_, err = db.Exec(`INSERT INTO assets(id, file_path, status) VALUES('a1','/g/1.jpg','indexed')`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO asset_ocr(asset_id, text) VALUES('a1','hello')`)
	require.NoError(t, err)

	var docVer int
	var isDoc sql.NullInt64
	require.NoError(t, db.QueryRow(
		`SELECT doc_ver, is_doc FROM asset_ocr WHERE asset_id='a1'`).Scan(&docVer, &isDoc))
	require.Equal(t, 0, docVer, "doc_ver defaults to 0 (pending computation)")
	require.False(t, isDoc.Valid, "is_doc defaults to NULL (not yet computed, distinct from 0=classified as non-document)")

	_, err = db.Exec(`INSERT INTO clip_text_cache(key, gen, vec) VALUES('a scan of a document','3',x'00000000')`)
	require.NoError(t, err)
	db.Close()

	db2, err := sqlite.Open(dbPath)
	require.NoError(t, err)
	db2.Close()
}

// TestMigrateAssetsAestheticScore verifies: the assets table carries an
// aesthetic_score column, defaulting to NULL (not yet scored) for a new
// asset; repeated Open is idempotent, doesn't error, and doesn't clear existing values.
func TestMigrateAssetsAestheticScore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "photos.db")

	db, err := sqlite.Open(path)
	require.NoError(t, err)

	_, err = db.Exec(`INSERT INTO assets(id, file_path, checksum, status) VALUES('a1','/x','c1','indexed')`)
	require.NoError(t, err)
	var score sql.NullFloat64
	require.NoError(t, db.QueryRow(`SELECT aesthetic_score FROM assets WHERE id='a1'`).Scan(&score))
	require.False(t, score.Valid, "the new column should default to NULL")
	require.NoError(t, db.Close())

	// Repeated open should be idempotent, no error, no clearing of the existing NULL value.
	db2, err := sqlite.Open(path)
	require.NoError(t, err)
	defer db2.Close()
	require.NoError(t, db2.QueryRow(`SELECT aesthetic_score FROM assets WHERE id='a1'`).Scan(&score))
	require.False(t, score.Valid, "repeated migration should not change the existing NULL value")
}

// TestMigrateAssetsCaptionSynced verifies: the assets table carries a
// caption_synced column (photo knowledge-base ingestion marker), defaulting
// to 0 (not handed off to Parser) for a new asset; repeated Open is
// idempotent, doesn't error, and doesn't clear existing values.
func TestMigrateAssetsCaptionSynced(t *testing.T) {
	dir := t.TempDir()
	db, err := sqlite.Open(filepath.Join(dir, "photos.db"))
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO assets(id, file_path, checksum, status) VALUES('a1','/x','c1','indexed')`)
	require.NoError(t, err)
	var v int
	require.NoError(t, db.QueryRow(`SELECT caption_synced FROM assets WHERE id='a1'`).Scan(&v))
	require.Equal(t, 0, v, "the new column should default to 0 (not handed off to Parser)")
	require.NoError(t, db.Close())

	// Repeated open should be idempotent, no error, no clearing of existing values.
	db2, err := sqlite.Open(filepath.Join(dir, "photos.db"))
	require.NoError(t, err)
	defer db2.Close()
	require.NoError(t, db2.QueryRow(`SELECT caption_synced FROM assets WHERE id='a1'`).Scan(&v))
	require.Equal(t, 0, v, "repeated migration should not change existing values")
}

// TestMigrateSmartViewMatchesOrigin verifies: upgrading a legacy DB should
// add smart_view_matches' origin column (0=auto match/1=manual pin/2=manual
// exclude), defaulting to 0; a second Open is idempotent.
func TestMigrateSmartViewMatchesOrigin(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "photos.db")

	db, err := sqlite.Open(path)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO assets(id, file_path, checksum, status) VALUES('a1','/x','c1','indexed')`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO smart_views(id, name, conds_raw, conds_parsed) VALUES('sv1','v','[]','[]')`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO smart_view_matches(smart_view_id, asset_id, match_score) VALUES('sv1','a1',0.8)`)
	require.NoError(t, err)
	var origin int
	require.NoError(t, db.QueryRow(`SELECT origin FROM smart_view_matches WHERE asset_id='a1'`).Scan(&origin))
	require.Equal(t, 0, origin, "should default to 0 (auto match)")
	require.NoError(t, db.Close())

	// Repeated open should be idempotent, no error, no clearing of existing values.
	db2, err := sqlite.Open(path)
	require.NoError(t, err)
	defer db2.Close()
	require.NoError(t, db2.QueryRow(`SELECT origin FROM smart_view_matches WHERE asset_id='a1'`).Scan(&origin))
	require.Equal(t, 0, origin, "repeated migration should not change existing values")
}

// ── Exemplar/KNN assignment foundation (face_person.exemplar/confirmed,
//    person_suggestions, person_negatives) ──────────────────────────────

// TestFacePersonExemplarConfirmedFreshDB verifies a brand-new DB's face_person
// table carries exemplar/confirmed columns defaulting to 0 (not yet an
// exemplar / not user-confirmed).
func TestFacePersonExemplarConfirmedFreshDB(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "fresh.db"))
	require.NoError(t, err)
	defer db.Close()

	_, err = db.Exec(`INSERT INTO assets(id, file_path, checksum, status) VALUES('a1','/x','c1','indexed')`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO face_detections(id, asset_id, bbox, embedding) VALUES('f1','a1','[]',x'00')`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO persons(id) VALUES('p1')`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO face_person(face_id, person_id) VALUES('f1','p1')`)
	require.NoError(t, err)

	var exemplar, confirmed int
	require.NoError(t, db.QueryRow(`SELECT exemplar, confirmed FROM face_person WHERE face_id='f1'`).Scan(&exemplar, &confirmed))
	require.Equal(t, 0, exemplar, "exemplar should default to 0")
	require.Equal(t, 0, confirmed, "confirmed should default to 0")
}

// TestFacePersonExemplarConfirmedUpgrade verifies upgrading a legacy DB whose
// face_person table predates the exemplar/confirmed columns: sqlite.Open must
// add both columns (defaulting to 0) without disturbing existing rows, and a
// second Open must be idempotent (no "duplicate column" error).
func TestFacePersonExemplarConfirmedUpgrade(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy_facePerson.db")

	// Phase 1: manually build a legacy DB -- face_person has no exemplar/confirmed columns.
	func() {
		raw, err := sql.Open("sqlite3", dbPath)
		require.NoError(t, err)
		defer raw.Close()
		_, err = raw.Exec(`CREATE TABLE assets (
			id                   TEXT PRIMARY KEY,
			file_path            TEXT UNIQUE NOT NULL,
			file_size            INTEGER,
			mime_type            TEXT,
			original_name        TEXT,
			taken_at             DATETIME,
			duration_ms          INTEGER,
			live_photo_video_id  TEXT,
			is_live_photo_video  INTEGER NOT NULL DEFAULT 0,
			indexed_at           DATETIME,
			status               TEXT NOT NULL DEFAULT 'pending',
			checksum             TEXT
		)`)
		require.NoError(t, err)
		_, err = raw.Exec(`CREATE TABLE face_detections (
			id TEXT PRIMARY KEY, asset_id TEXT NOT NULL, bbox TEXT NOT NULL, embedding BLOB NOT NULL
		)`)
		require.NoError(t, err)
		_, err = raw.Exec(`CREATE TABLE persons (id TEXT PRIMARY KEY, name TEXT NOT NULL DEFAULT '')`)
		require.NoError(t, err)
		_, err = raw.Exec(`CREATE TABLE face_person (face_id TEXT PRIMARY KEY, person_id TEXT)`)
		require.NoError(t, err)
		_, err = raw.Exec(`INSERT INTO assets(id, file_path, checksum, status) VALUES('a1','/x','c1','indexed')`)
		require.NoError(t, err)
		_, err = raw.Exec(`INSERT INTO face_detections(id, asset_id, bbox, embedding) VALUES('f1','a1','[]',x'00')`)
		require.NoError(t, err)
		_, err = raw.Exec(`INSERT INTO persons(id) VALUES('p1')`)
		require.NoError(t, err)
		_, err = raw.Exec(`INSERT INTO face_person(face_id, person_id) VALUES('f1','p1')`)
		require.NoError(t, err)
	}()

	// Phase 2: sqlite.Open triggers the migration, which should add both columns.
	db, err := sqlite.Open(dbPath)
	require.NoError(t, err)

	var exemplar, confirmed int
	require.NoError(t, db.QueryRow(`SELECT exemplar, confirmed FROM face_person WHERE face_id='f1'`).Scan(&exemplar, &confirmed))
	require.Equal(t, 0, exemplar, "upgraded rows should default exemplar to 0")
	require.Equal(t, 0, confirmed, "upgraded rows should default confirmed to 0")
	require.NoError(t, db.Close())

	// Repeated open must be idempotent (no "duplicate column" error).
	db2, err := sqlite.Open(dbPath)
	require.NoError(t, err)
	defer db2.Close()
	require.NoError(t, db2.QueryRow(`SELECT exemplar, confirmed FROM face_person WHERE face_id='f1'`).Scan(&exemplar, &confirmed))
	require.Equal(t, 0, exemplar)
	require.Equal(t, 0, confirmed)
}

// TestPersonSuggestionsTableFreshDB verifies the person_suggestions table
// (join/review suggestion queue) exists on a fresh DB with the expected
// columns, CHECK constraints, and UNIQUE(person_id, face_id).
func TestPersonSuggestionsTableFreshDB(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "sugg.db"))
	require.NoError(t, err)
	defer db.Close()

	var n int
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='person_suggestions'`).Scan(&n))
	require.Equal(t, 1, n)

	_, err = db.Exec(`INSERT INTO persons(id) VALUES('p1')`)
	require.NoError(t, err)

	// Valid 'join' kind, default status 'open'.
	_, err = db.Exec(`INSERT INTO person_suggestions(id, person_id, face_id, kind, score, created_at)
		VALUES('s1','p1','f1','join',0.5,'2026-08-20T00:00:00Z')`)
	require.NoError(t, err)
	var status string
	require.NoError(t, db.QueryRow(`SELECT status FROM person_suggestions WHERE id='s1'`).Scan(&status))
	require.Equal(t, "open", status, "status should default to 'open'")

	// Valid 'review' kind.
	_, err = db.Exec(`INSERT INTO person_suggestions(id, person_id, face_id, kind, score, created_at)
		VALUES('s2','p1','f2','review',0.6,'2026-08-20T00:00:00Z')`)
	require.NoError(t, err)

	// Invalid kind (e.g. the spec's literal 'merge', explicitly dropped per
	// the plan's revision -- merge suggestions still go through the existing
	// merge-suggestions endpoint) must be rejected by the CHECK constraint.
	_, err = db.Exec(`INSERT INTO person_suggestions(id, person_id, face_id, kind, score, created_at)
		VALUES('s3','p1','f3','merge',0.5,'2026-08-20T00:00:00Z')`)
	require.Error(t, err, "kind='merge' must be rejected -- only 'join'/'review' are valid")

	// Invalid status must be rejected by the CHECK constraint.
	_, err = db.Exec(`INSERT INTO person_suggestions(id, person_id, face_id, kind, score, status, created_at)
		VALUES('s4','p1','f4','join',0.5,'pending','2026-08-20T00:00:00Z')`)
	require.Error(t, err, "status must be one of open/accepted/rejected")

	// UNIQUE(person_id, face_id) must be enforced.
	_, err = db.Exec(`INSERT INTO person_suggestions(id, person_id, face_id, kind, score, created_at)
		VALUES('s5','p1','f1','review',0.7,'2026-08-20T00:00:00Z')`)
	require.Error(t, err, "duplicate (person_id, face_id) must violate the UNIQUE constraint")
}

// TestPersonNegativesTableFreshDB verifies the person_negatives table
// (KNN-assignment negative feedback: this face must never re-attach to this
// person) exists on a fresh DB with a composite PK.
func TestPersonNegativesTableFreshDB(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "neg.db"))
	require.NoError(t, err)
	defer db.Close()

	var n int
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='person_negatives'`).Scan(&n))
	require.Equal(t, 1, n)

	_, err = db.Exec(`INSERT INTO person_negatives(person_id, face_id, created_at) VALUES('p1','f1','2026-08-20T00:00:00Z')`)
	require.NoError(t, err)

	// Duplicate (person_id, face_id) must violate the composite PK.
	_, err = db.Exec(`INSERT INTO person_negatives(person_id, face_id, created_at) VALUES('p1','f1','2026-08-20T00:00:01Z')`)
	require.Error(t, err, "duplicate (person_id, face_id) must violate the PRIMARY KEY constraint")
}

// TestIdxSuggestionsOpenIndex verifies the idx_suggestions_open index exists,
// used by the open-suggestions-queue listing (status, person_id).
func TestIdxSuggestionsOpenIndex(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "idx.db"))
	require.NoError(t, err)
	defer db.Close()

	var n int
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='index' AND name='idx_suggestions_open'`).Scan(&n))
	require.Equal(t, 1, n)
}

// TestMigrateAddsSuggestionTablesOnUpgrade verifies upgrading a legacy DB
// that predates person_suggestions/person_negatives: sqlite.Open must create
// both tables (and the index) without disturbing existing data, and a second
// Open must be idempotent.
func TestMigrateAddsSuggestionTablesOnUpgrade(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy_suggestions.db")

	// Phase 1: manually build a legacy DB -- no person_suggestions/person_negatives at all.
	func() {
		raw, err := sql.Open("sqlite3", dbPath)
		require.NoError(t, err)
		defer raw.Close()
		_, err = raw.Exec(`CREATE TABLE assets (
			id                   TEXT PRIMARY KEY,
			file_path            TEXT UNIQUE NOT NULL,
			file_size            INTEGER,
			mime_type            TEXT,
			original_name        TEXT,
			taken_at             DATETIME,
			duration_ms          INTEGER,
			live_photo_video_id  TEXT,
			is_live_photo_video  INTEGER NOT NULL DEFAULT 0,
			indexed_at           DATETIME,
			status               TEXT NOT NULL DEFAULT 'pending',
			checksum             TEXT
		)`)
		require.NoError(t, err)
		_, err = raw.Exec(`CREATE TABLE persons (id TEXT PRIMARY KEY, name TEXT NOT NULL DEFAULT '')`)
		require.NoError(t, err)
		_, err = raw.Exec(`INSERT INTO persons(id) VALUES('p1')`)
		require.NoError(t, err)
	}()

	// Phase 2: sqlite.Open triggers the migration, which should create both tables + index.
	db, err := sqlite.Open(dbPath)
	require.NoError(t, err)

	var n int
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='person_suggestions'`).Scan(&n))
	require.Equal(t, 1, n)
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='person_negatives'`).Scan(&n))
	require.Equal(t, 1, n)
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='index' AND name='idx_suggestions_open'`).Scan(&n))
	require.Equal(t, 1, n)

	_, err = db.Exec(`INSERT INTO person_suggestions(id, person_id, face_id, kind, score, created_at)
		VALUES('s1','p1','f1','join',0.5,'2026-08-20T00:00:00Z')`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	// Repeated open must be idempotent.
	db2, err := sqlite.Open(dbPath)
	require.NoError(t, err)
	defer db2.Close()
	var got string
	require.NoError(t, db2.QueryRow(`SELECT id FROM person_suggestions WHERE id='s1'`).Scan(&got))
	require.Equal(t, "s1", got, "repeated migration should not disturb existing rows")
}

// TestMergeSuggestionsTableFreshDB verifies the merge_suggestions table
// (cluster-merge questions, gray-band candidates from the apple engine's HAC
// stop line) exists on a fresh DB with its CHECK/UNIQUE constraints enforced.
// The pair is stored canonically (person_a < person_b, direction carried
// separately by into_is_a) specifically so the same unordered pair can never
// produce two rows under a flipped direction -- see the CREATE TABLE's doc
// comment in pkg/sqlite/db.go.
func TestMergeSuggestionsTableFreshDB(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "mergesugg.db"))
	require.NoError(t, err)
	defer db.Close()

	var n int
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='merge_suggestions'`).Scan(&n))
	require.Equal(t, 1, n)
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='index' AND name='idx_merge_suggestions_open'`).Scan(&n))
	require.Equal(t, 1, n)

	_, err = db.Exec(`INSERT INTO persons(id) VALUES('p1'), ('p2')`)
	require.NoError(t, err)

	// Default status 'open'. into_is_a=1 means p1 (person_a) is the merge target.
	_, err = db.Exec(`INSERT INTO merge_suggestions(id, person_a, person_b, into_is_a, dist, created_at)
		VALUES('m1','p1','p2',1,0.58,'2026-08-20T00:00:00Z')`)
	require.NoError(t, err)
	var status string
	require.NoError(t, db.QueryRow(`SELECT status FROM merge_suggestions WHERE id='m1'`).Scan(&status))
	require.Equal(t, "open", status, "status should default to 'open'")

	// Invalid status must be rejected by the CHECK constraint.
	_, err = db.Exec(`INSERT INTO merge_suggestions(id, person_a, person_b, into_is_a, dist, status, created_at)
		VALUES('m2','p1','p2',1,0.58,'pending','2026-08-20T00:00:00Z')`)
	require.Error(t, err, "status must be one of open/accepted/rejected")

	// person_a > person_b must be rejected by the CHECK constraint -- the
	// pair must always be inserted in canonical (orderPair) order.
	_, err = db.Exec(`INSERT INTO merge_suggestions(id, person_a, person_b, into_is_a, dist, created_at)
		VALUES('m-badorder','p2','p1',0,0.58,'2026-08-20T00:00:00Z')`)
	require.Error(t, err, "person_a > person_b must violate the CHECK(person_a < person_b) constraint")

	// UNIQUE(person_a, person_b) must be enforced regardless of direction:
	// this is the whole point of the canonical-pair schema -- the physical
	// pair (p1,p2) can never produce two rows even if into_is_a differs.
	_, err = db.Exec(`INSERT INTO merge_suggestions(id, person_a, person_b, into_is_a, dist, created_at)
		VALUES('m3','p1','p2',0,0.60,'2026-08-20T00:00:01Z')`)
	require.Error(t, err, "duplicate (person_a, person_b) must violate the UNIQUE constraint, even with a different into_is_a")

	// ON CONFLICT DO UPDATE ... WHERE status='open' must refresh dist/
	// into_is_a for an open row (mirrors the generation stage's upsert).
	_, err = db.Exec(`INSERT INTO merge_suggestions(id, person_a, person_b, into_is_a, dist, created_at)
		VALUES('m4','p1','p2',0,0.60,'2026-08-20T00:00:01Z')
		ON CONFLICT(person_a, person_b) DO UPDATE SET dist=excluded.dist, into_is_a=excluded.into_is_a WHERE merge_suggestions.status='open'`)
	require.NoError(t, err)
	var dist float64
	var intoIsA int
	require.NoError(t, db.QueryRow(`SELECT dist, into_is_a FROM merge_suggestions WHERE id='m1'`).Scan(&dist, &intoIsA))
	require.Equal(t, 0.60, dist, "the open row's dist should have been refreshed by the upsert")
	require.Equal(t, 0, intoIsA, "the open row's direction should have been refreshed by the upsert too")

	// A decided row must be immutable to the same upsert.
	_, err = db.Exec(`UPDATE merge_suggestions SET status='rejected', decided_at='2026-08-20T00:00:02Z' WHERE id='m1'`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO merge_suggestions(id, person_a, person_b, into_is_a, dist, created_at)
		VALUES('m5','p1','p2',1,0.61,'2026-08-20T00:00:03Z')
		ON CONFLICT(person_a, person_b) DO UPDATE SET dist=excluded.dist, into_is_a=excluded.into_is_a WHERE merge_suggestions.status='open'`)
	require.NoError(t, err)
	require.NoError(t, db.QueryRow(`SELECT dist, into_is_a FROM merge_suggestions WHERE id='m1'`).Scan(&dist, &intoIsA))
	require.Equal(t, 0.60, dist, "a decided (rejected) row must not be touched by the upsert")
	require.Equal(t, 0, intoIsA, "a decided (rejected) row's direction must not be touched by the upsert either")
}

// TestFaceNegativePairsTableFreshDB verifies the face_negative_pairs table
// (durable cannot-link between two representative faces, written on a
// rejected cluster-merge question) exists on a fresh DB with a composite PK.
func TestFaceNegativePairsTableFreshDB(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "negpairs.db"))
	require.NoError(t, err)
	defer db.Close()

	var n int
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='face_negative_pairs'`).Scan(&n))
	require.Equal(t, 1, n)

	_, err = db.Exec(`INSERT INTO face_negative_pairs(face_a, face_b, created_at) VALUES('fa','fb','2026-08-20T00:00:00Z')`)
	require.NoError(t, err)

	// Duplicate (face_a, face_b) must violate the composite PK.
	_, err = db.Exec(`INSERT INTO face_negative_pairs(face_a, face_b, created_at) VALUES('fa','fb','2026-08-20T00:00:01Z')`)
	require.Error(t, err, "duplicate (face_a, face_b) must violate the PRIMARY KEY constraint")
}

// TestMigrateAddsMergeQuestionTablesOnUpgrade verifies upgrading a legacy DB
// that predates merge_suggestions/face_negative_pairs: sqlite.Open must
// create both tables (and the index) without disturbing existing data, and a
// second Open must be idempotent.
func TestMigrateAddsMergeQuestionTablesOnUpgrade(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy_merge_questions.db")

	// Phase 1: manually build a legacy DB -- no merge_suggestions/face_negative_pairs at all.
	func() {
		raw, err := sql.Open("sqlite3", dbPath)
		require.NoError(t, err)
		defer raw.Close()
		_, err = raw.Exec(`CREATE TABLE assets (
			id                   TEXT PRIMARY KEY,
			file_path            TEXT UNIQUE NOT NULL,
			file_size            INTEGER,
			mime_type            TEXT,
			original_name        TEXT,
			taken_at             DATETIME,
			duration_ms          INTEGER,
			live_photo_video_id  TEXT,
			is_live_photo_video  INTEGER NOT NULL DEFAULT 0,
			indexed_at           DATETIME,
			status               TEXT NOT NULL DEFAULT 'pending',
			checksum             TEXT
		)`)
		require.NoError(t, err)
		_, err = raw.Exec(`CREATE TABLE persons (id TEXT PRIMARY KEY, name TEXT NOT NULL DEFAULT '')`)
		require.NoError(t, err)
		_, err = raw.Exec(`INSERT INTO persons(id) VALUES('p1'), ('p2')`)
		require.NoError(t, err)
	}()

	// Phase 2: sqlite.Open triggers the migration, which should create both tables + index.
	db, err := sqlite.Open(dbPath)
	require.NoError(t, err)

	var n int
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='merge_suggestions'`).Scan(&n))
	require.Equal(t, 1, n)
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='face_negative_pairs'`).Scan(&n))
	require.Equal(t, 1, n)
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='index' AND name='idx_merge_suggestions_open'`).Scan(&n))
	require.Equal(t, 1, n)

	_, err = db.Exec(`INSERT INTO merge_suggestions(id, person_a, person_b, into_is_a, dist, created_at)
		VALUES('m1','p1','p2',1,0.58,'2026-08-20T00:00:00Z')`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	// Repeated open must be idempotent.
	db2, err := sqlite.Open(dbPath)
	require.NoError(t, err)
	defer db2.Close()
	var got string
	require.NoError(t, db2.QueryRow(`SELECT id FROM merge_suggestions WHERE id='m1'`).Scan(&got))
	require.Equal(t, "m1", got, "repeated migration should not disturb existing rows")
}

// TestCalibrationStateTableFreshDB verifies the calibration_state table
// (device self-calibrated effective values, one row per key) exists on a
// fresh DB with the expected columns and a PRIMARY KEY on key.
func TestCalibrationStateTableFreshDB(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "calibstate.db"))
	require.NoError(t, err)
	defer db.Close()

	var n int
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='calibration_state'`).Scan(&n))
	require.Equal(t, 1, n)

	_, err = db.Exec(`INSERT INTO calibration_state(key, value, model_gen, updated_at)
		VALUES('knn.threshold', 0.42, 'gen4', '2026-08-23T00:00:00Z')`)
	require.NoError(t, err)

	var value float64
	var modelGen string
	require.NoError(t, db.QueryRow(`SELECT value, model_gen FROM calibration_state WHERE key='knn.threshold'`).
		Scan(&value, &modelGen))
	require.Equal(t, 0.42, value)
	require.Equal(t, "gen4", modelGen)

	// Duplicate key must violate the PRIMARY KEY constraint.
	_, err = db.Exec(`INSERT INTO calibration_state(key, value, model_gen, updated_at)
		VALUES('knn.threshold', 0.50, 'gen4', '2026-08-23T00:00:01Z')`)
	require.Error(t, err, "duplicate key must violate the PRIMARY KEY constraint")
}

// TestCalibrationHistoryTableFreshDB verifies the calibration_history table
// (append-only audit log, one row per tier per calibration attempt) exists
// on a fresh DB with the expected index, and that its CHECK constraints
// reject unknown tier/outcome values.
func TestCalibrationHistoryTableFreshDB(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "calibhistory.db"))
	require.NoError(t, err)
	defer db.Close()

	var n int
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='calibration_history'`).Scan(&n))
	require.Equal(t, 1, n)
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='index' AND name='idx_calibration_history_tier'`).Scan(&n))
	require.Equal(t, 1, n)

	_, err = db.Exec(`INSERT INTO calibration_history(run_at, model_gen, tier, truth_counts, old_values, new_values, outcome)
		VALUES('2026-08-23T00:00:00Z', 'gen4', 'knn', '{"positives":123,"negatives":30,"persons":9}', '{}', '{}', 'applied')`)
	require.NoError(t, err)

	// Unknown tier must violate the CHECK(tier IN (...)) constraint.
	_, err = db.Exec(`INSERT INTO calibration_history(run_at, model_gen, tier, truth_counts, old_values, new_values, outcome)
		VALUES('2026-08-23T00:00:01Z', 'gen4', 'bogus', '{}', '{}', '{}', 'applied')`)
	require.Error(t, err, "unknown tier must violate the CHECK constraint")

	// Unknown outcome must violate the CHECK(outcome IN (...)) constraint.
	_, err = db.Exec(`INSERT INTO calibration_history(run_at, model_gen, tier, truth_counts, old_values, new_values, outcome)
		VALUES('2026-08-23T00:00:02Z', 'gen4', 'merge', '{}', '{}', '{}', 'bogus')`)
	require.Error(t, err, "unknown outcome must violate the CHECK constraint")

	// All valid tier/outcome combinations must be accepted.
	for _, tier := range []string{"knn", "merge", "twopass"} {
		for _, outcome := range []string{"applied", "held_insufficient", "held_hysteresis", "held_skewed", "clamped", "invariant_violation"} {
			_, err = db.Exec(`INSERT INTO calibration_history(run_at, model_gen, tier, truth_counts, old_values, new_values, outcome)
				VALUES('2026-08-23T00:00:03Z', 'gen4', ?, '{}', '{}', '{}', ?)`, tier, outcome)
			require.NoError(t, err, "tier=%s outcome=%s must be accepted", tier, outcome)
		}
	}
}

// TestMigrateAddsCalibrationTablesOnUpgrade verifies upgrading a legacy DB
// that predates calibration_state/calibration_history: sqlite.Open must
// create both tables (and the index) without disturbing existing data, and a
// second Open must be idempotent.
func TestMigrateAddsCalibrationTablesOnUpgrade(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy_calibration.db")

	// Phase 1: manually build a legacy DB -- no calibration tables at all.
	func() {
		raw, err := sql.Open("sqlite3", dbPath)
		require.NoError(t, err)
		defer raw.Close()
		_, err = raw.Exec(`CREATE TABLE assets (
			id                   TEXT PRIMARY KEY,
			file_path            TEXT UNIQUE NOT NULL,
			file_size            INTEGER,
			mime_type            TEXT,
			original_name        TEXT,
			taken_at             DATETIME,
			duration_ms          INTEGER,
			live_photo_video_id  TEXT,
			is_live_photo_video  INTEGER NOT NULL DEFAULT 0,
			indexed_at           DATETIME,
			status               TEXT NOT NULL DEFAULT 'pending',
			checksum             TEXT
		)`)
		require.NoError(t, err)
	}()

	// Phase 2: sqlite.Open triggers the migration, which should create both tables + index.
	db, err := sqlite.Open(dbPath)
	require.NoError(t, err)

	var n int
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='calibration_state'`).Scan(&n))
	require.Equal(t, 1, n)
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='calibration_history'`).Scan(&n))
	require.Equal(t, 1, n)
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='index' AND name='idx_calibration_history_tier'`).Scan(&n))
	require.Equal(t, 1, n)

	_, err = db.Exec(`INSERT INTO calibration_state(key, value, model_gen, updated_at)
		VALUES('knn.threshold', 0.42, 'gen4', '2026-08-23T00:00:00Z')`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	// Repeated open must be idempotent.
	db2, err := sqlite.Open(dbPath)
	require.NoError(t, err)
	defer db2.Close()
	var got string
	require.NoError(t, db2.QueryRow(`SELECT key FROM calibration_state WHERE key='knn.threshold'`).Scan(&got))
	require.Equal(t, "knn.threshold", got, "repeated migration should not disturb existing rows")
}
