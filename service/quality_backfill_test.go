package service

import (
	"context"
	"database/sql"
	"image"
	"image/color"
	"image/jpeg"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/disintegration/imaging"
	"github.com/stretchr/testify/require"
)

// makeNoiseImage builds a wxh image of independent random pixels — a
// high-frequency source with a large Laplacian response everywhere.
func makeNoiseImage(w, h int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	rng := rand.New(rand.NewSource(1))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, color.NRGBA{
				R: uint8(rng.Intn(256)), G: uint8(rng.Intn(256)), B: uint8(rng.Intn(256)), A: 255,
			})
		}
	}
	return img
}

// writeJPEG encodes img as a JPEG at path, creating parent directories.
func writeJPEG(t *testing.T, path string, img image.Image) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	f, err := os.Create(path)
	require.NoError(t, err)
	defer f.Close()
	require.NoError(t, jpeg.Encode(f, img, &jpeg.Options{Quality: 95}))
}

// ---- pure function tests -------------------------------------------------

func TestSquashSharpness(t *testing.T) {
	require.Equal(t, 0.0, squashSharpness(0), "zero variance (a flat crop) must squash to exactly 0")
	require.InDelta(t, 0.5, squashSharpness(sharpnessK), 1e-9,
		"v==sharpnessK is the constructed half-point (mirrors mlserver's SHARPNESS_K)")
	require.Less(t, squashSharpness(50), squashSharpness(200), "monotonic increasing in v")
	require.Less(t, squashSharpness(1e12), 1.0, "asymptotic to 1, never reaching it")
	require.Equal(t, 0.0, squashSharpness(-5), "a negative (impossible in practice) variance must clamp to 0, not go negative")
}

func TestGrayLaplacianVariance_NoiseExceedsBlurred(t *testing.T) {
	noise := makeNoiseImage(112, 112)
	blurred := imaging.Blur(noise, 8)
	vNoise := grayLaplacianVariance(noise, noise.Bounds())
	vBlur := grayLaplacianVariance(blurred, blurred.Bounds())
	require.Greater(t, vNoise, vBlur,
		"high-frequency noise must score a higher Laplacian variance than its heavily blurred version")
}

func TestGrayLaplacianVariance_FlatImageIsZero(t *testing.T) {
	flat := image.NewNRGBA(image.Rect(0, 0, 20, 20))
	for y := 0; y < 20; y++ {
		for x := 0; x < 20; x++ {
			flat.SetNRGBA(x, y, color.NRGBA{R: 128, G: 128, B: 128, A: 255})
		}
	}
	require.Equal(t, 0.0, grayLaplacianVariance(flat, flat.Bounds()), "a flat crop has zero gradient everywhere")
}

func TestGrayLaplacianVariance_TooSmallRectIsZero(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	require.Equal(t, 0.0, grayLaplacianVariance(img, img.Bounds()), "a 2x2 rect has no interior point for a 3x3 kernel")
}

// TestGrayLaplacianVariance_CheckerboardAbsoluteScale pins the ABSOLUTE
// output scale, not just relative ordering — a regression test for a real
// bug where img.At().RGBA() (16-bit range, 0-65535) was fed straight into
// the luma weights without descaling to 8-bit, inflating the variance by
// ~257^2 (~66000x) and saturating squashSharpness to ~1.0 for virtually
// every real crop. Ordering-only tests (noise > blurred) pass either way
// and can't catch this; only an absolute value pins it down.
//
// Setup: a 4x4 checkerboard, pixel (x,y) white (255,255,255) when x+y is
// even, black (0,0,0) otherwise. Hand-derived expected variance: luma
// weights sum to exactly 1.0, so luma(white)=255.0 and luma(black)=0.0
// exactly (no rounding). Every interior pixel's 4-neighbors are all the
// opposite checkerboard color, so the Laplacian response at each interior
// point is exactly ±4*255=±1020 (center=black -> +1020, center=white ->
// -1020). The interior is the 2x2 grid x,y in {1,2}: (1,1)+(2,2) are white
// (lap=-1020), (1,2)+(2,1) are black (lap=+1020) — an even 2/2 split, so
// mean(lap)=0 and variance = mean(lap^2) = 1020^2 = 1,040,400 on the
// correct 8-bit scale. The 257x scale bug above would instead land around
// 1020^2 * 257^2 ≈ 6.87e10 — many orders of magnitude outside the
// tolerance below.
func TestGrayLaplacianVariance_CheckerboardAbsoluteScale(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 4, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			v := uint8(0)
			if (x+y)%2 == 0 {
				v = 255
			}
			img.SetNRGBA(x, y, color.NRGBA{R: v, G: v, B: v, A: 255})
		}
	}
	got := grayLaplacianVariance(img, img.Bounds())
	require.InDelta(t, 1040400.0, got, 1.0,
		"variance must be on the 8-bit luma scale (1020^2); a 257x (16-bit) scale bug would land ~6.6e4x higher")
}

