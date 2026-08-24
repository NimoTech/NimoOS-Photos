package service

import (
	"database/sql"
	"fmt"
	"math"
	"path/filepath"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	"github.com/stretchr/testify/require"
)

// ── Pure-function test: knnDistance's self-exclusion + median-of-k ─────────

// TestKNNDistance_SelfExclusionAndMedianOfK reproduces, without any DB, the
// exact statistic matcher.go's Match computes: median distance of a
// person's exemplars among the k nearest of them to the query vector. All
// vectors here are 2D toys -- cosDist only cares about relative angle, not
// dimensionality -- with components chosen from small Pythagorean triples so
// every expected distance is an exact rational (4,3)/5 -> cos=0.8 -> dist=0.2,
// (3,4)/5 -> cos=0.6 -> dist=0.4, (0,1) vs (1,0) -> cos=0 -> dist=1.0.
func TestKNNDistance_SelfExclusionAndMedianOfK(t *testing.T) {
	exemplars := []knnExemplarFace{
		{FaceID: "e1", Vec: []float32{1, 0}}, // dist to query below: 0
		{FaceID: "e2", Vec: []float32{4, 3}}, // dist 0.2
		{FaceID: "e3", Vec: []float32{3, 4}}, // dist 0.4
		{FaceID: "e4", Vec: []float32{0, 1}}, // dist 1.0
	}
	query := []float32{1, 0}

	// Query face ID is NOT one of this person's exemplars: no self-exclusion
	// applies, so the pool is all 4. Nearest 2 are e1 (0) and e2 (0.2) ->
	// median (even count) = mean(0, 0.2) = 0.1.
	dist, ok := knnDistance(exemplars, "not-an-exemplar", query, "p1", 2)
	require.True(t, ok)
	require.InDelta(t, 0.1, dist, 1e-9)

	// Query face ID IS e1 itself: self-exclusion must drop it from the pool,
	// so the nearest 2 of the remaining three are e2 (0.2) and e3 (0.4) ->
	// median = mean(0.2, 0.4) = 0.3. This must differ from the case above --
	// if it doesn't, self-exclusion silently isn't happening.
	dist, ok = knnDistance(exemplars, "e1", query, "p1", 2)
	require.True(t, ok)
	require.InDelta(t, 0.3, dist, 1e-9)

	// k=3, self-excluded: nearest 3 of {e2(0.2), e3(0.4), e4(1.0)} is all
	// three (odd count) -> median = the middle sorted value = 0.4.
	dist, ok = knnDistance(exemplars, "e1", query, "p1", 3)
	require.True(t, ok)
	require.InDelta(t, 0.4, dist, 1e-9)
}

// TestKNNDistance_SelfExclusionEmptiesSoloExemplar confirms a person whose
// ONLY exemplar is the face being evaluated against itself reports "no
// usable distance" (ok=false), not a trivial (and meaningless) zero.
func TestKNNDistance_SelfExclusionEmptiesSoloExemplar(t *testing.T) {
	exemplars := []knnExemplarFace{{FaceID: "e1", Vec: []float32{1, 0}}}
	_, ok := knnDistance(exemplars, "e1", []float32{1, 0}, "p1", 5)
	require.False(t, ok)
}

// ── Fixture DB helpers ───────────────────────────────────────────────────

func knnOpenFixtureDB(t *testing.T, name string) *sql.DB {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), name))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	return db
}

func knnInsertPerson(t *testing.T, db *sql.DB, personID string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO persons(id, name) VALUES(?, '')`, personID)
	require.NoError(t, err)
}

// knnInsertFace inserts a minimal asset + face_detections row (embedding
// vec, excluded flag) satisfying loadKNNFaceVectors' query (INNER JOIN to a
// live asset, WHERE excluded=0 filter applied by loadKNNFaceVectors itself,
// not here).
func knnInsertFace(t *testing.T, db *sql.DB, faceID string, vec []float32, excluded bool) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO assets(id, file_path, status) VALUES(?, ?, 'indexed')`,
		faceID, "/fixture/"+faceID+".jpg")
	require.NoError(t, err)
	exclInt := 0
	if excluded {
		exclInt = 1
	}
	_, err = db.Exec(`INSERT INTO face_detections(id, asset_id, bbox, embedding, excluded) VALUES(?, ?, ?, ?, ?)`,
		faceID, faceID, `{"x1":0,"y1":0,"x2":10,"y2":10}`, sqlite.SerializeFloat32(vec), exclInt)
	require.NoError(t, err)
}

