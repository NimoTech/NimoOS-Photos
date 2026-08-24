package service_test

// Tests for Task 5 of the apple-cluster-engine plan: wiring the two-pass
// engine (SegmentMoments -> GreedyMomentClusters -> HACComplete) into
// RunClustering behind photos.ClusterEngine, alongside the legacy DBSCAN
// path. Reuses makeTestFaceDB/insertAssetFace/normalize from faces_test.go
// (same package) rather than redefining them.
//
// Existing RunClustering tests (faces_test.go, anchor_cover_test.go) are left
// untouched -- they already exercise the "apple" engine by default (config.Cfg
// nil -> clusterEngine() falls back to "apple"), and a representative subset
// of their anchoring scenarios is reproduced here as new, explicitly
// parameterized tests covering both engine values, per the plan's regression
// requirement.

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/NimoTech/NimoOS-Photos/pkg/config"
	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// setClusterEngine points config.Cfg at a fresh Config carrying only the
// engine selector; every other accessor (clusterEpsilon/momentGap/tightEps/
// mergeEps) falls back to its documented default for a zero-value field, so
// this doesn't need to duplicate those defaults.
func setClusterEngine(t *testing.T, engine string) {
	t.Helper()
	old := config.Cfg
	config.Cfg = &config.Config{ClusterEngine: engine}
	t.Cleanup(func() { config.Cfg = old })
}

// insertAssetFaceAt is insertAssetFace plus an explicit taken_at, needed to
// exercise SegmentMoments (insertAssetFace's asset always has a NULL
// taken_at/indexed_at, which collapses every face into a single moment).
func insertAssetFaceAt(t *testing.T, db *sql.DB, assetID string, vec []float32, takenAt time.Time) string {
	t.Helper()
	_, err := db.Exec(`INSERT INTO assets(id, file_path, status, taken_at) VALUES(?,?, 'indexed', ?)`,
		assetID, "/x/"+assetID+".jpg", takenAt.UTC().Format("2006-01-02 15:04:05"))
	require.NoError(t, err)
	faceID := uuid.NewString()
	_, err = db.Exec(`INSERT INTO face_detections(id, asset_id, bbox, embedding) VALUES(?,?,?,?)`,
		faceID, assetID, `{"x1":0,"y1":0,"x2":1,"y2":1}`, sqlite.SerializeFloat32(vec))
	require.NoError(t, err)
	return faceID
}

// insertAssetFaceIndexedAt is insertAssetFace plus an explicit indexed_at,
// with taken_at left NULL (omitted from the INSERT). This exercises the
// SegmentMoments fallback ("zero takenAt falls back to indexedAt") through
// the real RunClustering -> loadFacesWithProgress -> apple-engine path, as
// opposed to unit-testing SegmentMoments directly.
func insertAssetFaceIndexedAt(t *testing.T, db *sql.DB, assetID string, vec []float32, indexedAt time.Time) string {
	t.Helper()
	_, err := db.Exec(`INSERT INTO assets(id, file_path, status, indexed_at) VALUES(?,?, 'indexed', ?)`,
		assetID, "/x/"+assetID+".jpg", indexedAt.UTC().Format("2006-01-02 15:04:05"))
	require.NoError(t, err)
	faceID := uuid.NewString()
	_, err = db.Exec(`INSERT INTO face_detections(id, asset_id, bbox, embedding) VALUES(?,?,?,?)`,
		faceID, assetID, `{"x1":0,"y1":0,"x2":1,"y2":1}`, sqlite.SerializeFloat32(vec))
	require.NoError(t, err)
	return faceID
}

// personOfFace looks up the person a face currently belongs to.
func personOfFace(t *testing.T, db *sql.DB, faceID string) string {
	t.Helper()
	var pid string
	require.NoError(t, db.QueryRow(`SELECT person_id FROM face_person WHERE face_id=?`, faceID).Scan(&pid))
	return pid
}