// TestSquashSharpness_AbsoluteScaleBands is the end-to-end absolute-scale
// regression companion to the checkerboard test above: it pins
// squashSharpness(grayLaplacianVariance(...)) into bands that a 257x scale
// error would blow straight through (at that scale both ends saturate to
// ~1.0 and the bands below would be violated on the low end).
func TestSquashSharpness_AbsoluteScaleBands(t *testing.T) {
	noise := makeNoiseImage(112, 112)
	blurred := imaging.Blur(noise, 8)
	sNoise := squashSharpness(grayLaplacianVariance(noise, noise.Bounds()))
	sBlur := squashSharpness(grayLaplacianVariance(blurred, blurred.Bounds()))
	require.Greater(t, sNoise, 0.7, "unblurred high-frequency noise must land solidly sharp on the correct 8-bit scale")
	require.Less(t, sBlur, 0.6, "a heavily Gaussian-blurred crop must land clearly below the noise crop on the correct 8-bit scale")
}

// ---- BackfillSharpness integration tests ---------------------------------

func insertSharpnessTestAsset(t *testing.T, db *sql.DB, id, path string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO assets(id, file_path, status) VALUES(?, ?, 'indexed')`, id, path)
	require.NoError(t, err)
}

func insertSharpnessTestFace(t *testing.T, db *sql.DB, faceID, assetID, bboxJSON string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO face_detections(id, asset_id, bbox, embedding) VALUES(?,?,?,X'00000000')`,
		faceID, assetID, bboxJSON)
	require.NoError(t, err)
}

// TestBackfillSharpness_FillsBothAndOrdersBySharpness covers the full
// contract: two NULL-sharpness faces pointing at real temp JPEGs (one sharp,
// one heavily blurred) both end up with a non-NULL score in [0,1) that
// preserves the expected ordering, frontality stays untouched, and the
// marker file is written.
func TestBackfillSharpness_FillsBothAndOrdersBySharpness(t *testing.T) {
	db := makeTestDB(t)
	srcDir := t.TempDir()
	markerDir := t.TempDir()

	sharpPath := filepath.Join(srcDir, "sharp.jpg")
	blurredPath := filepath.Join(srcDir, "blurred.jpg")
	writeJPEG(t, sharpPath, makeNoiseImage(120, 120))
	writeJPEG(t, blurredPath, imaging.Blur(makeNoiseImage(120, 120), 8))

	insertSharpnessTestAsset(t, db, "a-sharp", sharpPath)
	insertSharpnessTestAsset(t, db, "a-blur", blurredPath)
	insertSharpnessTestFace(t, db, "f-sharp", "a-sharp", `{"x1":0,"y1":0,"x2":100,"y2":100}`)
	insertSharpnessTestFace(t, db, "f-blur", "a-blur", `{"x1":0,"y1":0,"x2":100,"y2":100}`)

	s := NewFaceService(db)
	s.SetThumbDir(t.TempDir())

	require.NoError(t, s.BackfillSharpness(context.Background(), markerDir))

	var sharp, blur, frontSharp sql.NullFloat64
	require.NoError(t, db.QueryRow(`SELECT sharpness FROM face_detections WHERE id='f-sharp'`).Scan(&sharp))
	require.NoError(t, db.QueryRow(`SELECT sharpness FROM face_detections WHERE id='f-blur'`).Scan(&blur))
	require.NoError(t, db.QueryRow(`SELECT frontality FROM face_detections WHERE id='f-sharp'`).Scan(&frontSharp))

	require.True(t, sharp.Valid, "sharp face must get a non-NULL score")
	require.True(t, blur.Valid, "blurred face must get a non-NULL score")
	require.GreaterOrEqual(t, sharp.Float64, 0.0)
	require.Less(t, sharp.Float64, 1.0)
	require.GreaterOrEqual(t, blur.Float64, 0.0)
	require.Less(t, blur.Float64, 1.0)
	require.Greater(t, sharp.Float64, blur.Float64, "the sharp source must score higher than the heavily blurred one")
	require.False(t, frontSharp.Valid, "frontality is never backfilled here — must stay NULL")

	markerPath := filepath.Join(markerDir, sharpnessBackfillMarkerFile)
	require.FileExists(t, markerPath, "marker file must be written on completion")

	// Second call is a no-op: reset sharpness back to NULL and rerun — if the
	// marker guard is honored, it must stay NULL rather than being recomputed.
	_, err := db.Exec(`UPDATE face_detections SET sharpness=NULL WHERE id='f-sharp'`)
	require.NoError(t, err)
	require.NoError(t, s.BackfillSharpness(context.Background(), markerDir))
	var afterRerun sql.NullFloat64
	require.NoError(t, db.QueryRow(`SELECT sharpness FROM face_detections WHERE id='f-sharp'`).Scan(&afterRerun))
	require.False(t, afterRerun.Valid, "a second call must be a no-op guarded by the marker file")
}

