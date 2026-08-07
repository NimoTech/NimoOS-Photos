package service_test

import (
	"context"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/stretchr/testify/require"
)

// writePlainJPEG creates a wxh JPEG at path and returns the path.
func writePlainJPEG(t *testing.T, dir, name string, w, h int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	p := filepath.Join(dir, name)
	f, err := os.Create(p)
	require.NoError(t, err)
	defer f.Close()
	require.NoError(t, jpeg.Encode(f, img, nil))
	return p
}

// When the cover face's asset is trashed, FaceThumbnail must fall back to
// another live face of the same person instead of 404ing.
func TestFaceThumbnailFallsBackToLiveFace(t *testing.T) {
	db := makeTestFaceDB(t)
	dir := t.TempDir()
	vec := make([]float32, 512)
	vec[0] = 1.0
	insertAssetFace(t, db, "a1", normalize(vec))
	insertAssetFace(t, db, "a2", normalize(vec))
	// Point both assets at real JPEG files with sane bboxes.
	p1 := writePlainJPEG(t, dir, "a1.jpg", 100, 100)
	p2 := writePlainJPEG(t, dir, "a2.jpg", 100, 100)
	_, err := db.Exec(`UPDATE assets SET file_path=? WHERE id='a1'`, p1)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE assets SET file_path=? WHERE id='a2'`, p2)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE face_detections SET bbox='{"x1":10,"y1":10,"x2":60,"y2":60}'`)
	require.NoError(t, err)

	require.NoError(t, service.NewFaceService(db).RunClustering(context.Background()))
	var pid, coverAsset string
	require.NoError(t, db.QueryRow(
		`SELECT id, cover_asset_id FROM persons`).Scan(&pid, &coverAsset))

	// Trash the cover asset (deleted_at set) — the other asset stays live.
	_, err = db.Exec(`UPDATE assets SET deleted_at=CURRENT_TIMESTAMP WHERE id=?`, coverAsset)
	require.NoError(t, err)

	ps := service.NewPersonService(db)
	out, err := ps.FaceThumbnail(pid, filepath.Join(dir, "cache"), dir)
	require.NoError(t, err, "must fall back to the remaining live face")
	require.FileExists(t, out)
}

// End-to-end: a 100x50 raw image with orientation=6 must produce a rotated
// crop rather than a mis-scaled one (bbox and EXIF dims are raw-space).
func TestFaceThumbnailOrientation6(t *testing.T) {
	db := makeTestFaceDB(t)
	dir := t.TempDir()
	vec := make([]float32, 512)
	vec[0] = 1.0
	insertAssetFace(t, db, "a1", normalize(vec))
	p1 := writePlainJPEG(t, dir, "a1.jpg", 100, 50)
	_, err := db.Exec(`UPDATE assets SET file_path=? WHERE id='a1'`, p1)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE face_detections SET bbox='{"x1":10,"y1":10,"x2":40,"y2":40}'`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO asset_exif(asset_id, width, height, orientation)
		VALUES('a1', 100, 50, 6)`)
	require.NoError(t, err)

	require.NoError(t, service.NewFaceService(db).RunClustering(context.Background()))
	var pid string
	require.NoError(t, db.QueryRow(`SELECT id FROM persons`).Scan(&pid))

	out, err := service.NewPersonService(db).FaceThumbnail(pid, filepath.Join(dir, "cache"), dir)
	require.NoError(t, err)
	require.FileExists(t, out)
}
