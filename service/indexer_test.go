package service

import (
	"context"
	"database/sql"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NimoTech/NimoOS-Photos/common"
	"github.com/NimoTech/NimoOS-Photos/pkg/config"
	"github.com/NimoTech/NimoOS-Photos/pkg/mlclient"
	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	"github.com/stretchr/testify/require"
)

// mockML implements MLProvider — all methods return zero vectors / nil errors.
type mockML struct{}

func (m *mockML) CLIPImageEmbed(_ []byte) ([]float32, error) {
	return make([]float32, common.CLIPDim), nil
}
func (m *mockML) CLIPTextEmbed(_ string) ([]float32, error) {
	return make([]float32, common.CLIPDim), nil
}
func (m *mockML) DetectAndRecognizeFaces(_ []byte) ([]mlclient.FaceResult, error) {
	return nil, nil
}
func (m *mockML) OCR(_ []byte) ([]mlclient.OCRLine, error) {
	return []mlclient.OCRLine{}, nil
}
func (m *mockML) IsReady() bool { return true }

// makeTestDB opens a fresh in-memory SQLite database (schema migrated).
func makeTestDB(t *testing.T) *sql.DB {
	t.Helper()
	tmp := filepath.Join(t.TempDir(), "test.db")
	db, err := sqlite.Open(tmp)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	return db
}

// makeTestJPEG writes a minimal valid JPEG to dir and returns its path.
func makeTestJPEG(t *testing.T, dir string) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 100, 100))
	for y := 0; y < 100; y++ {
		for x := 0; x < 100; x++ {
			img.Set(x, y, color.RGBA{R: 100, G: 150, B: 200, A: 255})
		}
	}
	path := filepath.Join(dir, "test.jpg")
	f, err := os.Create(path)
	require.NoError(t, err)
	defer f.Close()
	require.NoError(t, jpeg.Encode(f, img, nil))
	return path
}

// oneFaceML always reports detecting exactly one face — used to distinguish
// "ML wasn't called at all" from "ML was called but the result wasn't
// persisted"; better proof that face detection has actually been removed
// from the indexing pipeline (rather than coincidentally never detecting a
// face) than the 0-face mockML/recordingML.
type oneFaceML struct{ mockML }

func (m *oneFaceML) DetectAndRecognizeFaces(_ []byte) ([]mlclient.FaceResult, error) {
	vec := make([]float32, common.FaceDim)
	vec[0] = 1
	return []mlclient.FaceResult{{BBox: mlclient.BoundingBox{X1: 0, Y1: 0, X2: 1, Y2: 1}, Embedding: vec}}, nil
}

// writeJPEGAt writes a solid-color JPEG to the given path, with the color
// determined by seed, to construct a "same file_path, different content
// (different checksum)" scenario.
func writeJPEGAt(t *testing.T, path string, seed int) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 40, 40))
	c := color.RGBA{R: uint8(seed * 37 % 256), G: uint8(seed * 53 % 256), B: uint8(seed * 97 % 256), A: 255}
	for y := 0; y < 40; y++ {
		for x := 0; x < 40; x++ {
			img.Set(x, y, c)
		}
	}
	f, err := os.Create(path)
	require.NoError(t, err)
	defer f.Close()
	require.NoError(t, jpeg.Encode(f, img, nil))
}