// TestBackfillSharpness_AlreadyScoredRowUntouched ensures the WHERE
// sharpness IS NULL filter really excludes rows that already have a score
// (e.g. written by nimoos-photos-ml-server for post-upgrade faces).
func TestBackfillSharpness_AlreadyScoredRowUntouched(t *testing.T) {
	db := makeTestDB(t)
	srcDir := t.TempDir()
	markerDir := t.TempDir()

	path := filepath.Join(srcDir, "a.jpg")
	writeJPEG(t, path, makeNoiseImage(120, 120))
	insertSharpnessTestAsset(t, db, "a1", path)
	insertSharpnessTestFace(t, db, "f1", "a1", `{"x1":0,"y1":0,"x2":100,"y2":100}`)
	_, err := db.Exec(`UPDATE face_detections SET sharpness=0.42 WHERE id='f1'`)
	require.NoError(t, err)

	s := NewFaceService(db)
	s.SetThumbDir(t.TempDir())
	require.NoError(t, s.BackfillSharpness(context.Background(), markerDir))

	var v sql.NullFloat64
	require.NoError(t, db.QueryRow(`SELECT sharpness FROM face_detections WHERE id='f1'`).Scan(&v))
	require.True(t, v.Valid)
	require.InDelta(t, 0.42, v.Float64, 1e-9, "a row that already has a score must not be recomputed")
}

// TestBackfillSharpness_UnreadableSourceSkippedPermanently: a missing source
// file must be skipped (left NULL) without erroring out the whole run, and
// the marker must still be written so it's never retried.
func TestBackfillSharpness_UnreadableSourceSkippedPermanently(t *testing.T) {
	db := makeTestDB(t)
	markerDir := t.TempDir()

	insertSharpnessTestAsset(t, db, "a-missing", "/nonexistent/does-not-exist.jpg")
	insertSharpnessTestFace(t, db, "f-missing", "a-missing", `{"x1":0,"y1":0,"x2":10,"y2":10}`)

	s := NewFaceService(db)
	s.SetThumbDir(t.TempDir())
	require.NoError(t, s.BackfillSharpness(context.Background(), markerDir))

	var v sql.NullFloat64
	require.NoError(t, db.QueryRow(`SELECT sharpness FROM face_detections WHERE id='f-missing'`).Scan(&v))
	require.False(t, v.Valid, "an unreadable source must stay NULL rather than erroring the whole batch")
	require.FileExists(t, filepath.Join(markerDir, sharpnessBackfillMarkerFile),
		"the marker must still be written so an unreadable file is never retried on a later start")
}

// TestBackfillSharpness_VideoAssetUsesThumbKeyframe verifies the source
// selection mirrors detectFaceScanTarget: a video asset's face is scored
// from thumbs/<id>/large.jpg, not from its (bogus, e.g. unmounted) file_path.
func TestBackfillSharpness_VideoAssetUsesThumbKeyframe(t *testing.T) {
	db := makeTestDB(t)
	thumbDir := t.TempDir()
	markerDir := t.TempDir()
	const assetID = "vid1"

	_, err := db.Exec(`INSERT INTO assets(id, file_path, mime_type, status) VALUES(?, '/nonexistent/video.mp4', 'video/mp4', 'indexed')`, assetID)
	require.NoError(t, err)
	writeJPEG(t, filepath.Join(thumbDir, assetID, "large.jpg"), makeNoiseImage(120, 120))
	insertSharpnessTestFace(t, db, "fv", assetID, `{"x1":0,"y1":0,"x2":100,"y2":100}`)

	s := NewFaceService(db)
	s.SetThumbDir(thumbDir)
	require.NoError(t, s.BackfillSharpness(context.Background(), markerDir))

	var v sql.NullFloat64
	require.NoError(t, db.QueryRow(`SELECT sharpness FROM face_detections WHERE id='fv'`).Scan(&v))
	require.True(t, v.Valid, "video asset must be scored from the thumb keyframe, not the bogus file_path")
}