// TestClusterEngine_TwoMomentTwoPeopleGrouping is the plan's primary
// service-level fixture: two moments (capture times ~2h apart, well past the
// default 60-minute momentGap), each containing the same two people (X and Y,
// orthogonal embeddings). Both engines must land on exactly 2 persons, with
// X's two appearances merged into one person and Y's two appearances merged
// into the other -- X and Y themselves must never merge.
//
// Parameterized over both engine values: this doubles as case ① (apple
// produces 2 persons with correct membership) and case ② (dbscan walks the
// unchanged legacy path and produces an equivalent grouping) from the brief.
func TestClusterEngine_TwoMomentTwoPeopleGrouping(t *testing.T) {
	for _, engine := range []string{"apple", "dbscan"} {
		t.Run(engine, func(t *testing.T) {
			setClusterEngine(t, engine)

			db := makeTestFaceDB(t)
			dim := 512
			vecX := make([]float32, dim)
			vecX[0] = 1.0
			vecY := make([]float32, dim)
			vecY[1] = 1.0

			momentA := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
			momentB := momentA.Add(2 * time.Hour)

			fXA := insertAssetFaceAt(t, db, "xa", vecX, momentA)
			fYA := insertAssetFaceAt(t, db, "ya", vecY, momentA.Add(time.Minute))
			fXB := insertAssetFaceAt(t, db, "xb", vecX, momentB)
			fYB := insertAssetFaceAt(t, db, "yb", vecY, momentB.Add(time.Minute))

			svc := service.NewFaceService(db)
			require.NoError(t, svc.RunClustering(context.Background()))

			var personCount int
			require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM persons`).Scan(&personCount))
			require.Equal(t, 2, personCount,
				"engine=%s: expected 2 persons (X merged across its two moments, Y merged across its two moments)", engine)

			pXA, pYA := personOfFace(t, db, fXA), personOfFace(t, db, fYA)
			pXB, pYB := personOfFace(t, db, fXB), personOfFace(t, db, fYB)
			require.Equal(t, pXA, pXB, "engine=%s: X's two moments must land on the same person", engine)
			require.Equal(t, pYA, pYB, "engine=%s: Y's two moments must land on the same person", engine)
			require.NotEqual(t, pXA, pYA, "engine=%s: X and Y must not be merged together", engine)
		})
	}
}

// TestClusterEngine_InvalidValueFallsBackToAppleAndWarns is case ③ from the
// brief: an unrecognized photos.ClusterEngine value must not error out or
// silently pick some undefined behavior -- it falls back to "apple" and logs
// a warning so a config typo is discoverable.
func TestClusterEngine_InvalidValueFallsBackToAppleAndWarns(t *testing.T) {
	setClusterEngine(t, "not-a-real-engine")

	obsCore, logs := observer.New(zap.WarnLevel)
	restore := zap.ReplaceGlobals(zap.New(obsCore))
	defer restore()

	db := makeTestFaceDB(t)
	dim := 512
	vecX := make([]float32, dim)
	vecX[0] = 1.0
	vecY := make([]float32, dim)
	vecY[1] = 1.0
	insertAssetFace(t, db, "a1", vecX)
	insertAssetFace(t, db, "a2", vecY)

	svc := service.NewFaceService(db)
	require.NoError(t, svc.RunClustering(context.Background()))

	var personCount int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM persons`).Scan(&personCount))
	require.Equal(t, 2, personCount,
		"an invalid engine value should still cluster correctly by falling back to apple, not error out")

	require.NotEmpty(t, logs.All(), "an invalid engine value should produce a log entry")
	var sawWarn bool
	for _, e := range logs.All() {
		if e.Level == zap.WarnLevel {
			sawWarn = true
		}
	}
	require.True(t, sawWarn, "expected a Warn-level log entry for the invalid engine value")
}

// TestRunClustering_BothEngines_BasicSplit reproduces TestRunClustering's
// fixture (2 near-identical faces + 1 orthogonal one -> 2 persons) as a new,
// explicitly-parameterized regression test: existing test files are left
// untouched, but the same scenario must hold for both engine values.
func TestRunClustering_BothEngines_BasicSplit(t *testing.T) {
	for _, engine := range []string{"apple", "dbscan"} {
		t.Run(engine, func(t *testing.T) {
			setClusterEngine(t, engine)

			db := makeTestFaceDB(t)
			_, _ = db.Exec(`INSERT INTO assets(id,file_path,status) VALUES('a1','/p1.jpg','indexed')`)
			_, _ = db.Exec(`INSERT INTO assets(id,file_path,status) VALUES('a2','/p2.jpg','indexed')`)

			dim := 512
			face := func(val float32, idx int) []float32 {
				v := make([]float32, dim)
				v[idx] = val
				return v
			}
			insert := func(faceID, assetID string, vec []float32) {
				_, _ = db.Exec(`INSERT INTO face_detections(id,asset_id,bbox,embedding) VALUES(?,?,'{}',?)`,
					faceID, assetID, sqlite.SerializeFloat32(vec))
			}
			insert("f1", "a1", face(1.0, 0))
			insert("f2", "a1", face(0.9999, 0)) // near-identical
			insert("f3", "a2", face(1.0, 1))    // orthogonal

			svc := service.NewFaceService(db)
			require.NoError(t, svc.RunClustering(context.Background()))

			var personCount int
			require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM persons`).Scan(&personCount))
			require.Equal(t, 2, personCount, "engine=%s: should have 2 persons (1 with two faces, 1 with one face)", engine)

			var fpCount int
			require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM face_person`).Scan(&fpCount))
			require.Equal(t, 3, fpCount, "engine=%s: all 3 faces should have a person association", engine)
		})
	}
}

