package sqlite_test

import (
	"math"
	"os"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
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
