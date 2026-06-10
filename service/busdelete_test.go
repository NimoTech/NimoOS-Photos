package service

import (
	"path/filepath"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// extractDeletedPaths
// ---------------------------------------------------------------------------

func TestExtractDeletedPaths_ValidMessage(t *testing.T) {
	msg := []byte(`{
		"sourceID":"nimoos",
		"name":"nimoos:media:deleted",
		"properties":{"paths":"[\"/DATA/a.jpg\",\"/DATA/b/c.png\"]"},
		"timestamp":1234567890,
		"uuid":"abc-123"
	}`)
	got := extractDeletedPaths(msg)
	require.Equal(t, []string{"/DATA/a.jpg", "/DATA/b/c.png"}, got)
}

func TestExtractDeletedPaths_WrongName(t *testing.T) {
	msg := []byte(`{
		"sourceID":"nimoos",
		"name":"nimoos:file:created",
		"properties":{"paths":"[\"/DATA/a.jpg\"]"},
		"timestamp":1,
		"uuid":"x"
	}`)
	require.Nil(t, extractDeletedPaths(msg))
}

func TestExtractDeletedPaths_InvalidPathsJSON(t *testing.T) {
	// properties.paths is not a valid JSON array string
	msg := []byte(`{
		"sourceID":"nimoos",
		"name":"nimoos:media:deleted",
		"properties":{"paths":"not-json"},
		"timestamp":1,
		"uuid":"x"
	}`)
	require.Nil(t, extractDeletedPaths(msg))
}

func TestExtractDeletedPaths_InvalidEnvelope(t *testing.T) {
	require.Nil(t, extractDeletedPaths([]byte(`{broken`)))
}

// ---------------------------------------------------------------------------
// shouldHandleDeletedPath
// ---------------------------------------------------------------------------

func TestShouldHandleDeletedPath(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"/a/b.jpg", true},        // supported media extension
		{"/a/b.txt", false},       // non-media extension
		{"/a/dirname", true},      // no extension → treat as directory
		{"/a/b.PNG", true},        // uppercase extension → supported
		{"/a/b.mp4", true},        // supported video extension
		{"/a/b.pdf", false},       // non-media extension
		{"/a/b.HEIC", true},       // uppercase supported
		{"/a/b.", false},          // dot with empty ext — filepath.Ext returns "." which is not in supportedExts
	}
	for _, tt := range tests {
		got := shouldHandleDeletedPath(tt.path)
		if got != tt.want {
			t.Errorf("shouldHandleDeletedPath(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// handleDeletedPaths — integration-style test using a real SQLite DB
// ---------------------------------------------------------------------------

func TestHandleDeletedPaths_CleansAssetAndVector(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "x.db"))
	require.NoError(t, err)
	defer db.Close()

	ix := NewIndexer(db, nil, t.TempDir(), 1)

	// Insert a fake asset pointing to a non-existent file (so pruneMissingUnder
	// will also treat it as gone).
	const assetID = "test-asset-001"
	const filePath = "/DATA/nonexistent/photo.jpg"

	_, err = db.Exec(
		`INSERT INTO assets(id,file_path,status,is_live_photo_video) VALUES(?,?,'indexed',0)`,
		assetID, filePath,
	)
	require.NoError(t, err)

	// Insert a matching CLIP embedding.
	_, err = db.Exec(`INSERT INTO asset_clip_idx(asset_id) VALUES(?)`, assetID)
	require.NoError(t, err)

	var rowid int64
	require.NoError(t, db.QueryRow(`SELECT rowid FROM asset_clip_idx WHERE asset_id=?`, assetID).Scan(&rowid))

	vec := make([]float32, 512)
	vec[0] = 0.5
	blob := sqlite.SerializeFloat32(vec)
	_, err = db.Exec(`INSERT INTO clip_embeddings(rowid,embedding) VALUES(?,?)`, rowid, blob)
	require.NoError(t, err)

	// Sanity: both rows exist before the call.
	var assetCount, vecCount int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM assets`).Scan(&assetCount))
	require.Equal(t, 1, assetCount, "pre-condition: asset exists")
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM clip_embeddings`).Scan(&vecCount))
	require.Equal(t, 1, vecCount, "pre-condition: clip vector exists")

	// Act: delete the path.
	handleDeletedPaths(ix, []string{filePath})

	// Assert: asset and vector are gone.
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM assets`).Scan(&assetCount))
	require.Equal(t, 0, assetCount, "asset must be removed")
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM clip_embeddings`).Scan(&vecCount))
	require.Equal(t, 0, vecCount, "clip vector must be removed")
}

func TestHandleDeletedPaths_NonMediaPathSkipped(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "y.db"))
	require.NoError(t, err)
	defer db.Close()

	ix := NewIndexer(db, nil, t.TempDir(), 1)

	// Insert a row for a .txt path — should not be touched.
	_, err = db.Exec(
		`INSERT INTO assets(id,file_path,status,is_live_photo_video) VALUES('a2','/DATA/note.txt','indexed',0)`,
	)
	require.NoError(t, err)

	handleDeletedPaths(ix, []string{"/DATA/note.txt"})

	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM assets`).Scan(&count))
	require.Equal(t, 1, count, "non-media asset must not be removed")
}
