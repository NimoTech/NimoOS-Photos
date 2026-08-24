package service_test

// End-to-end tests wiring cluster-merge question generation into the real
// RunClustering pipeline (apple engine): a real two-face pass that lands
// exactly in the HAC gray band must produce a real merge_suggestions row,
// and a pre-existing face_negative_pairs entry must suppress it.

import (
	"context"
	"math"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/stretchr/testify/require"
)

// mqVecAtCosineDistance mirrors cluster_engine_test.go's vecAtCosineDistance
// (unexported, package-private there) since this file lives in
// package service_test.
func mqVecAtCosineDistance(dim int, dist float64) (v1, v2 []float32) {
	v1 = make([]float32, dim)
	v1[0] = 1.0
	cosTheta := 1.0 - dist
	sinTheta := math.Sqrt(1 - cosTheta*cosTheta)
	v2 = make([]float32, dim)
	v2[0] = float32(cosTheta)
	v2[1] = float32(sinTheta)
	return v1, v2
}

// TestRunClustering_GrayBandPairProducesMergeSuggestion is the primary
// end-to-end fixture: two faces whose distance (0.58) sits inside the
// default gray band (ClusterMergeEps=0.55, +MergeSuggestBand=0.06 ->
// (0.55, 0.61]) must NOT be merged into one person by pass-2's HAC (that's
// exactly the point of the stop line), but must produce an open
// merge_suggestions row referencing the two resulting persons.
func TestRunClustering_GrayBandPairProducesMergeSuggestion(t *testing.T) {
	db := makeTestFaceDB(t)
	dim := 512
	va, vb := mqVecAtCosineDistance(dim, 0.58)
	insertAssetFace(t, db, "gb-a", va)
	insertAssetFace(t, db, "gb-b", vb)

	svc := service.NewFaceService(db)
	require.NoError(t, svc.RunClustering(context.Background()))

	var personCount int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM persons`).Scan(&personCount))
	require.Equal(t, 2, personCount, "0.58 > ClusterMergeEps(0.55): HAC must not merge this pair")

	var cnt int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM merge_suggestions WHERE status='open'`).Scan(&cnt))
	require.Equal(t, 1, cnt, "the gray-band pair must produce exactly one open cluster-merge question")

	var dist float64
	require.NoError(t, db.QueryRow(`SELECT dist FROM merge_suggestions WHERE status='open'`).Scan(&dist))
	require.InDelta(t, 0.58, dist, 1e-3)
}

// TestRunClustering_NegativeLinkedPairNeverResuggested proves the full
// loop: reject a gray-band question (writing a normalized
// face_negative_pairs row for the two representative faces), then trigger a
// brand-new RunClustering pass over the same faces -- the auto persons are
// rebuilt with new ids, but the pair must still be suppressed because
// suppression keys on face ids, not person ids.
func TestRunClustering_NegativeLinkedPairNeverResuggested(t *testing.T) {
	db := makeTestFaceDB(t)
	dim := 512
	va, vb := mqVecAtCosineDistance(dim, 0.58)
	fa := insertAssetFace(t, db, "nl-a", va)
	fb := insertAssetFace(t, db, "nl-b", vb)

	svc := service.NewFaceService(db)
	require.NoError(t, svc.RunClustering(context.Background()))

	var id string
	require.NoError(t, db.QueryRow(`SELECT id FROM merge_suggestions WHERE status='open'`).Scan(&id))

	ps := service.NewPersonService(db)
	dec, err := ps.RejectMergeSuggestion(id)
	require.NoError(t, err)
	require.Equal(t, "rejected", dec.Status)

	var negCnt int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM face_negative_pairs WHERE face_a IN (?,?) AND face_b IN (?,?)`,
		fa, fb, fa, fb).Scan(&negCnt))
	require.Equal(t, 1, negCnt)

	// Force a fresh pass: RunClustering's step 2 deletes every non-anchored
	// (auto) person and step 4 rebuilds them with brand-new ids from the
	// same underlying faces, which are untouched.
	require.NoError(t, svc.RunClustering(context.Background()))

	var personCount int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM persons`).Scan(&personCount))
	require.Equal(t, 2, personCount, "still not merged by HAC")

	var openCnt int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM merge_suggestions WHERE status='open'`).Scan(&openCnt))
	require.Equal(t, 0, openCnt, "the negative-linked pair must not be resuggested under the rebuilt auto person ids")
}