// TestIndexerProcessFile_DoesNotDetectFaces asserts that face detection has
// been removed from the indexing pipeline: face_detections is empty and
// face_scanned=0 after processFileInternal, even though ML would return a
// real face result, it's never called/written — detection is handed off to
// the independent FaceService.RunPipeline (0→95% real progress + 95→100%
// clustering tail).
func TestIndexerProcessFile_DoesNotDetectFaces(t *testing.T) {
	prev := config.Cfg
	t.Cleanup(func() { config.Cfg = prev })
	config.Cfg = &config.Config{FacesEnabled: true, ScenesEnabled: true, OCREnabled: true}

	db := makeTestDB(t)
	ml := &oneFaceML{}
	ix := NewIndexer(db, ml, t.TempDir(), 1)
	path := makeTestJPEG(t, t.TempDir())

	require.True(t, ix.processFileInternal(path, processOpts{}))

	var assetID string
	require.NoError(t, db.QueryRow(`SELECT id FROM assets WHERE file_path=?`, path).Scan(&assetID))

	var faceCount, faceScanned int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM face_detections WHERE asset_id=?`, assetID).Scan(&faceCount))
	require.NoError(t, db.QueryRow(`SELECT face_scanned FROM assets WHERE id=?`, assetID).Scan(&faceScanned))
	require.Zero(t, faceCount, "face detection has been removed from the indexing pipeline, face_detections must not be written")
	require.Zero(t, faceScanned, "face_scanned should stay 0, waiting for RunPipeline to process it")
}

// TestReprocess_ContentChange_ResetsFaceScanned asserts: when a file_path's
// content actually changes (checksum differs), reprocessing resets
// face_scanned back to 0, handing it off to RunPipeline for re-detection —
// covers the "edited/replaced the original image but the path is unchanged"
// scenario.
func TestReprocess_ContentChange_ResetsFaceScanned(t *testing.T) {
	db := makeTestDB(t)
	ix := NewIndexer(db, &mockML{}, t.TempDir(), 1)
	dir := t.TempDir()
	path := filepath.Join(dir, "a.jpg")
	writeJPEGAt(t, path, 1)

	require.True(t, ix.processFileInternal(path, processOpts{}))
	var assetID string
	require.NoError(t, db.QueryRow(`SELECT id FROM assets WHERE file_path=?`, path).Scan(&assetID))
	_, err := db.Exec(`UPDATE assets SET face_scanned=1 WHERE id=?`, assetID)
	require.NoError(t, err)

	// Content actually changed: write a different image to the same path
	// (checksum is necessarily different).
	writeJPEGAt(t, path, 2)
	require.True(t, ix.processFileInternal(path, processOpts{}))

	var fs int
	require.NoError(t, db.QueryRow(`SELECT face_scanned FROM assets WHERE id=?`, assetID).Scan(&fs))
	require.Equal(t, 0, fs, "a content change (different checksum) should reset face_scanned to 0")
}

// TestForceReprocess_UnchangedContent_PreservesFaceScanned asserts: when a
// force rerun happens but the file content (checksum) hasn't changed (e.g.
// Embedder/Rebuilder's pure CLIP backfill), face_scanned must not be
// cleared — otherwise every CLIP backfill pass would throw the same batch
// of assets back into the face-detection queue, producing duplicate rows in
// face_detections.
func TestForceReprocess_UnchangedContent_PreservesFaceScanned(t *testing.T) {
	db := makeTestDB(t)
	ix := NewIndexer(db, &mockML{}, t.TempDir(), 1)
	path := makeTestJPEG(t, t.TempDir())

	require.True(t, ix.processFileInternal(path, processOpts{}))
	var assetID string
	require.NoError(t, db.QueryRow(`SELECT id FROM assets WHERE file_path=?`, path).Scan(&assetID))
	_, err := db.Exec(`UPDATE assets SET face_scanned=1 WHERE id=?`, assetID)
	require.NoError(t, err)

	// Same file content, force=true just bypasses the "already indexed,
	// skip" short-circuit (matching Embedder's usage of
	// ForceReprocess(processOpts{force:true, skipExif:true, skipThumb:true})).
	ok := ix.ForceReprocess(path, processOpts{force: true, skipExif: true, skipThumb: true})
	require.True(t, ok)

	var fs int
	require.NoError(t, db.QueryRow(`SELECT face_scanned FROM assets WHERE id=?`, assetID).Scan(&fs))
	require.Equal(t, 1, fs, "a force rerun with unchanged content should not clear face_scanned")
}

// TestReprocess_ContentChange_ResetsCaptionSynced asserts: when a
// file_path's content actually changes (checksum differs), reprocessing
// resets caption_synced back to 0, handing it off to the photo-knowledge
// feed pipeline to re-hand off to Parser — covers the "edited/replaced the
// original image but the path is unchanged" scenario.
func TestReprocess_ContentChange_ResetsCaptionSynced(t *testing.T) {
	db := makeTestDB(t)
	ix := NewIndexer(db, &mockML{}, t.TempDir(), 1)
	dir := t.TempDir()
	path := filepath.Join(dir, "a.jpg")
	writeJPEGAt(t, path, 1)

	require.True(t, ix.processFileInternal(path, processOpts{}))
	var assetID string
	require.NoError(t, db.QueryRow(`SELECT id FROM assets WHERE file_path=?`, path).Scan(&assetID))
	_, err := db.Exec(`UPDATE assets SET caption_synced=1 WHERE id=?`, assetID)
	require.NoError(t, err)

	// Content actually changed: write a different image to the same path
	// (checksum is necessarily different).
	writeJPEGAt(t, path, 2)
	require.True(t, ix.processFileInternal(path, processOpts{}))

	var cs int
	require.NoError(t, db.QueryRow(`SELECT caption_synced FROM assets WHERE id=?`, assetID).Scan(&cs))
	require.Equal(t, 0, cs, "a content change (different checksum) should reset caption_synced to 0")
}

// TestForceReprocess_UnchangedContent_PreservesCaptionSynced asserts: when a
// force rerun happens but the file content (checksum) hasn't changed (e.g.
// Embedder/Rebuilder's pure CLIP backfill), caption_synced must not be
// cleared — otherwise every backfill pass would throw the same batch of
// assets already handed off to Parser back into the feed queue, producing
// duplicate feeds.
func TestForceReprocess_UnchangedContent_PreservesCaptionSynced(t *testing.T) {
	db := makeTestDB(t)
	ix := NewIndexer(db, &mockML{}, t.TempDir(), 1)
	path := makeTestJPEG(t, t.TempDir())

	require.True(t, ix.processFileInternal(path, processOpts{}))
	var assetID string
	require.NoError(t, db.QueryRow(`SELECT id FROM assets WHERE file_path=?`, path).Scan(&assetID))
	_, err := db.Exec(`UPDATE assets SET caption_synced=1 WHERE id=?`, assetID)
	require.NoError(t, err)

	// Same file content, force=true just bypasses the "already indexed,
	// skip" short-circuit (matching Embedder's usage of
	// ForceReprocess(processOpts{force:true, skipExif:true, skipThumb:true})).
	ok := ix.ForceReprocess(path, processOpts{force: true, skipExif: true, skipThumb: true})
	require.True(t, ok)

	var cs int
	require.NoError(t, db.QueryRow(`SELECT caption_synced FROM assets WHERE id=?`, assetID).Scan(&cs))
	require.Equal(t, 1, cs, "a force rerun with unchanged content should not clear caption_synced")
}

// boxedML returns OCR results with text boxes, for coverage-calculation tests.
type boxedML struct{ mockML }

func (m *boxedML) OCR(_ []byte) ([]mlclient.OCRLine, error) {
	return []mlclient.OCRLine{
		// 0.4 wide × 0.1 tall = 4% area
		{Text: "TOTAL $42.00", Score: 0.97, Box: []float64{0.1, 0.1, 0.5, 0.1, 0.5, 0.2, 0.1, 0.2}},
		// 0.2 × 0.05 = 1% area
		{Text: "Invoice #1", Score: 0.95, Box: []float64{0.1, 0.3, 0.3, 0.3, 0.3, 0.35, 0.1, 0.35}},
		// Low-confidence line: excluded from text, line count, and coverage
		{Text: "noise", Score: 0.2, Box: []float64{0, 0, 1, 0, 1, 1, 0, 1}},
	}, nil
}

// TestOcrAssetStoresCoverageAndLines verifies ocrAsset stores coverage (the
// summed area of text boxes) together with the kept line count, and that
// low-confidence lines are filtered out.
func TestOcrAssetStoresCoverageAndLines(t *testing.T) {
	db := makeTestDB(t)
	_, err := db.Exec(`INSERT INTO assets(id,file_path,status) VALUES('a1','/p/a1.jpg','indexed')`)
	require.NoError(t, err)

	ix := NewIndexer(db, &boxedML{}, t.TempDir(), 1)
	require.NoError(t, ix.ocrAsset("a1", []byte("img")))

	var text string
	var coverage float64
	var lines int
	require.NoError(t, db.QueryRow(
		`SELECT text, coverage, line_count FROM asset_ocr WHERE asset_id='a1'`,
	).Scan(&text, &coverage, &lines))
	require.Equal(t, "TOTAL $42.00\nInvoice #1", text)
	require.Equal(t, 2, lines)
	require.InDelta(t, 0.05, coverage, 1e-9) // 4% + 1%
}

// TestOcrAssetStoresLineBoxes verifies ocrAsset writes each kept line's
// text+coordinates+confidence into asset_ocr_lines one row at a time
// (low-confidence lines are not written), setting boxes_ver to 1; a rerun
// overwrites the old rows.
func TestOcrAssetStoresLineBoxes(t *testing.T) {
	db := makeTestDB(t)
	_, err := db.Exec(`INSERT INTO assets(id,file_path,status) VALUES('a1','/p/a1.jpg','indexed')`)
	require.NoError(t, err)

	ix := NewIndexer(db, &boxedML{}, t.TempDir(), 1)
	require.NoError(t, ix.ocrAsset("a1", []byte("img")))

	var ver int
	require.NoError(t, db.QueryRow(`SELECT boxes_ver FROM asset_ocr WHERE asset_id='a1'`).Scan(&ver))
	require.Equal(t, 1, ver)

	type row struct {
		text, box string
		score     float64
	}
	readLines := func() []row {
		rows, err := db.Query(`SELECT text, box, score FROM asset_ocr_lines WHERE asset_id='a1' ORDER BY line_no`)
		require.NoError(t, err)
		defer rows.Close()
		var out []row
		for rows.Next() {
			var r row
			require.NoError(t, rows.Scan(&r.text, &r.box, &r.score))
			out = append(out, r)
		}
		require.NoError(t, rows.Err())
		return out
	}

	got := readLines()
	require.Len(t, got, 2, "the low-confidence line (noise) must not be stored")
	require.Equal(t, "TOTAL $42.00", got[0].text)
	require.Equal(t, "[0.1,0.1,0.5,0.1,0.5,0.2,0.1,0.2]", got[0].box)
	require.InDelta(t, 0.97, got[0].score, 1e-9)
	require.Equal(t, "Invoice #1", got[1].text)

	// Rerun overwrite: the second run's results replace the first, no old
	// rows left behind.
	require.NoError(t, ix.ocrAsset("a1", []byte("img")))
	require.Len(t, readLines(), 2)

	// Re-OCR must reset doc_ver so the upper layer recomputes the doc
	// verdict; is_doc keeps its old value, read smoothly before the
	// recompute (see hasOcrExpr's NULL fallback — non-NULL here so the old
	// value is still seen).
	_, err = db.Exec(`UPDATE asset_ocr SET doc_ver=1, is_doc=1 WHERE asset_id='a1'`)
	require.NoError(t, err)
	require.NoError(t, ix.ocrAsset("a1", []byte("img")))
	var docVer, isDoc int
	require.NoError(t, db.QueryRow(`SELECT doc_ver, is_doc FROM asset_ocr WHERE asset_id='a1'`).Scan(&docVer, &isDoc))
	require.Equal(t, 0, docVer, "re-OCR must reset doc_ver to trigger a recompute")
	require.Equal(t, 1, isDoc, "is_doc keeps its old value, used smoothly before the recompute")
}

// TestOcrAssetNilBoxStored verifies that when ML returns no geometry
// (Box=nil), the line is still stored, with box set to '[]'.
type nilBoxML struct{ mockML }

func (m *nilBoxML) OCR(_ []byte) ([]mlclient.OCRLine, error) {
	return []mlclient.OCRLine{{Text: "no geometry", Score: 0.9, Box: nil}}, nil
}

func TestOcrAssetNilBoxStored(t *testing.T) {
	db := makeTestDB(t)
	_, err := db.Exec(`INSERT INTO assets(id,file_path,status) VALUES('a1','/p/a1.jpg','indexed')`)
	require.NoError(t, err)

	ix := NewIndexer(db, &nilBoxML{}, t.TempDir(), 1)
	require.NoError(t, ix.ocrAsset("a1", []byte("img")))

	var box string
	require.NoError(t, db.QueryRow(`SELECT box FROM asset_ocr_lines WHERE asset_id='a1' AND line_no=0`).Scan(&box))
	require.Equal(t, "[]", box)
}

// TestIndexerProcessesImage tests the full pipeline for a single image.
func TestIndexerProcessesImage(t *testing.T) {
	db := makeTestDB(t)
	thumbDir := t.TempDir()
	imgDir := t.TempDir()
	imgPath := makeTestJPEG(t, imgDir)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	idx := NewIndexer(db, &mockML{}, thumbDir, 1)
	go idx.Start(ctx)

	idx.Enqueue(imgPath)

	// Wait up to 4s for the asset to reach "indexed" status.
	var assetID string
	require.Eventually(t, func() bool {
		var status string
		err := db.QueryRow(
			`SELECT id, status FROM assets WHERE file_path=?`, imgPath,
		).Scan(&assetID, &status)
		return err == nil && status == "indexed"
	}, 4*time.Second, 100*time.Millisecond, "asset should reach 'indexed' status")

	require.NotEmpty(t, assetID, "asset ID must be populated")

	// Verify thumbnail exists.
	smallPath := filepath.Join(thumbDir, assetID, "small.jpg")
	_, err := os.Stat(smallPath)
	require.NoError(t, err, "small.jpg thumbnail must exist at %s", smallPath)
}

// TestIndexerDeduplicates enqueues the same path twice and verifies only one
// DB record is created.
func TestIndexerDeduplicates(t *testing.T) {
	db := makeTestDB(t)
	thumbDir := t.TempDir()
	imgDir := t.TempDir()
	imgPath := makeTestJPEG(t, imgDir)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	idx := NewIndexer(db, &mockML{}, thumbDir, 2)
	go idx.Start(ctx)

	// Enqueue twice — the second should be a no-op.
	idx.Enqueue(imgPath)
	idx.Enqueue(imgPath)

	// Wait for indexed.
	require.Eventually(t, func() bool {
		var status string
		err := db.QueryRow(
			`SELECT status FROM assets WHERE file_path=?`, imgPath,
		).Scan(&status)
		return err == nil && status == "indexed"
	}, 4*time.Second, 100*time.Millisecond, "asset should reach 'indexed' status")

	// Exactly one record.
	var count int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM assets WHERE file_path=?`, imgPath,
	).Scan(&count))
	require.Equal(t, 1, count, "duplicate enqueue must not create duplicate DB rows")
}

