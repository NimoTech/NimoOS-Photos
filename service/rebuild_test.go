package service

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/NimoTech/NimoOS-Photos/common"
	"github.com/NimoTech/NimoOS-Photos/pkg/config"
	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	"github.com/stretchr/testify/require"
)

// seedFaceAndClip writes a face_detections row and a CLIP vector for an
// existing asset, for verifying whether rebuild preserves old ML data for
// an asset whose "source file is unreadable".
func seedFaceAndClip(t *testing.T, db *sql.DB, assetID string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO face_detections(id, asset_id, bbox, embedding) VALUES(?,?,?,?)`,
		"face-"+assetID, assetID, "[0,0,1,1]", sqlite.SerializeFloat32(make([]float32, common.FaceDim)))
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO asset_clip_idx(asset_id) VALUES(?)`, assetID)
	require.NoError(t, err)
	var rowid int64
	require.NoError(t, db.QueryRow(`SELECT rowid FROM asset_clip_idx WHERE asset_id=?`, assetID).Scan(&rowid))
	_, err = db.Exec(`INSERT INTO clip_embeddings(rowid, embedding) VALUES(?,?)`, rowid, sqlite.SerializeFloat32(make([]float32, common.CLIPDim)))
	require.NoError(t, err)
}

// TestRebuildKeepsExistingMLDataForUnreadableSource verifies: when an
// asset's source file is unreadable during rebuild (e.g. it's on a
// removable drive that's been unplugged), rebuild must never delete its
// existing face_detections / CLIP vector — processFileInternal fails and
// returns right at its first os.ReadFile, and if the old data had already
// been deleted beforehand, it would be permanently and silently lost with
// no way to recover. A readable asset should still be reprocessed as usual.
func TestRebuildKeepsExistingMLDataForUnreadableSource(t *testing.T) {
	prev := config.Cfg
	t.Cleanup(func() { config.Cfg = prev })
	config.Cfg = &config.Config{FacesEnabled: true, ScenesEnabled: true, OCREnabled: true}

	db := makeTestDB(t)
	dir := t.TempDir()
	okPath := makeTestJPEG(t, dir)
	missingPath := filepath.Join(dir, "gone-missing.jpg") // never created: simulates an unreadable source

	const okID = "asset-ok"
	const missingID = "asset-missing"
	_, err := db.Exec(`INSERT INTO assets(id,file_path,status,is_live_photo_video) VALUES(?,?,'indexed',0)`, okID, okPath)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO assets(id,file_path,status,is_live_photo_video) VALUES(?,?,'indexed',0)`, missingID, missingPath)
	require.NoError(t, err)

	seedFaceAndClip(t, db, okID)
	seedFaceAndClip(t, db, missingID)

	var missingRowidBefore int64
	require.NoError(t, db.QueryRow(`SELECT rowid FROM asset_clip_idx WHERE asset_id=?`, missingID).Scan(&missingRowidBefore))

	ml := &recordingML{}
	ix := NewIndexer(db, ml, t.TempDir(), 1)
	faces := NewFaceService(db)
	reg := NewTaskRegistry(nil)
	rb := NewRebuilder(context.Background(), db, ix, faces, reg, 2)

	taskID, err := rb.Start()
	require.NoError(t, err)
	require.Eventually(t, func() bool { return !rb.running.Load() }, 10*time.Second, 50*time.Millisecond)

	// Unreadable asset: the old face_detections row must be kept as-is.
	var faceCount int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM face_detections WHERE asset_id=?`, missingID).Scan(&faceCount))
	require.Equal(t, 1, faceCount, "the old face row must be kept when the source file is unreadable")

	// Unreadable asset: the old CLIP vector (same rowid) must be kept as-is.
	var missingRowidAfter int64
	require.NoError(t, db.QueryRow(`SELECT rowid FROM asset_clip_idx WHERE asset_id=?`, missingID).Scan(&missingRowidAfter))
	require.Equal(t, missingRowidBefore, missingRowidAfter)
	var vecCount int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM clip_embeddings WHERE rowid=?`, missingRowidAfter).Scan(&vecCount))
	require.Equal(t, 1, vecCount, "the old CLIP vector must be kept when the source file is unreadable")

	// Readable asset: reprocessed normally (mockML recorded a CLIP call; after the rerun it has 0 faces, the old row has been replaced).
	require.Greater(t, ml.clipCalls, 0)
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM face_detections WHERE asset_id=?`, okID).Scan(&faceCount))
	require.Equal(t, 0, faceCount, "a readable asset should be reprocessed; its old face row is cleared and replaced by mockML's result (0 faces)")

	// The task label counts 1 failure.
	var label string
	for _, task := range reg.List() {
		if task.ID == taskID {
			label = task.Label
		}
	}
	require.Contains(t, label, "1 failed")
}