// TestRunClustering_BothEngines_HiddenPersonSurvives reproduces
// TestRunClustering_HiddenPersonNotDeletedAndExcludedFromNewClusters as a new,
// explicitly-parameterized regression test: a hidden (anchored) person must
// survive re-clustering under both engines.
//
// The step-3 absorption mechanism itself is NOT identical across engines as
// of Task 4 of the exemplar-assignment SDD: "dbscan" still snaps the nearby
// face via the untouched legacy centroid+assignEpsilon check, but "apple"
// now requires quality-gated exemplars and a KNN vote clearing
// assignMinVotes -- hp-1 here carries no score/frontality/sharpness signals,
// so it gates out of SelectExemplars entirely and the hidden person ends up
// with zero exemplars, meaning Match always returns "none" for it. See
// faces_assign_test.go's TestRunClustering_EngineSplit_QualityGateOnlyAppliesToApple
// for this same split as its own dedicated regression, and
// TestRunClustering_AppleAssign_AutoJoinsPersonUnconfirmed for the
// exemplar-gated case that does join under "apple".
func TestRunClustering_BothEngines_HiddenPersonSurvives(t *testing.T) {
	for _, engine := range []string{"apple", "dbscan"} {
		t.Run(engine, func(t *testing.T) {
			setClusterEngine(t, engine)

			db := makeTestFaceDB(t)
			dim := 512
			a := make([]float32, dim)
			a[0] = 1.0
			insertAssetFace(t, db, "hp-1", normalize(a))
			svc := service.NewFaceService(db)
			require.NoError(t, svc.RunClustering(context.Background()))

			var pid string
			require.NoError(t, db.QueryRow(`SELECT id FROM persons`).Scan(&pid))
			_, err := db.Exec(`UPDATE persons SET hidden=1 WHERE id=?`, pid)
			require.NoError(t, err)

			a2 := make([]float32, dim)
			a2[0] = 0.97
			a2[1] = 0.03
			insertAssetFace(t, db, "hp-2", normalize(a2))
			require.NoError(t, svc.RunClustering(context.Background()))

			var hidden int
			require.NoError(t, db.QueryRow(`SELECT hidden FROM persons WHERE id=?`, pid).Scan(&hidden))
			require.Equal(t, 1, hidden, "engine=%s", engine)

			var cnt int
			require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM face_person WHERE person_id=?`, pid).Scan(&cnt))
			wantCnt := 1 // apple: no quality signals -> no exemplars -> no snap
			if engine == "dbscan" {
				wantCnt = 2 // dbscan: legacy centroid snap is unaffected by this task
			}
			require.Equal(t, wantCnt, cnt, "engine=%s", engine)
		})
	}
}

// TestRunClustering_BothEngines_CoverLockedPersonSurvives reproduces
// TestReclusterKeepsCoverLockedPerson as a new, explicitly-parameterized
// regression test.
func TestRunClustering_BothEngines_CoverLockedPersonSurvives(t *testing.T) {
	for _, engine := range []string{"apple", "dbscan"} {
		t.Run(engine, func(t *testing.T) {
			setClusterEngine(t, engine)

			db := makeTestFaceDB(t)
			vec := make([]float32, 512)
			vec[0] = 1.0
			insertAssetFace(t, db, "a1", normalize(vec))
			insertAssetFace(t, db, "a2", normalize(vec))
			fs := service.NewFaceService(db)
			require.NoError(t, fs.RunClustering(context.Background()))

			var pid, coverFace string
			require.NoError(t, db.QueryRow(`SELECT id, cover_face_id FROM persons`).Scan(&pid, &coverFace))
			_, err := db.Exec(`UPDATE persons SET cover_locked=1 WHERE id=?`, pid)
			require.NoError(t, err)

			require.NoError(t, fs.RunClustering(context.Background()))

			var pid2, coverFace2 string
			require.NoError(t, db.QueryRow(`SELECT id, cover_face_id FROM persons`).Scan(&pid2, &coverFace2))
			require.Equal(t, pid, pid2, "engine=%s: cover-locked person must survive re-clustering", engine)
			require.Equal(t, coverFace, coverFace2, "engine=%s: pinned cover must be preserved", engine)
		})
	}
}

// TestClusterEngine_AppleEngineFallsBackToIndexedAtWhenTakenAtNull covers the
// SegmentMoments fallback documented on faceRow (service/faces.go): assets
// with a NULL taken_at (e.g. missing/stripped EXIF) must still be bucketed
// into moments using indexed_at, exercised end-to-end through the real
// RunClustering -> loadFacesWithProgress -> apple-engine wiring, not just
// SegmentMoments in isolation. Same two-moment/two-person fixture as
// TestClusterEngine_TwoMomentTwoPeopleGrouping, but every asset here has
// taken_at NULL and only indexed_at set.
func TestClusterEngine_AppleEngineFallsBackToIndexedAtWhenTakenAtNull(t *testing.T) {
	setClusterEngine(t, "apple")

	db := makeTestFaceDB(t)
	dim := 512
	vecX := make([]float32, dim)
	vecX[0] = 1.0
	vecY := make([]float32, dim)
	vecY[1] = 1.0

	momentA := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	momentB := momentA.Add(2 * time.Hour)

	fXA := insertAssetFaceIndexedAt(t, db, "ixa", vecX, momentA)
	fYA := insertAssetFaceIndexedAt(t, db, "iya", vecY, momentA.Add(time.Minute))
	fXB := insertAssetFaceIndexedAt(t, db, "ixb", vecX, momentB)
	fYB := insertAssetFaceIndexedAt(t, db, "iyb", vecY, momentB.Add(time.Minute))

	svc := service.NewFaceService(db)
	require.NoError(t, svc.RunClustering(context.Background()))

	var personCount int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM persons`).Scan(&personCount))
	require.Equal(t, 2, personCount,
		"NULL taken_at faces must still segment into 2 moments via the indexed_at fallback, yielding 2 persons")

	pXA, pYA := personOfFace(t, db, fXA), personOfFace(t, db, fYA)
	pXB, pYB := personOfFace(t, db, fXB), personOfFace(t, db, fYB)
	require.Equal(t, pXA, pXB, "X's two moments (segmented via indexed_at) must land on the same person")
	require.Equal(t, pYA, pYB, "Y's two moments (segmented via indexed_at) must land on the same person")
	require.NotEqual(t, pXA, pYA, "X and Y must not be merged together")
}