// TestIndexer_ForceReprocess_BypassesChecksumShortcut asserts ForceReprocess
// can still rerun ML even when the asset is already indexed, writing the
// missing clip_embeddings.
func TestIndexer_ForceReprocess_BypassesChecksumShortcut(t *testing.T) {
	db := makeTestDB(t)
	thumbDir := t.TempDir()
	imgDir := t.TempDir()
	imgPath := makeTestJPEG(t, imgDir)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// First pass: run indexing with a "not ready" ML mock, simulating the
	// real-world scenario of "asset marked indexed but has no vector".
	notReady := &mockMLNotReady{}
	idx := NewIndexer(db, notReady, thumbDir, 1)
	go idx.Start(ctx)
	idx.Enqueue(imgPath)

	var assetID string
	require.Eventually(t, func() bool {
		return db.QueryRow(`SELECT id FROM assets WHERE file_path=? AND status='indexed'`, imgPath).Scan(&assetID) == nil
	}, 4*time.Second, 50*time.Millisecond)

	var hasIdx int
	_ = db.QueryRow(`SELECT COUNT(*) FROM asset_clip_idx WHERE asset_id=?`, assetID).Scan(&hasIdx)
	require.Equal(t, 0, hasIdx, "precondition: asset should have no clip embedding")

	// Second pass: swap in a ready ML mock and call ForceReprocess.
	cancel() // shut down the old worker
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	ready := &mockML{}
	idx2 := NewIndexer(db, ready, thumbDir, 1)
	go idx2.Start(ctx2)

	ok := idx2.ForceReprocess(imgPath, processOpts{force: true, skipExif: true, skipThumb: true})
	require.True(t, ok, "ForceReprocess should return true")

	require.Eventually(t, func() bool {
		var n int
		_ = db.QueryRow(`SELECT COUNT(*) FROM asset_clip_idx WHERE asset_id=?`, assetID).Scan(&n)
		return n == 1
	}, 2*time.Second, 50*time.Millisecond)
}