// TestRebuildPrunesOrphanClipVectors verifies: now that the full-DB wipe is
// gone, orphaned CLIP vectors (asset_clip_idx / clip_embeddings rows whose
// asset no longer exists) are still cleaned up by rebuild's finalize()
// stage via pruneOrphanClipVectors.
func TestRebuildPrunesOrphanClipVectors(t *testing.T) {
	db := makeTestDB(t)
	// asset_clip_idx.asset_id has an FK (ON DELETE CASCADE); on the normal
	// path, orphans can only arise from a "historical delete that bypassed
	// the cascade" (see clipvec_internal_test.go); here we likewise
	// temporarily disable the FK constraint to simulate that leftover
	// orphan state, rather than genuinely breaking the cascade itself.
	const rowid = 888888
	_, err := db.Exec(`PRAGMA foreign_keys=OFF`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO asset_clip_idx(rowid, asset_id) VALUES(?,?)`, rowid, "ghost")
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO clip_embeddings(rowid, embedding) VALUES(?,?)`, rowid, sqlite.SerializeFloat32(make([]float32, common.CLIPDim)))
	require.NoError(t, err)
	_, err = db.Exec(`PRAGMA foreign_keys=ON`)
	require.NoError(t, err)

	rb := NewRebuilder(context.Background(), db, NewIndexer(db, &mockML{}, t.TempDir(), 1), NewFaceService(db), NewTaskRegistry(nil), 1)
	_, err = rb.Start()
	require.NoError(t, err)
	require.Eventually(t, func() bool { return !rb.running.Load() }, 5*time.Second, 20*time.Millisecond)

	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM asset_clip_idx WHERE asset_id=?`, "ghost").Scan(&n))
	require.Equal(t, 0, n, "orphaned asset_clip_idx rows must be cleaned up")
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM clip_embeddings WHERE rowid=?`, rowid).Scan(&n))
	require.Equal(t, 0, n, "orphaned CLIP vectors must be cleaned up")
}

