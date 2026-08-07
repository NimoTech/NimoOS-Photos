package service_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/stretchr/testify/require"
)

// Re-scanning an asset whose content changed (face_scanned reset to 0) must
// replace its old detections instead of stacking new ones on top.
func TestRescanReplacesOldFaces(t *testing.T) {
	db := makeTestFaceDB(t)
	vec := make([]float32, 512)
	vec[0] = 1.0
	insertAssetFace(t, db, "a1", normalize(vec)) // "old content" face

	// Simulate content change: reset face_scanned, keep the stale face row,
	// and point file_path at a readable file for the rescan.
	p := filepath.Join(t.TempDir(), "a1.jpg")
	require.NoError(t, os.WriteFile(p, []byte("new-content"), 0o644))
	_, err := db.Exec(`UPDATE assets SET face_scanned=0, status='indexed', file_path=? WHERE id='a1'`, p)
	require.NoError(t, err)

	fs := service.NewFaceService(db)
	fs.SetML(&pipelineMockML{facesPer: 1}) // mock returning exactly 1 face
	require.NoError(t, fs.RunPipeline(context.Background()))

	var n int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM face_detections WHERE asset_id='a1'`).Scan(&n))
	require.Equal(t, 1, n, "old face must be deleted, only the fresh detection remains")
}