// mockMLNotReady always returns IsReady=false, forcing processFile to skip the ML stage.
type mockMLNotReady struct{ mockML }

func (m *mockMLNotReady) IsReady() bool { return false }

// TestIndexer_ForceReprocess_SkipExif asserts asset_exif row is left untouched when skipExif=true.
func TestIndexer_ForceReprocess_SkipExif(t *testing.T) {
	db := makeTestDB(t)
	thumbDir := t.TempDir()
	imgDir := t.TempDir()
	imgPath := makeTestJPEG(t, imgDir)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	idx := NewIndexer(db, &mockML{}, thumbDir, 1)
	go idx.Start(ctx)
	idx.Enqueue(imgPath)

	var assetID string
	require.Eventually(t, func() bool {
		return db.QueryRow(`SELECT id FROM assets WHERE file_path=? AND status='indexed'`, imgPath).Scan(&assetID) == nil
	}, 4*time.Second, 50*time.Millisecond)

	// Deliberately pollute asset_exif: write an obviously wrong value
	_, err := db.Exec(`UPDATE asset_exif SET width=99999 WHERE asset_id=?`, assetID)
	require.NoError(t, err)

	ok := idx.ForceReprocess(imgPath, processOpts{force: true, skipExif: true, skipThumb: true})
	require.True(t, ok)

	// The pollution should survive when skipExif takes effect
	var w int
	require.NoError(t, db.QueryRow(`SELECT width FROM asset_exif WHERE asset_id=?`, assetID).Scan(&w))
	require.Equal(t, 99999, w, "the original asset_exif should be kept when skipExif=true")
}

// TestIndexer_ForceReprocess_SkipThumb asserts thumbnails aren't regenerated when skipThumb=true.
func TestIndexer_ForceReprocess_SkipThumb(t *testing.T) {
	db := makeTestDB(t)
	thumbDir := t.TempDir()
	imgDir := t.TempDir()
	imgPath := makeTestJPEG(t, imgDir)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	idx := NewIndexer(db, &mockML{}, thumbDir, 1)
	go idx.Start(ctx)
	idx.Enqueue(imgPath)

	var assetID string
	require.Eventually(t, func() bool {
		return db.QueryRow(`SELECT id FROM assets WHERE file_path=? AND status='indexed'`, imgPath).Scan(&assetID) == nil
	}, 4*time.Second, 50*time.Millisecond)

	// Replace a file in the thumbnail directory with a sentinel (pick any
	// existing image already in the directory)
	entries, err := os.ReadDir(filepath.Join(thumbDir, assetID))
	require.NoError(t, err)
	require.NotEmpty(t, entries, "precondition: thumbnails should already be generated")
	sentinel := filepath.Join(thumbDir, assetID, entries[0].Name())
	require.NoError(t, os.WriteFile(sentinel, []byte{0}, 0644))

	ok := idx.ForceReprocess(imgPath, processOpts{force: true, skipExif: true, skipThumb: true})
	require.True(t, ok)

	info, err := os.Stat(sentinel)
	require.NoError(t, err)
	require.Equal(t, int64(1), info.Size(), "the original thumbnail should be kept when skipThumb=true (sentinel is still 1 byte)")
}

