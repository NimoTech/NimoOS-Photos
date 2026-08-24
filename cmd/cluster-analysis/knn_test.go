package main

import (
	"database/sql"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/stretchr/testify/require"
)

// ── Report-formatting tests ─────────────────────────────────────────────────
//
// The truth-loading / grid-scan / selection core these tests build on now
// lives in service/calibrate_knn.go and service/calibrate_knn_test.go
// (shared with the in-service calibration runner); the tests here only
// exercise report-formatting code that stayed in this CLI package
// (computeKNNDistStats/printKNNDistribution and runKNN's full printed
// output).

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
// vec, excluded flag) satisfying loadFaces' query (INNER JOIN to a live
// asset, WHERE excluded=0 filter applied by loadFaces itself, not here).
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
// calling the tool's own cosDist) purely for computing this test's expected
// distances against 2D toy vectors.
func cosDist2D(ax, ay, bx, by float64) float64 {
	dot := ax*bx + ay*by
	na := math.Sqrt(ax*ax + ay*ay)
	nb := math.Sqrt(bx*bx + by*by)
	return 1 - dot/(na*nb)
}

// ── Fixture DB smoke test: hand-verifiable distributions + full report ─────

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

// TestKNNMode_SmokeFixture_KnownDistributionsAndBestCombo is the
// requirement's fixture-DB smoke test for report formatting: 6 persons x
// (15 low + 3 high confirmed positives, 3 hard + 2 easy rejected negatives)
// = 108 positives / 30 negatives / 6 distinct persons, clearing all three
// insufficient-data bars. The grid-scan/selection numbers themselves are
// asserted in service/calibrate_knn_test.go's
// TestKNNGridScanAndSelectCombo_SmokeFixture_KnownBestCombo against the same
// fixture shape; this test only covers computeKNNDistStats/
// printKNNDistribution's stats and runKNN's full printed report text.
func TestKNNMode_SmokeFixture_KnownDistributionsAndBestCombo(t *testing.T) {
	db := knnOpenFixtureDB(t, "knn-smoke.db")

	const nPersons = 6
	const nLowPerPerson, nHighPerPerson = 15, 3 // -> 90 low + 18 high = 108 positives
	const nHardPerPerson, nEasyPerPerson = 3, 2 // -> 18 hard + 12 easy = 30 negatives
	faceSeq := 0
	for p := 0; p < nPersons; p++ {
		knnPopulatePersonFixture(t, db, fmt.Sprintf("p%02d", p), &faceSeq,
			nLowPerPerson, nHighPerPerson, nHardPerPerson, nEasyPerPerson)
	}

	low := cosDist2D(1, 0, 4, 3)
	high := cosDist2D(1, 0, 5, 12)
	hard := cosDist2D(1, 0, 8, 15)
	easy := cosDist2D(1, 0, 0, 1)

	truth, err := service.LoadKNNTruth(db, 5)
	require.NoError(t, err)
	require.Len(t, truth.Positives, 108)
	require.Len(t, truth.Negatives, 30)

	// -- Distributions: positives sorted have the 90 "low" values first
	// (indices 0-89) then the 18 "high" values (90-107); percentile's
	// linear-interpolation indices for q1 (idx 26.75), median (53.5), and
	// q3 (80.25) all land inside the low block (< 90), so only max reaches
	// the high value.
	posStats := computeKNNDistStats(truth.Positives)
	require.Equal(t, 108, posStats.Count)
	require.InDelta(t, low, posStats.Min, 1e-9)
	require.InDelta(t, low, posStats.Q1, 1e-9)
	require.InDelta(t, low, posStats.Median, 1e-9)
	require.InDelta(t, low, posStats.Q3, 1e-9)
	require.InDelta(t, high, posStats.Max, 1e-9)

	// Negatives sorted: 18 "hard" values first (indices 0-17), then 12
	// "easy" values (18-29). q1 (idx 7.25) and median (14.5) land in the
	// hard block; q3 (idx 21.75) lands in the easy block.
	negStats := computeKNNDistStats(truth.Negatives)
	require.Equal(t, 30, negStats.Count)
	require.InDelta(t, hard, negStats.Min, 1e-9)
	require.InDelta(t, hard, negStats.Q1, 1e-9)
	require.InDelta(t, hard, negStats.Median, 1e-9)
	require.InDelta(t, easy, negStats.Q3, 1e-9)
	require.InDelta(t, easy, negStats.Max, 1e-9)

	// -- Full report wiring: the recommendation and warning-absence must
	// show up in runKNN's actual printed output, not just the underlying
	// service helpers.
	var buf strings.Builder
	runKNN(&buf, db, 5)
	out := buf.String()
	require.NotContains(t, out, "INSUFFICIENT DATA")
	require.Contains(t, out, "RECOMMENDED: T_auto=0.35 T_suggest=0.53  falseAccept=0 trueAccept=90 grayPositives=0 grayNegatives=18 miss=18")
}

// ── Insufficient-data warning: appears below the bars, disappears above ────

// knnBulkFixture inserts nPersons persons, each with one exemplar and
// posPerPerson/negPerPerson trivial-distance truth rows -- used only to hit
// (or stay under) the insufficient-data bars; the exact distances don't
// matter for this test; both are far from the 0.01 grid boundaries.
func knnBulkFixture(t *testing.T, db *sql.DB, nPersons, posPerPerson, negPerPerson int) {
	t.Helper()
	faceSeq := 0
	next := func() string { faceSeq++; return fmt.Sprintf("bulk_f%05d", faceSeq) }
	for p := 0; p < nPersons; p++ {
		personID := fmt.Sprintf("bp%02d", p)
		knnInsertPerson(t, db, personID)
		ex := next()
		knnInsertFace(t, db, ex, []float32{1, 0}, false)
		knnInsertFacePersonRow(t, db, ex, personID, true, false)
		for i := 0; i < posPerPerson; i++ {
			f := next()
			knnInsertFace(t, db, f, []float32{4, 3}, false)
			knnInsertFacePersonRow(t, db, f, personID, false, true)
		}
		for i := 0; i < negPerPerson; i++ {
			f := next()
			knnInsertFace(t, db, f, []float32{0, 1}, false)
			knnInsertNegative(t, db, f, personID)
		}
	}
}

func TestKNNMode_InsufficientDataWarning_AppearsBelowBarsDisappearsAbove(t *testing.T) {
	// Below every bar: 2 persons (< 5), 6 positives (< 100), 4 negatives (< 20).
	dbSmall := knnOpenFixtureDB(t, "knn-small.db")
	knnBulkFixture(t, dbSmall, 2, 3, 2)
	var bufSmall strings.Builder
	runKNN(&bufSmall, dbSmall, 5)
	require.Contains(t, bufSmall.String(), "INSUFFICIENT DATA")

	// Above every bar: 6 persons (>= 5), 120 positives (>= 100), 30 negatives (>= 20).
	dbBig := knnOpenFixtureDB(t, "knn-big.db")
	knnBulkFixture(t, dbBig, 6, 20, 5)
	var bufBig strings.Builder
	runKNN(&bufBig, dbBig, 5)
	require.NotContains(t, bufBig.String(), "INSUFFICIENT DATA")
}