// TestRebuildReprocessesAssetsWithoutDuplicatingFaces verifies rebuild
// recomputes ML without doubling face_detections, and writes the
// photos_meta timestamp.
func TestRebuildReprocessesAssetsWithoutDuplicatingFaces(t *testing.T) {
	prev := config.Cfg
	t.Cleanup(func() { config.Cfg = prev })
	config.Cfg = &config.Config{FacesEnabled: true, ScenesEnabled: true, OCREnabled: true}

	db := makeTestDB(t)
	ml := &recordingML{}
	ix := NewIndexer(db, ml, t.TempDir(), 1)
	path := makeTestJPEG(t, t.TempDir())
	require.True(t, ix.processFileInternal(path, processOpts{}))

	clipBefore := ml.clipCalls
	faces := NewFaceService(db)
	reg := NewTaskRegistry(nil)
	rb := NewRebuilder(context.Background(), db, ix, faces, reg, 2)

	taskID, err := rb.Start()
	require.NoError(t, err)
	require.NotEmpty(t, taskID)

	// Wait for the background task to finish (running flag reset).
	require.Eventually(t, func() bool { return !rb.running.Load() }, 10*time.Second, 50*time.Millisecond)

	require.Greater(t, ml.clipCalls, clipBefore) // did actually recompute

	var faceRows int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM face_detections`).Scan(&faceRows))
	require.LessOrEqual(t, faceRows, 1) // mockML returns 0 faces → no leftover old rows after rebuild

	var lastBuilt string
	require.NoError(t, db.QueryRow(`SELECT value FROM photos_meta WHERE key='index_last_rebuilt'`).Scan(&lastBuilt))
	require.NotEmpty(t, lastBuilt)
}

// TestRebuildRedetectsFacesViaRunPipeline verifies: now that face detection
// has been removed from processFileInternal, rebuild's finalize() must call
// RunPipeline (rather than RunClustering, which only regroups) to bring
// back the old faces deleted in the worker loop — the worker loop already
// reset face_scanned to 0 for those assets when it deleted their
// face_detections, so if finalize still called RunClustering, that batch of
// faces would be permanently empty and never detected again.
func TestRebuildRedetectsFacesViaRunPipeline(t *testing.T) {
	prev := config.Cfg
	t.Cleanup(func() { config.Cfg = prev })
	config.Cfg = &config.Config{FacesEnabled: true, ScenesEnabled: true, OCREnabled: true}

	db := makeTestDB(t)
	thumbDir := t.TempDir()
	path := makeTestJPEG(t, t.TempDir())

	ml := &oneFaceML{}
	ix := NewIndexer(db, ml, thumbDir, 1)
	require.True(t, ix.processFileInternal(path, processOpts{}))

	var assetID string
	require.NoError(t, db.QueryRow(`SELECT id FROM assets WHERE file_path=?`, path).Scan(&assetID))

	// Face detection has been removed from the indexing pipeline: there
	// should be no face_detections after processFileInternal.
	var faceRows int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM face_detections WHERE asset_id=?`, assetID).Scan(&faceRows))
	require.Zero(t, faceRows, "face detection has been removed from the indexing pipeline, processFileInternal must not write face_detections")

	// Manually simulate "RunPipeline has already run once": write one old face row + face_scanned=1.
	_, err := db.Exec(`INSERT INTO face_detections(id, asset_id, bbox, embedding) VALUES(?,?,?,?)`,
		"old-face", assetID, "[0,0,1,1]", sqlite.SerializeFloat32(make([]float32, common.FaceDim)))
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE assets SET face_scanned=1 WHERE id=?`, assetID)
	require.NoError(t, err)

	faces := NewFaceService(db)
	faces.SetML(ml)
	faces.SetThumbDir(thumbDir)
	reg := NewTaskRegistry(nil)
	rb := NewRebuilder(context.Background(), db, ix, faces, reg, 1)

	_, err = rb.Start()
	require.NoError(t, err)
	require.Eventually(t, func() bool { return !rb.running.Load() }, 10*time.Second, 50*time.Millisecond)

	// finalize()'s faces.RunPipeline runs synchronously within the same
	// goroutine as run(), so it has already finished by the time running is reset.
	var fs int
	require.NoError(t, db.QueryRow(`SELECT face_scanned FROM assets WHERE id=?`, assetID).Scan(&fs))
	require.Equal(t, 1, fs, "re-detection should be complete after rebuild, face_scanned should be back to 1")

	var newFaceRows int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM face_detections WHERE asset_id=?`, assetID).Scan(&newFaceRows))
	require.Equal(t, 1, newFaceRows, "once the old face is deleted, RunPipeline should re-detect 1 new face, not leave it permanently empty")
}

// TestRebuildClearsAestheticScore verifies: on a rebuild across model
// generations, the old aesthetic score (computed from the old model's
// vector) must be cleared to NULL — the recordingML used in this test has
// no aestheticHead injected, so writeClipEmbedding's inline scoring is
// skipped and won't write the score back, letting this assert the
// "clearing" step in isolation, decoupled from the inline-scoring-refill path.
func TestRebuildClearsAestheticScore(t *testing.T) {
	prev := config.Cfg
	t.Cleanup(func() { config.Cfg = prev })
	config.Cfg = &config.Config{FacesEnabled: true, ScenesEnabled: true, OCREnabled: true}

	db := makeTestDB(t)
	ml := &recordingML{}
	ix := NewIndexer(db, ml, t.TempDir(), 1)
	path := makeTestJPEG(t, t.TempDir())
	require.True(t, ix.processFileInternal(path, processOpts{}))

	var assetID string
	require.NoError(t, db.QueryRow(`SELECT id FROM assets WHERE file_path=?`, path).Scan(&assetID))
	// Manually write an old aesthetic score, simulating the state "already scored before the model change".
	_, err := db.Exec(`UPDATE assets SET aesthetic_score=0.75 WHERE id=?`, assetID)
	require.NoError(t, err)

	faces := NewFaceService(db)
	reg := NewTaskRegistry(nil)
	rb := NewRebuilder(context.Background(), db, ix, faces, reg, 2)

	_, err = rb.Start()
	require.NoError(t, err)
	require.Eventually(t, func() bool { return !rb.running.Load() }, 10*time.Second, 50*time.Millisecond)

	var score sql.NullFloat64
	require.NoError(t, db.QueryRow(`SELECT aesthetic_score FROM assets WHERE id=?`, assetID).Scan(&score))
	require.False(t, score.Valid, "the old aesthetic score should be cleared to NULL after a cross-generation rebuild, automatically refilled by inline scoring (if enabled)")
}