// TestAssetExifUpsertReplacesOnConflict drives the asset_exif upsert SQL
// directly to confirm that ON CONFLICT(asset_id) DO UPDATE replaces only the
// columns listed in the DO UPDATE clause, leaving previously-written columns
// untouched. This guards the indexer's image→video sequential write path.
func TestAssetExifUpsertReplacesOnConflict(t *testing.T) {
	dir := t.TempDir()
	db, err := sqlite.Open(filepath.Join(dir, "test.db"))
	require.NoError(t, err)
	defer db.Close()

	// Seed an asset row so the FK is satisfied.
	_, err = db.Exec(`INSERT INTO assets(id, file_path, status) VALUES('a1','/tmp/a.jpg','pending')`)
	require.NoError(t, err)

	// First write (image-style: with iso).
	_, err = db.Exec(`
		INSERT INTO asset_exif(asset_id, width, height, iso, aperture, make)
		VALUES('a1', 100, 200, 800, 1.8, 'Apple')
		ON CONFLICT(asset_id) DO UPDATE SET
		  width = excluded.width,
		  height = excluded.height,
		  iso = excluded.iso,
		  aperture = excluded.aperture,
		  make = excluded.make`)
	require.NoError(t, err)

	var width, iso int
	var aperture float64
	var make string
	require.NoError(t, db.QueryRow(
		`SELECT width, iso, aperture, make FROM asset_exif WHERE asset_id='a1'`,
	).Scan(&width, &iso, &aperture, &make))
	require.Equal(t, 100, width)
	require.Equal(t, 800, iso)
	require.InDelta(t, 1.8, aperture, 1e-6)
	require.Equal(t, "Apple", make)

	// Second write (video-style: different columns; conflicts on asset_id).
	_, err = db.Exec(`
		INSERT INTO asset_exif(asset_id, width, height, video_codec, frame_rate, bit_rate, rotation)
		VALUES('a1', 1920, 1080, 'h264', 29.97, 12000000, 90)
		ON CONFLICT(asset_id) DO UPDATE SET
		  width = excluded.width,
		  height = excluded.height,
		  video_codec = excluded.video_codec,
		  frame_rate = excluded.frame_rate,
		  bit_rate = excluded.bit_rate,
		  rotation = excluded.rotation`)
	require.NoError(t, err)

	var w2, h2, br, rot int
	var codec string
	var fps float64
	require.NoError(t, db.QueryRow(
		`SELECT width, height, video_codec, frame_rate, bit_rate, rotation FROM asset_exif WHERE asset_id='a1'`,
	).Scan(&w2, &h2, &codec, &fps, &br, &rot))
	require.Equal(t, 1920, w2)
	require.Equal(t, 1080, h2)
	require.Equal(t, "h264", codec)
	require.InDelta(t, 29.97, fps, 1e-3)
	require.Equal(t, 12000000, br)
	require.Equal(t, 90, rot)

	// Image-side columns from the first write should still be there (they were
	// NOT listed in the second upsert's DO UPDATE clause).
	var oldIso int
	var oldAp float64
	var oldMake string
	require.NoError(t, db.QueryRow(
		`SELECT iso, aperture, make FROM asset_exif WHERE asset_id='a1'`,
	).Scan(&oldIso, &oldAp, &oldMake))
	require.Equal(t, 800, oldIso)
	require.InDelta(t, 1.8, oldAp, 1e-6)
	require.Equal(t, "Apple", oldMake)
}

func TestScanDirectorySkipsHiddenDirs(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.jpg"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	trashDir := filepath.Join(root, ".trash", "id1")
	if err := os.MkdirAll(trashDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(trashDir, "b.jpg"), []byte("y"), 0644); err != nil {
		t.Fatal(err)
	}

	var collected []string
	err := walkSupported(context.Background(), root, func(p string) { collected = append(collected, p) })
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range collected {
		if strings.Contains(p, ".trash") {
			t.Fatalf("scan should skip .trash, but collected %q", p)
		}
	}
	if len(collected) != 1 {
		t.Fatalf("collected %d files, want 1", len(collected))
	}
}