func knnInsertFacePersonRow(t *testing.T, db *sql.DB, faceID, personID string, exemplar, confirmed bool) {
	t.Helper()
	toInt := func(b bool) int {
		if b {
			return 1
		}
		return 0
	}
	_, err := db.Exec(`INSERT INTO face_person(face_id, person_id, exemplar, confirmed) VALUES(?, ?, ?, ?)`,
		faceID, personID, toInt(exemplar), toInt(confirmed))
	require.NoError(t, err)
}

func knnInsertNegative(t *testing.T, db *sql.DB, faceID, personID string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO person_negatives(person_id, face_id, created_at) VALUES(?, ?, CURRENT_TIMESTAMP)`,
		personID, faceID)
	require.NoError(t, err)
}

// cosDist2D independently reimplements the cosine-distance formula (not by
// calling the service's own cosDist) purely for computing this test's
// expected distances against 2D toy vectors.
func cosDist2D(ax, ay, bx, by float64) float64 {
	dot := ax*bx + ay*by
	na := math.Sqrt(ax*ax + ay*ay)
	nb := math.Sqrt(bx*bx + by*by)
	return 1 - dot/(na*nb)
}

// ── Fixture DB test: loading, self-exclusion, and skip counters ────────────

// TestLoadKNNTruth_LoadingSelfExclusionAndSkipCounters exercises
// LoadKNNTruth end-to-end against a real (synthetic) production-schema DB,
// covering: a normal confirmed positive and a normal negative each with a
// known, hand-verified distance; a person whose ONLY exemplar is ALSO the
// confirmed face under test (self-exclusion must empty its pool, counted as
// skipped rather than a bogus zero-distance positive); and an excluded face
// referenced from each of the confirmed/negative tables (must be skipped as
// "face not loaded", matching loadKNNFaceVectors' excluded=0 filter).
func TestLoadKNNTruth_LoadingSelfExclusionAndSkipCounters(t *testing.T) {
	db := knnOpenFixtureDB(t, "knn-loading.db")

	// Person pA: one exemplar, one normal confirmed positive, one normal
	// negative, plus one excluded confirmed face and one excluded negative
	// face (both must be skipped as "face not loaded").
	knnInsertPerson(t, db, "pA")
	knnInsertFace(t, db, "exA", []float32{1, 0}, false)
	knnInsertFacePersonRow(t, db, "exA", "pA", true, false)

	knnInsertFace(t, db, "cA1", []float32{4, 3}, false) // dist 0.2
	knnInsertFacePersonRow(t, db, "cA1", "pA", false, true)

	knnInsertFace(t, db, "nA1", []float32{0, 1}, false) // dist 1.0
	knnInsertNegative(t, db, "nA1", "pA")

	knnInsertFace(t, db, "cAexcl", []float32{4, 3}, true) // excluded=1
	knnInsertFacePersonRow(t, db, "cAexcl", "pA", false, true)

	knnInsertFace(t, db, "nAexcl", []float32{0, 1}, true) // excluded=1
	knnInsertNegative(t, db, "nAexcl", "pA")

	// Person pB: its ONLY exemplar face is also confirmed=1 on the same
	// row -- self-exclusion must empty pB's pool for this row, so it's
	// skipped as "no exemplar", not counted as a trivial zero-distance
	// positive.
	knnInsertPerson(t, db, "pB")
	knnInsertFace(t, db, "exB", []float32{1, 0}, false)
	knnInsertFacePersonRow(t, db, "exB", "pB", true, true)

	truth, err := LoadKNNTruth(db, 5)
	require.NoError(t, err)

	require.Len(t, truth.Positives, 1)
	require.Equal(t, "cA1", truth.Positives[0].FaceID)
	require.InDelta(t, cosDist2D(1, 0, 4, 3), truth.Positives[0].Dist, 1e-9)
	require.Equal(t, 1, truth.PosSkippedNoFace, "cAexcl (excluded=1) must be skipped as face-not-loaded")
	require.Equal(t, 1, truth.PosSkippedNoExemplar, "pB's only exemplar is the confirmed face itself -- self-exclusion must empty its pool")

	require.Len(t, truth.Negatives, 1)
	require.Equal(t, "nA1", truth.Negatives[0].FaceID)
	require.InDelta(t, cosDist2D(1, 0, 0, 1), truth.Negatives[0].Dist, 1e-9)
	require.Equal(t, 1, truth.NegSkippedNoFace, "nAexcl (excluded=1) must be skipped as face-not-loaded")

	require.Len(t, truth.DistinctPersons, 1)
	require.True(t, truth.DistinctPersons["pA"])
	require.False(t, truth.DistinctPersons["pB"], "pB contributed zero usable rows, must not count toward distinct-persons")

	// Exemplar template counts: pA's exA + pB's exB are both loaded
	// (excluded=0) -> 2 persons, 2 faces.
	require.Equal(t, 2, truth.ExemplarPersons)
	require.Equal(t, 2, truth.ExemplarFaces)
}

// ── Fixture DB smoke test: hand-verifiable grid scan + selection ───────────

// knnPopulatePersonFixture inserts one person with an exemplar template and
// four categories of truth rows against it: nLow confirmed positives at a
// clearly-good distance, nHigh confirmed positives at a clearly-hard
// distance (never auto-acceptable within the grid), nHard rejected
// negatives at a distance inside the T_auto grid's range (exercises the
// false-accept boundary), and nEasy rejected negatives at a distance far
// beyond the entire grid (never auto- or gray-zone eligible). Distances are
// derived from Pythagorean triples against the exemplar (1,0) so they are
// exact rationals, and the "hard"/"high" values are deliberately NOT equal
// to any 0.01 grid point (9/17 and 8/13 respectively), so every grid
// threshold comparison below has a safe floating-point margin rather than
// resting on an exact-equality coin flip.
func knnPopulatePersonFixture(t *testing.T, db *sql.DB, personID string, faceSeq *int, nLow, nHigh, nHard, nEasy int) {
	t.Helper()
	next := func() string {
		*faceSeq++
		return fmt.Sprintf("%s_f%04d", personID, *faceSeq)
	}

	knnInsertPerson(t, db, personID)
	ex := next()
	knnInsertFace(t, db, ex, []float32{1, 0}, false)
	knnInsertFacePersonRow(t, db, ex, personID, true, false)

	for i := 0; i < nLow; i++ {
		f := next()
		knnInsertFace(t, db, f, []float32{4, 3}, false) // dist = 1-4/5 = 0.2
		knnInsertFacePersonRow(t, db, f, personID, false, true)
	}
	for i := 0; i < nHigh; i++ {
		f := next()
		knnInsertFace(t, db, f, []float32{5, 12}, false) // dist = 1-5/13 ~= 0.6154
		knnInsertFacePersonRow(t, db, f, personID, false, true)
	}
	for i := 0; i < nHard; i++ {
		f := next()
		knnInsertFace(t, db, f, []float32{8, 15}, false) // dist = 1-8/17 ~= 0.5294
		knnInsertNegative(t, db, f, personID)
	}
	for i := 0; i < nEasy; i++ {
		f := next()
		knnInsertFace(t, db, f, []float32{0, 1}, false) // dist = 1.0
		knnInsertNegative(t, db, f, personID)
	}
}

// TestKNNGridScanAndSelectCombo_SmokeFixture_KnownBestCombo is the
// requirement's fixture-DB smoke test for the grid-scan/selection core: 6
// persons x (15 low + 3 high confirmed positives, 3 hard + 2 easy rejected
// negatives) = 108 positives / 30 negatives / 6 distinct persons, clearing
// all three insufficient-data bars. Every count below is derived by hand in
// the comments; see knnPopulatePersonFixture's doc comment for the distance
// derivations. (Distribution-stats and full report-text assertions over the
// same fixture live in cmd/cluster-analysis/knn_test.go, since those exercise
// report-formatting code that stayed in the CLI.)
func TestKNNGridScanAndSelectCombo_SmokeFixture_KnownBestCombo(t *testing.T) {
	db := knnOpenFixtureDB(t, "knn-smoke.db")

	const nPersons = 6
	const nLowPerPerson, nHighPerPerson = 15, 3 // -> 90 low + 18 high = 108 positives
	const nHardPerPerson, nEasyPerPerson = 3, 2 // -> 18 hard + 12 easy = 30 negatives
	faceSeq := 0
	for p := 0; p < nPersons; p++ {
		knnPopulatePersonFixture(t, db, fmt.Sprintf("p%02d", p), &faceSeq,
			nLowPerPerson, nHighPerPerson, nHardPerPerson, nEasyPerPerson)
	}

	truth, err := LoadKNNTruth(db, 5)
	require.NoError(t, err)

	require.Len(t, truth.Positives, 108)
	require.Len(t, truth.Negatives, 30)
	require.Len(t, truth.DistinctPersons, nPersons)
	require.Zero(t, truth.PosSkippedNoFace)
	require.Zero(t, truth.PosSkippedNoExemplar)
	require.Zero(t, truth.NegSkippedNoFace)

	// -- Grid scan + selection --
	// trueAccept is FLAT at 90 across the entire T_auto grid: "low"
	// (0.2) always clears every T_auto>=0.35, "high" (~0.6154) never
	// does (grid max is 0.55).
	// falseAccept is 0 for T_auto in [0.35,0.52] (all below "hard"
	// ~0.5294) and 18 for T_auto in [0.53,0.55] (>= "hard") -- "easy"
	// (1.0) never triggers within the grid at all.
	// Among the 18 zero-false-accept T_auto values, grayNegatives maxes
	// out at 18 (all the "hard" negatives) once T_suggest clears ~0.5294,
	// i.e. T_suggest>=0.53 -- reachable for every candidate T_auto, so
	// the tie survives down to the smallest (T_auto, T_suggest) pair:
	// (0.35, 0.53).
	results := KNNGridScan(truth.Positives, truth.Negatives)
	SortKNNResults(results)
	require.NotEmpty(t, results)
	best := results[0]
	require.InDelta(t, 0.35, best.TAuto, 1e-9)
	require.InDelta(t, 0.53, best.TSuggest, 1e-9)
	require.Equal(t, 0, best.FalseAccept)
	require.Equal(t, 90, best.TrueAccept)
	require.Equal(t, 0, best.GrayPositives, "the 18 'high' positives miss T_suggest=0.53 entirely (~0.6154 > 0.53), landing in Miss instead")
	require.Equal(t, 18, best.GrayNegatives)
	require.Equal(t, 18, best.Miss)

	tAuto, tSuggest, ok := SelectKNNCombo(results)
	require.True(t, ok)
	require.InDelta(t, 0.35, tAuto, 1e-9)
	require.InDelta(t, 0.53, tSuggest, 1e-9)

	require.False(t, KNNInsufficient(len(truth.Positives), len(truth.Negatives), len(truth.DistinctPersons)))
}

// ── KNNInsufficient: bars in isolation ──────────────────────────────────

// TestKNNInsufficient_EachBarIndividually confirms all three bars
// (positives>=100, negatives>=20, persons>=5) gate independently: falling
// short on any single one is enough to report insufficient, and clearing
// all three at once reports sufficient.
func TestKNNInsufficient_EachBarIndividually(t *testing.T) {
	require.True(t, KNNInsufficient(99, 100, 100), "positives just under the bar")
	require.True(t, KNNInsufficient(100, 19, 100), "negatives just under the bar")
	require.True(t, KNNInsufficient(100, 100, 4), "persons just under the bar")
	require.False(t, KNNInsufficient(100, 20, 5), "exactly at every bar is sufficient")
}

// ── SelectKNNCombo: three required cases ────────────────────────────────

// TestSelectKNNCombo_ZeroFalseAcceptHeadWins covers the case where the
// (already-sorted) grid has at least one zero-false-accept combo: the head
// of the sorted slice is returned verbatim.
func TestSelectKNNCombo_ZeroFalseAcceptHeadWins(t *testing.T) {
	rs := []KNNComboResult{
		{TAuto: 0.35, TSuggest: 0.53, FalseAccept: 0, TrueAccept: 90},
		{TAuto: 0.36, TSuggest: 0.54, FalseAccept: 0, TrueAccept: 88},
		{TAuto: 0.53, TSuggest: 0.60, FalseAccept: 3, TrueAccept: 95},
	}
	tAuto, tSuggest, ok := SelectKNNCombo(rs)
	require.True(t, ok)
	require.Equal(t, rs[0].TAuto, tAuto)
	require.Equal(t, rs[0].TSuggest, tSuggest)
}

// TestSelectKNNCombo_EveryComboLeaksFalseAccept covers the case where every
// combo in the grid has FalseAccept > 0 (no threshold reaches zero
// false-accepts): ok must be false.
func TestSelectKNNCombo_EveryComboLeaksFalseAccept(t *testing.T) {
	rs := []KNNComboResult{
		{TAuto: 0.35, TSuggest: 0.53, FalseAccept: 1, TrueAccept: 90},
		{TAuto: 0.36, TSuggest: 0.54, FalseAccept: 2, TrueAccept: 91},
	}
	SortKNNResults(rs)
	_, _, ok := SelectKNNCombo(rs)
	require.False(t, ok)
}

// TestSelectKNNCombo_EmptyGrid covers an empty grid (e.g. truth so sparse
// KNNGridScan produced nothing): ok must be false.
func TestSelectKNNCombo_EmptyGrid(t *testing.T) {
	_, _, ok := SelectKNNCombo(nil)
	require.False(t, ok)
}