// TestModelGenStale verifies modelGenStale's judgment across the three
// cases: generation key missing / matching / stale.
func TestModelGenStale(t *testing.T) {
	db := makeTestDB(t)
	if !modelGenStale(db) {
		t.Fatal("fresh db should be stale (no ml_model_gen key)")
	}
	if _, err := db.Exec(`INSERT INTO photos_meta(key,value) VALUES('ml_model_gen',?)`, common.MLModelGen); err != nil {
		t.Fatal(err)
	}
	if modelGenStale(db) {
		t.Fatal("db with current gen should not be stale")
	}
	if _, err := db.Exec(`UPDATE photos_meta SET value='1' WHERE key='ml_model_gen'`); err != nil {
		t.Fatal(err)
	}
	if !modelGenStale(db) {
		t.Fatal("db with old gen should be stale")
	}
}

// TestRebuildRejectsConcurrentRuns verifies a duplicate trigger returns ErrRebuildRunning.
func TestRebuildRejectsConcurrentRuns(t *testing.T) {
	db := makeTestDB(t)
	rb := NewRebuilder(context.Background(), db, NewIndexer(db, &mockML{}, t.TempDir(), 1), NewFaceService(db), NewTaskRegistry(nil), 1)
	rb.running.Store(true) // simulate already running
	_, err := rb.Start()
	require.ErrorIs(t, err, ErrRebuildRunning)
}

// TestRebuildEmptyLibraryFinishesImmediately: rebuilding an empty library completes immediately and writes meta.
func TestRebuildEmptyLibraryFinishesImmediately(t *testing.T) {
	db := makeTestDB(t)
	rb := NewRebuilder(context.Background(), db, NewIndexer(db, &mockML{}, t.TempDir(), 1), NewFaceService(db), NewTaskRegistry(nil), 1)
	_, err := rb.Start()
	require.NoError(t, err)
	require.Eventually(t, func() bool { return !rb.running.Load() }, 5*time.Second, 20*time.Millisecond)
	var lastBuilt string
	require.NoError(t, db.QueryRow(`SELECT value FROM photos_meta WHERE key='index_last_rebuilt'`).Scan(&lastBuilt))
	require.NotEmpty(t, lastBuilt)
}

// TestRebuildExcludesOfflineAssets verifies: when an asset's removable
// drive has been unplugged (offline=1), rebuild's target query must skip
// it — its source file can't be read, and processing it would just count
// a wasted failure; MountGuard proactively triggers a Backfill/BackfillOCR
// on replug to fill the gap left in the meantime.
func TestRebuildExcludesOfflineAssets(t *testing.T) {
	db := makeTestDB(t)
	onlineID := insertAsset(t, db, "/DATA/Gallery/online.jpg", "indexed")
	offlineID := insertAsset(t, db, "/media/X/offline.jpg", "indexed")
	_, err := db.Exec(`UPDATE assets SET offline=1 WHERE id=?`, offlineID)
	require.NoError(t, err)

	rb := NewRebuilder(context.Background(), db, NewIndexer(db, &mockML{}, t.TempDir(), 1), NewFaceService(db), NewTaskRegistry(nil), 1)
	taskID, err := rb.Start()
	require.NoError(t, err)
	require.Eventually(t, func() bool { return !rb.running.Load() }, 5*time.Second, 20*time.Millisecond)

	var total int64 = -1
	for _, task := range rb.reg.List() {
		if task.ID == taskID {
			total = task.Total
		}
	}
	require.Equal(t, int64(1), total, "offline assets should not count toward rebuild targets")
	_ = onlineID
}

// TestShouldStampModelGen verifies: ml_model_gen must not be stamped when
// not a single file was successfully rerun with the new model this pass
// (total>0 and everything failed), otherwise modelGenStale would never
// trigger again and MaybeAutoRebuild would lose its chance to auto-retry
// (typical scenario: a model upgrade coinciding with every removable drive
// being offline).
func TestShouldStampModelGen(t *testing.T) {
	require.True(t, shouldStampModelGen(10, 0))   // all succeeded
	require.True(t, shouldStampModelGen(10, 3))   // partial failure: still stamped
	require.True(t, shouldStampModelGen(0, 0))    // empty DB: stamping is legitimate
	require.False(t, shouldStampModelGen(10, 10)) // all failed: not stamped
}