// TestRemoveByPathSkipsTrashedAsset is a regression test for the watcher race:
// soft-deleting moves the file, which fires an fsnotify Rename on the old path;
// the watcher calls RemoveByPath, which must NOT hard-delete a trashed asset.
func TestRemoveByPathSkipsTrashedAsset(t *testing.T) {
	db := makeTestDB(t)
	idx := NewIndexer(db, &mockML{}, t.TempDir(), 1)

	_, err := db.Exec(`INSERT INTO assets(id, file_path, status, deleted_at, original_path)
		VALUES('t1', '/DATA/Gallery/foo.jpg', 'indexed', CURRENT_TIMESTAMP, '/DATA/Gallery/foo.jpg')`)
	require.NoError(t, err)

	idx.RemoveByPath("/DATA/Gallery/foo.jpg")

	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM assets WHERE id='t1'`).Scan(&n))
	require.Equal(t, 1, n, "RemoveByPath must not delete a soft-deleted asset")
}

// TestScanDirectoryOnceDedups: concurrent rescans of the same root directory
// only run once — both the watcher's mount polling (followMounts) and
// MountGuard's replug recovery can trigger a rescan for the same mount,
// and a redundant scan wastes IO. Skips outright when the in-flight marker
// is held, and can scan again once released.
func TestScanDirectoryOnceDedups(t *testing.T) {
	db := makeTestDB(t)
	ix := NewIndexer(db, &mockML{}, t.TempDir(), 1)
	dir := t.TempDir()

	ix.scanDirInFlight.Store(dir, struct{}{}) // simulate another rescan in flight
	started, err := ix.ScanDirectoryOnce(dir)
	require.NoError(t, err)
	require.False(t, started, "must skip while in-flight")

	ix.scanDirInFlight.Delete(dir)
	started, err = ix.ScanDirectoryOnce(dir)
	require.NoError(t, err)
	require.True(t, started, "must actually run once released")
}

// recordingML records how many times each ML capability was called, for
// feature-flag-gating assertions. lastOCRData additionally records the bytes
// received by the most recent OCR call, to assert that an oversized original
// gets the degraded thumbnail rather than the original.
type recordingML struct {
	mockML
	clipCalls, faceCalls, ocrCalls int
	lastOCRData                    []byte
}

func (m *recordingML) CLIPImageEmbed(d []byte) ([]float32, error) {
	m.clipCalls++
	return m.mockML.CLIPImageEmbed(d)
}
func (m *recordingML) DetectAndRecognizeFaces(d []byte) ([]mlclient.FaceResult, error) {
	m.faceCalls++
	return m.mockML.DetectAndRecognizeFaces(d)
}
func (m *recordingML) OCR(d []byte) ([]mlclient.OCRLine, error) {
	m.ocrCalls++
	m.lastOCRData = d
	return m.mockML.OCR(d)
}

// TestIndexerHonorsFeatureFlags verifies the indexer skips the corresponding ML calls when Scenes/OCR/Faces are disabled.
func TestIndexerHonorsFeatureFlags(t *testing.T) {
	db := makeTestDB(t)
	ml := &recordingML{}
	ix := NewIndexer(db, ml, t.TempDir(), 1)
	path := makeTestJPEG(t, t.TempDir())

	prev := config.Cfg
	t.Cleanup(func() { config.Cfg = prev })

	config.Cfg = &config.Config{FacesEnabled: false, ScenesEnabled: false, OCREnabled: false}
	require.True(t, ix.processFileInternal(path, processOpts{force: true}))
	require.Zero(t, ml.clipCalls)
	require.Zero(t, ml.ocrCalls)

	config.Cfg = &config.Config{FacesEnabled: true, ScenesEnabled: true, OCREnabled: true}
	require.True(t, ix.processFileInternal(path, processOpts{force: true}))
	require.Equal(t, 1, ml.clipCalls)
	require.Equal(t, 1, ml.ocrCalls)

	// Face detection has been removed from the indexing pipeline and handed
	// off to the independent FaceService.RunPipeline (a real-progress
	// task): regardless of FacesEnabled, processFileInternal must no
	// longer call DetectAndRecognizeFaces directly.
	require.Zero(t, ml.faceCalls, "face detection has been handed off to RunPipeline, the indexer must not call ML directly")
}

// TestProcessFileInternal_OCRFallsBackToThumbnailForOversizedOriginal covers a
// real bug found in production: when the original image exceeds
// immich-ml/PIL's 178.9MP hard cap (the real case was a 16320x12240=199.8MP
// Pexels photo in the library), inline OCR must fall back to the already-
// generated large.jpg thumbnail instead of the original bytes, or the OCR
// request necessarily 500s and gets silently swallowed forever.
//
// Uses a preseeded row with a known id so processFileInternal takes the
// ON CONFLICT(file_path) UPDATE branch (leaving the existing id unchanged),
// letting us place the large.jpg thumbnail at a known path before the call —
// the real thumb.Generate would necessarily fail to decode this
// hand-constructed JPEG header (no real pixel data), so it won't overwrite
// the preseeded thumbnail.
func TestProcessFileInternal_OCRFallsBackToThumbnailForOversizedOriginal(t *testing.T) {
	db := makeTestDB(t)
	thumbDir := t.TempDir()
	imgDir := t.TempDir()
	const assetID = "asset-ocr-oversized"

	oversizedPath := filepath.Join(imgDir, "big.jpg")
	require.NoError(t, os.WriteFile(oversizedPath, fakeJPEGHeader(16320, 12240), 0o644))

	_, err := db.Exec(`INSERT INTO assets(id, file_path, status) VALUES(?, ?, 'pending')`, assetID, oversizedPath)
	require.NoError(t, err)

	require.NoError(t, os.MkdirAll(filepath.Join(thumbDir, assetID), 0o755))
	generatedThumb := makeTestJPEG(t, t.TempDir())
	thumbBytes, err := os.ReadFile(generatedThumb)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(thumbDir, assetID, "large.jpg"), thumbBytes, 0o644))

	prev := config.Cfg
	t.Cleanup(func() { config.Cfg = prev })
	config.Cfg = &config.Config{FacesEnabled: true, ScenesEnabled: true, OCREnabled: true}

	ml := &recordingML{}
	ix := NewIndexer(db, ml, thumbDir, 1)
	require.True(t, ix.processFileInternal(oversizedPath, processOpts{force: true}))

	require.Equal(t, 1, ml.ocrCalls)
	require.Equal(t, thumbBytes, ml.lastOCRData, "OCR should receive the large.jpg thumbnail bytes, not the oversized original's bytes")
}

// TestProcessFileInternal_OCRSkippedWhenOversizedAndNoThumbnail covers the
// case where even the degraded thumbnail is unavailable: the real
// thumb.Generate necessarily fails to decode this hand-built JPEG header
// with no valid pixel data, producing no large.jpg/small.jpg, so OCR must be
// skipped (following the existing swallow-the-error style) instead of
// force-feeding the oversized original to ML.
func TestProcessFileInternal_OCRSkippedWhenOversizedAndNoThumbnail(t *testing.T) {
	db := makeTestDB(t)
	thumbDir := t.TempDir()
	imgDir := t.TempDir()

	oversizedPath := filepath.Join(imgDir, "big.jpg")
	require.NoError(t, os.WriteFile(oversizedPath, fakeJPEGHeader(16320, 12240), 0o644))

	prev := config.Cfg
	t.Cleanup(func() { config.Cfg = prev })
	config.Cfg = &config.Config{FacesEnabled: true, ScenesEnabled: true, OCREnabled: true}

	ml := &recordingML{}
	ix := NewIndexer(db, ml, thumbDir, 1)
	require.True(t, ix.processFileInternal(oversizedPath, processOpts{force: true}))

	require.Zero(t, ml.ocrCalls, "should skip OCR when no thumbnail is available, rather than passing the oversized original to ML")
}

// TestResolveMimeType verifies we store the authoritative MIME type for
// supported media extensions rather than http.DetectContentType's content
// sniffing result — the latter returns application/octet-stream for
// QuickTime (.mov) and HEIC, and the misleading video/webm for Matroska
// (.mkv). The whole system (the frontend picks <video>/<img> from mime_type,
// and every "mime_type LIKE 'video/%'" query on the backend) depends on this
// field, so it must be stored correctly.
func TestResolveMimeType(t *testing.T) {
	cases := []struct {
		ext  string
		want string
	}{
		{".mov", "video/quicktime"},
		{".mkv", "video/x-matroska"},
		{".avi", "video/x-msvideo"},
		{".mp4", "video/mp4"},
		{".heic", "image/heic"},
		{".webp", "image/webp"},
		{".jpg", "image/jpeg"},
		{".jpeg", "image/jpeg"},
		{".png", "image/png"},
		{".MOV", "video/quicktime"}, // case-insensitive
		// Additional image formats
		{".gif", "image/gif"},
		{".bmp", "image/bmp"},
		{".tiff", "image/tiff"},
		{".tif", "image/tiff"},
		{".avif", "image/avif"},
		// Additional video formats (only ones browsers can play natively inline)
		{".webm", "video/webm"},
		{".m4v", "video/mp4"},
		{".3gp", "video/3gpp"},
	}
	for _, c := range cases {
		require.Equalf(t, c.want, resolveMimeType(nil, c.ext),
			"resolveMimeType(%q) should return the authoritative type", c.ext)
	}

	// Unknown extensions fall back to content sniffing.
	pngHeader := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}
	require.Equal(t, "image/png", resolveMimeType(pngHeader, ".bin"),
		"unknown extensions should fall back to http.DetectContentType")
}

// TestPruneMissingUnderSkipsWhenMountVanished: when the prune at the end of
// a recovery scan (replug → ScanDirectory) collides with "the drive gets
// unplugged again during the scan, leaving an empty leftover mount dir",
// every file under that directory stats as "doesn't exist" — pruning as
// usual would physically wipe out that entire drive's assets, both their
// rows and their vectors and thumbnails. Interlock #1: prune must be skipped
// when the directory's mount point isn't in the current mount table.
func TestPruneMissingUnderSkipsWhenMountVanished(t *testing.T) {
	db := makeTestDB(t)
	// The fixture must use a mount name that doesn't exist on the real
	// machine (see TestPruneMissingUnderKeepsOfflineAssets's note):
	// /media/devmon is 0700 on this box, so stat reports EACCES rather than
	// ENOENT, which would nullify this test case (the asset would survive
	// regardless of whether the interlock exists, making the assertion
	// vacuously true).
	id := insertAsset(t, db, "/media/nimoos-test-V/gone.jpg", "indexed") // file doesn't exist on disk
	ix := NewIndexer(db, &mockML{}, t.TempDir(), 1)
	ix.mountRoots = func() []string { return []string{"/DATA"} } // /media/nimoos-test-V has vanished from the mount table

	require.NoError(t, ix.pruneMissingUnder("/media/nimoos-test-V"))

	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM assets WHERE id=?`, id).Scan(&n))
	require.Equal(t, 1, n, "prune must be forbidden once the mount point has vanished, the asset must be left untouched")
}

