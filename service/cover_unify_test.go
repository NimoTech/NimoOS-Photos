package service_test

import (
	"context"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/stretchr/testify/require"
)

// TestRecomputePersonStats_QualityBreaksCentroidTie is the core regression
// case for the cover-selection unification: recomputePersonStatsTx (the
// full-rebuild path, exercised here via RunClustering) must rank covers
// using the same quality-weighted hybrid score as recomputeOneCentroidTx
// (the merge/detach/unlock path), not raw nearest-centroid distance.
//
// Two faces of the same person are constructed to be exactly equidistant
// from the centroid (symmetric about a secondary embedding dimension, the
// same trick used by TestPersonCoverHybridSelection). Face B is inserted
// FIRST and face A SECOND. Both share the same asset aesthetic score and
// face bbox/EXIF (so their hybrid scores are identical on the
// aesthetic*ratio axis) — the only difference is quality signals: A has
// frontality=1.0 and sharpness=0.9 (near-ideal), B has sharpness=0.1 (blurry)
// and no frontality (neutral).
//
// Before the fix, recomputePersonStatsTx picks the nearest-centroid face
// while completely ignoring quality; on an exact tie it keeps whichever
// face the strict "<" comparison saw first — B, since it's listed first —
// so this test FAILS pre-fix. After delegating to the shared
// selectCoverFace, A's higher quality factor wins the hybrid-score
// comparison, so cover_face_id must be A.
func TestRecomputePersonStats_QualityBreaksCentroidTie(t *testing.T) {
	db := makeTestFaceDB(t)
	dim := 512

	// v1/v2 are symmetric about dim1, so their normalized centroid direction
	// lies purely along dim0 and both are exactly equidistant from it.
	vB := make([]float32, dim)
	vB[0] = 1.0
	vB[1] = -0.3
	vA := make([]float32, dim)
	vA[0] = 1.0
	vA[1] = 0.3

	// B inserted first, A second.
	faceB := insertAssetFace(t, db, "cu-b", normalize(vB))
	faceA := insertAssetFace(t, db, "cu-a", normalize(vA))

	// Same aesthetic score and same face-area ratio for both, so only the
	// quality factor (frontality/sharpness) should differentiate them.
	_, err := db.Exec(`UPDATE assets SET aesthetic_score=8.0 WHERE id IN ('cu-a','cu-b')`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO asset_exif(asset_id, width, height) VALUES('cu-a',1000,1000),('cu-b',1000,1000)`)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE face_detections SET bbox='{"x1":0,"y1":0,"x2":300,"y2":300}' WHERE id IN (?,?)`, faceA, faceB)
	require.NoError(t, err)

	// A: near-ideal quality. B: blurry, frontality left NULL (neutral).
	_, err = db.Exec(`UPDATE face_detections SET frontality=1.0, sharpness=0.9 WHERE id=?`, faceA)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE face_detections SET sharpness=0.1 WHERE id=?`, faceB)
	require.NoError(t, err)

	fs := service.NewFaceService(db)
	require.NoError(t, fs.RunClustering(context.Background()))

	var pid, cover string
	require.NoError(t, db.QueryRow(`SELECT id, cover_face_id FROM persons`).Scan(&pid, &cover))
	require.Equal(t, faceA, cover,
		"the higher-quality face (sharp, frontal) must win the cover slot even on an exact centroid-distance tie")
}

// TestRecomputePersonStats_LockedCoverSurvivesRecluster is a regression case
// for recomputePersonStatsTx: a locked cover must not be displaced by
// selectCoverFace even when the other member face is later given a much
// higher quality-weighted score.
func TestRecomputePersonStats_LockedCoverSurvivesRecluster(t *testing.T) {
	db := makeTestFaceDB(t)
	dim := 512

	v1 := make([]float32, dim)
	v1[0] = 1.0
	v1[1] = 0.3
	v2 := make([]float32, dim)
	v2[0] = 1.0
	v2[1] = -0.3

	faceLocked := insertAssetFace(t, db, "lk-cu-1", normalize(v1))
	faceOther := insertAssetFace(t, db, "lk-cu-2", normalize(v2))

	fs := service.NewFaceService(db)
	require.NoError(t, fs.RunClustering(context.Background()))

	var pid string
	require.NoError(t, db.QueryRow(`SELECT id FROM persons`).Scan(&pid))
	_, err := db.Exec(`UPDATE persons SET cover_locked=1, cover_face_id=? WHERE id=?`, faceLocked, pid)
	require.NoError(t, err)

	// Give the OTHER (non-locked) face a much better quality-weighted hybrid
	// score than the locked one.
	_, err = db.Exec(`UPDATE assets SET aesthetic_score=9.0 WHERE id IN ('lk-cu-1','lk-cu-2')`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO asset_exif(asset_id, width, height) VALUES('lk-cu-1',1000,1000),('lk-cu-2',1000,1000)`)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE face_detections SET bbox='{"x1":0,"y1":0,"x2":300,"y2":300}' WHERE id IN (?,?)`, faceLocked, faceOther)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE face_detections SET frontality=1.0, sharpness=0.95 WHERE id=?`, faceOther)
	require.NoError(t, err)

	require.NoError(t, fs.RunClustering(context.Background()))

	var coverAfter string
	require.NoError(t, db.QueryRow(`SELECT cover_face_id FROM persons WHERE id=?`, pid).Scan(&coverAfter))
	require.Equal(t, faceLocked, coverAfter,
		"a locked cover must survive re-cluster even though the other face now scores higher")
}