// TestPruneMissingUnderKeepsOfflineAssets: interlock #2 — an offline=1
// asset's file being unreachable is exactly the state the offline flag
// itself records, not evidence of "the file was deleted"; prune must
// exclude them. An offline=0 asset whose file has genuinely vanished is
// still cleaned up as usual.
func TestPruneMissingUnderKeepsOfflineAssets(t *testing.T) {
	db := makeTestDB(t)
	// Use a real, existing empty temp directory for the mount point:
	// pruneDeleteAllowed re-verifies the mount root is still alive via
	// os.Stat before deleting (see pruneDeleteAllowed) — a made-up
	// nonexistent path would make that re-check always false and nullify
	// this test case; the actual files under the directory are still not
	// created, preserving the "file has vanished" semantics.
	mountDir := t.TempDir()
	offID := insertAsset(t, db, filepath.Join(mountDir, "offline.jpg"), "indexed")
	onID := insertAsset(t, db, filepath.Join(mountDir, "deleted.jpg"), "indexed")
	_, err := db.Exec(`UPDATE assets SET offline=1 WHERE id=?`, offID)
	require.NoError(t, err)

	ix := NewIndexer(db, &mockML{}, t.TempDir(), 1)
	ix.mountRoots = func() []string { return []string{"/DATA", mountDir} }

	require.NoError(t, ix.pruneMissingUnder(mountDir))

	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM assets WHERE id=?`, offID).Scan(&n))
	require.Equal(t, 1, n, "an offline=1 asset must be excluded from prune")
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM assets WHERE id=?`, onID).Scan(&n))
	require.Equal(t, 0, n, "an offline=0 asset whose file has genuinely vanished should be cleaned up as usual")
}

// TestStatusCountsReportsOfflineCount verifies StatusCounts().Offline
// correctly counts offline=1 assets without affecting Indexed's existing
// counting rule (an offline asset is still counted in Indexed by its
// status; Offline is a separate, additive statistic). A trashed offline
// asset (deleted_at non-null + offline=1, double-flagged) is not counted —
// it has already vanished from the library view and isn't the subject the
// "N photos are on a disconnected drive" hint is meant for.
func TestStatusCountsReportsOfflineCount(t *testing.T) {
	db := makeTestDB(t)
	insertAsset(t, db, "/DATA/a.jpg", "indexed")
	offID := insertAsset(t, db, "/media/X/b.jpg", "indexed")
	insertAsset(t, db, "/DATA/c.jpg", "pending")
	_, err := db.Exec(`UPDATE assets SET offline=1 WHERE id=?`, offID)
	require.NoError(t, err)
	// Double-flagged asset: trashed first, then the drive unplugged (or
	// vice versa) — should not count toward Offline.
	dualID := insertAsset(t, db, "/media/X/trashed.jpg", "indexed")
	_, err = db.Exec(`UPDATE assets SET offline=1, deleted_at='2026-01-01 00:00:00' WHERE id=?`, dualID)
	require.NoError(t, err)

	ix := NewIndexer(db, &mockML{}, t.TempDir(), 1)
	status := ix.StatusCounts()

	require.Equal(t, 1, status.Offline, "the offline=1 count must not include the trashed double-flagged asset")
	require.Equal(t, 3, status.Indexed, "Indexed's counting rule is unchanged: an offline asset is still counted by status")
	require.Equal(t, 1, status.Pending)
}

// TestPruneMissingUnderLikeMetacharSiblings: `_` is a LIKE wildcard, and real
// USB drive labels do contain underscores (Kingston_DataTra). Pruning
// …/disk_A must not spill over onto a sibling mount …/diskXA that differs
// only in that `_` position — the old `LIKE 'disk_A/%'` would incidentally
// match it and physically delete assets on the sibling drive that are
// merely "temporarily unreachable".
func TestPruneMissingUnderLikeMetacharSiblings(t *testing.T) {
	db := makeTestDB(t)
	// The fixture uses a mount name that doesn't exist on the real machine,
	// same reasoning as above (EACCES would nullify the assertion).
	siblingID := insertAsset(t, db, "/media/nimoos-test-diskXA/photo.jpg", "indexed") // file doesn't exist
	ix := NewIndexer(db, &mockML{}, t.TempDir(), 1)
	ix.mountRoots = func() []string {
		return []string{"/DATA", "/media/nimoos-test-disk_A", "/media/nimoos-test-diskXA"}
	}

	require.NoError(t, ix.pruneMissingUnder("/media/nimoos-test-disk_A"))

	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM assets WHERE id=?`, siblingID).Scan(&n))
	require.Equal(t, 1, n, "pruning disk_A must not delete assets on the sibling mount diskXA")
}

// TestPruneSystemMountAssetsPurgesDevmonAssets: the startup cleanup must hard-
// delete existing devmon (USB) assets along with their CLIP vectors and face
// rows, without affecting assets on other paths.
func TestPruneSystemMountAssetsPurgesDevmonAssets(t *testing.T) {
	db := makeTestDB(t)
	usb := insertAsset(t, db, "/media/devmon/stickA/photo.jpg", "indexed")
	keep := insertAsset(t, db, "/DATA/Gallery/keep.jpg", "indexed")
	seedFaceAndClip(t, db, usb)
	seedFaceAndClip(t, db, keep)

	ix := NewIndexer(db, &mockML{}, t.TempDir(), 1)
	ix.pruneSystemMountAssets()

	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM assets WHERE id=?`, usb).Scan(&n))
	require.Equal(t, 0, n, "the devmon asset row must be hard-deleted")
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM asset_clip_idx WHERE asset_id=?`, usb).Scan(&n))
	require.Equal(t, 0, n, "the devmon asset's CLIP mapping must be cleared")
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM face_detections WHERE asset_id=?`, usb).Scan(&n))
	require.Equal(t, 0, n, "the devmon asset's face rows must be cleared")
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM assets WHERE id=?`, keep).Scan(&n))
	require.Equal(t, 1, n, "non-devmon assets must not be affected")
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM asset_clip_idx WHERE asset_id=?`, keep).Scan(&n))
	require.Equal(t, 1, n)
}

// TestPruneMissingUnderDeletedSubdir: the mount root is still alive but a
// subdirectory under it was deleted wholesale (Files deletes a folder →
// busdelete calls pruneMissingUnder with the deleted directory's path) — a
// "legitimate deletion". The interlock must not mistake it for a "drive
// unplug" and skip cleanup — otherwise every asset under that directory
// lingers forever, still hit by search (real incident: 81 assets got stuck
// after album directories under /media/RAID_0 were deleted).
func TestPruneMissingUnderDeletedSubdir(t *testing.T) {
	db := makeTestDB(t)
	mountDir := t.TempDir() // the mount root always exists
	gonedir := filepath.Join(mountDir, "Miami")
	id := insertAsset(t, db, filepath.Join(gonedir, "photo.jpg"), "indexed") // the directory and the file inside it have both vanished

	ix := NewIndexer(db, &mockML{}, t.TempDir(), 1)
	ix.mountRoots = func() []string { return []string{"/DATA", mountDir} }

	require.NoError(t, ix.pruneMissingUnder(gonedir))

	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM assets WHERE id=?`, id).Scan(&n))
	require.Equal(t, 0, n, "assets under a deleted subdirectory must be cleaned up while the mount root is still alive")
}

func TestPruneDeleteGuard(t *testing.T) {
	root := t.TempDir()
	underRoot := func(string) (string, bool) { return root, true }
	require.True(t, pruneDeleteAllowed(root, underRoot))
	// Mount root alive, subdirectory deleted: a legitimate deletion, bulk delete is allowed (previously misjudged as a drive unplug before the fix)
	require.True(t, pruneDeleteAllowed(filepath.Join(root, "gone"), underRoot))
	// Directory isn't under any mounted root (the mount has vanished from /proc/mounts): bulk delete forbidden
	require.False(t, pruneDeleteAllowed(root, func(string) (string, bool) { return "", false }))
	// stat on the mount root itself fails (a dead mount lingering in the mount table): bulk delete forbidden
	deadRoot := filepath.Join(root, "dead-mount")
	require.False(t, pruneDeleteAllowed(deadRoot, func(string) (string, bool) { return deadRoot, true }))
}

// TestPruneRcloneMountAssetsPurges: historically mis-indexed assets under an
// rclone cloud-drive mount point are defensively hard-deleted at startup;
// a mount point containing an underscore (real naming is
// /mnt/yu.wu_dropbox_*) must not leak a LIKE wildcard match and delete
// assets on an adjacent path.
func TestPruneRcloneMountAssetsPurges(t *testing.T) {
	db := makeTestDB(t)
	cloud := insertAsset(t, db, "/mnt/yu.wu_dropbox_178/photo.jpg", "indexed")
	// `_` is a single-char wildcard in LIKE: if the implementation misuses LIKE, the prefix /mnt/yu.wuXdropbox... would also match
	sibling := insertAsset(t, db, "/mnt/yu.wuXdropbox_178/photo.jpg", "indexed")
	keep := insertAsset(t, db, "/DATA/Gallery/keep.jpg", "indexed")

	ix := NewIndexer(db, &mockML{}, t.TempDir(), 1)
	ix.pruneRcloneMountAssets([]string{"/mnt/yu.wu_dropbox_178"})

	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM assets WHERE id=?`, cloud).Scan(&n))
	require.Equal(t, 0, n, "assets under the rclone mount must be cleaned up")
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM assets WHERE id=?`, sibling).Scan(&n))
	require.Equal(t, 1, n, "an adjacent similar path must not be deleted by a LIKE wildcard mismatch")
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM assets WHERE id=?`, keep).Scan(&n))
	require.Equal(t, 1, n)
}

// TestPendingBackfillExcludesPreviewWhenDisabled: when PreviewPregen is off
// (includePreview=false), the pre-scan only judges backlog by sprite.jpg's
// absence — preview.mp4 missing no longer counts, since it's left to the
// route's lazy generation; with it on, both are checked.
func TestPendingBackfillExcludesPreviewWhenDisabled(t *testing.T) {
	thumbDir := t.TempDir()
	// Candidate a: sprite already exists, preview missing → doesn't count
	// as backlog when includePreview=false
	require.NoError(t, os.MkdirAll(filepath.Join(thumbDir, "a"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(thumbDir, "a", "sprite.jpg"), []byte("x"), 0644))
	// Candidate b: sprite missing → counts as backlog in both modes
	cands := []spriteCandidate{{id: "a"}, {id: "b"}}

	got := pendingBackfill(cands, thumbDir, false)
	require.Len(t, got, 1)
	require.Equal(t, "b", got[0].id, "only b should remain with includePreview=false")

	got = pendingBackfill(cands, thumbDir, true)
	require.Len(t, got, 2, "both should be pending with includePreview=true")
}
