package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/NimoTech/NimoOS-Photos/common"
	"github.com/NimoTech/NimoOS-Photos/pkg/config"
	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	"github.com/stretchr/testify/require"
)

// ── Shared fixture helpers ───────────────────────────────────────────────

// knnVecAtDist returns a unit 2D vector v = (cos(theta), sin(theta)) such
// that cosDist2D(1,0, v...) == dist exactly (up to float64 rounding):
// cos(theta) = 1-dist by construction, since cosDist between (1,0) and a
// unit vector is 1-cos(theta). Lets every knn fixture in this file place a
// truth row at a precisely chosen distance without hunting for Pythagorean
// triples.
func knnVecAtDist(dist float64) []float32 {
	cosTheta := 1 - dist
	sinTheta := math.Sqrt(1 - cosTheta*cosTheta)
	return []float32{float32(cosTheta), float32(sinTheta)}
}

// runCalibrateTestQuery is a tiny helper reducing boilerplate for the
// single-value lookups below.
func mustScanFloat(t *testing.T, db *sql.DB, query string, args ...any) (float64, bool) {
	t.Helper()
	var v float64
	err := db.QueryRow(query, args...).Scan(&v)
	if err == sql.ErrNoRows {
		return 0, false
	}
	require.NoError(t, err)
	return v, true
}

// calibHistoryRow mirrors one calibration_history row for test assertions.
type calibHistoryRow struct {
	Tier        string
	Outcome     string
	TruthCounts map[string]any
	OldValues   map[string]float64
	NewValues   map[string]float64
}

func queryCalibHistory(t *testing.T, db *sql.DB) []calibHistoryRow {
	t.Helper()
	rows, err := db.Query(`SELECT tier, outcome, truth_counts, old_values, new_values FROM calibration_history ORDER BY id`)
	require.NoError(t, err)
	defer rows.Close()
	var out []calibHistoryRow
	for rows.Next() {
		var r calibHistoryRow
		var truthRaw, oldRaw, newRaw string
		require.NoError(t, rows.Scan(&r.Tier, &r.Outcome, &truthRaw, &oldRaw, &newRaw))
		require.NoError(t, json.Unmarshal([]byte(truthRaw), &r.TruthCounts))
		require.NoError(t, json.Unmarshal([]byte(oldRaw), &r.OldValues))
		require.NoError(t, json.Unmarshal([]byte(newRaw), &r.NewValues))
		out = append(out, r)
	}
	require.NoError(t, rows.Err())
	return out
}

func countCalibState(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM calibration_state`).Scan(&n))
	return n
}

// knnInsertDecidedSuggestions inserts n decided (status='accepted')
// person_suggestions rows so the knn tier's throttle (step 1, gated on
// person_suggestions.decided_at, a signal independent of the confirmed/
// negative truth rows the tier's bars/recommendation actually read) is due.
// person_suggestions carries no foreign keys on person_id/face_id, so these
// rows need not reference any real person/face.
func knnInsertDecidedSuggestions(t *testing.T, db *sql.DB, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		_, err := db.Exec(`INSERT INTO person_suggestions(id, person_id, face_id, kind, score, status, created_at, decided_at)
			VALUES(?,?,?,?,?,?,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`,
			fmt.Sprintf("throttle-sugg-%04d", i), "throttle-person", fmt.Sprintf("throttle-face-%04d", i),
			"join", 0.5, "accepted")
		require.NoError(t, err)
	}
}

// mergeInsertDecidedPairs inserts n decided merge_suggestions rows, one per
// generated distinct person pair drawn from persons[], at the given dist and
// status. Panics (via require) if it runs out of distinct pairs -- callers
// must pass enough persons for the requested n.
func mergeInsertDecidedPairs(t *testing.T, db *sql.DB, persons []string, start *int, n int, dist float64, status string) {
	t.Helper()
	for k := 0; k < n; k++ {
		i, j := nextPairIndices(persons, start)
		mergeInsertSuggestion(t, db, persons[i], persons[j], dist, status)
	}
}

// nextPairIndices walks all (i,j) i<j combinations of persons in order,
// using *cursor as a flattened combination index that advances by one per
// call.
func nextPairIndices(persons []string, cursor *int) (int, int) {
	n := len(persons)
	c := *cursor
	*cursor++
	for i := 0; i < n; i++ {
		remaining := n - i - 1
		if c < remaining {
			return i, i + 1 + c
		}
		c -= remaining
	}
	panic("nextPairIndices: ran out of distinct person pairs")
}

// ── Case 1: knn truth insufficient -> one held_insufficient row ──────────

func TestRunKNNTier_InsufficientTruth_HeldInsufficientRow(t *testing.T) {
	resetCalibState(t)
	db := testCalibDB(t)
	SetCalibrationDB(db)
	knnInsertDecidedSuggestions(t, db, 20) // clears the throttle so the bars check actually runs
	s := NewFaceService(db)

	require.NoError(t, s.runKNNTier(context.Background()))

	hist := queryCalibHistory(t, db)
	require.Len(t, hist, 1)
	require.Equal(t, "knn", hist[0].Tier)
	require.Equal(t, "held_insufficient", hist[0].Outcome)
	require.Equal(t, 0, countCalibState(t, db))
}

// ── Case 2: knn sufficient, zero-FA grid solution -> applied, both keys ───

// knnBuildCase2Fixture populates 6 persons with a P1 baseline (dist 0.10,
// always auto-accepted), a P2 bump (dist 0.469, only auto-accepted once
// T_auto>=0.47) and negatives (dist 0.499, false-accepted only once
// T_auto>=0.50). Hand-verified (see the task's design notes): the unique
// winning combo is (T_auto=0.47, T_suggest=0.52) -- both within the builtin
// profile's bands, so boundAdjust never clamps either key; only step-2's
// max-step truncation limits AssignSuggestDist's move, and the result is
// "applied" for both keys, not "clamped".
func knnBuildCase2Fixture(t *testing.T, db *sql.DB) {
	t.Helper()
	const nPersons = 6
	faceSeq := 0
	next := func() string {
		faceSeq++
		return fmt.Sprintf("f%04d", faceSeq)
	}
	for p := 0; p < nPersons; p++ {
		personID := fmt.Sprintf("p%02d", p)
		knnInsertPerson(t, db, personID)
		ex := next()
		knnInsertFace(t, db, ex, []float32{1, 0}, false)
		knnInsertFacePersonRow(t, db, ex, personID, true, false)

		for i := 0; i < 15; i++ { // P1 baseline: 15*6=90 positives at dist 0.10
			f := next()
			knnInsertFace(t, db, f, knnVecAtDist(0.10), false)
			knnInsertFacePersonRow(t, db, f, personID, false, true)
		}
		for i := 0; i < 3; i++ { // P2 bump: 3*6=18 positives at dist 0.469
			f := next()
			knnInsertFace(t, db, f, knnVecAtDist(0.469), false)
			knnInsertFacePersonRow(t, db, f, personID, false, true)
		}
		for i := 0; i < 4; i++ { // negatives: 4*6=24 at dist 0.499
			f := next()
			knnInsertFace(t, db, f, knnVecAtDist(0.499), false)
			knnInsertNegative(t, db, f, personID)
		}
	}
}

func TestRunKNNTier_SufficientZeroFAGrid_AppliedBothKeys(t *testing.T) {
	resetCalibState(t)
	db := testCalibDB(t)
	SetCalibrationDB(db)
	knnInsertDecidedSuggestions(t, db, 20)
	knnBuildCase2Fixture(t, db)
	s := NewFaceService(db)

	require.NoError(t, s.runKNNTier(context.Background()))

	hist := queryCalibHistory(t, db)
	require.Len(t, hist, 1)
	require.Equal(t, "knn", hist[0].Tier)
	require.Equal(t, "applied", hist[0].Outcome)
	require.InDelta(t, 0.47, hist[0].NewValues["AssignAutoDist"], 1e-9)
	require.InDelta(t, 0.58, hist[0].NewValues["AssignSuggestDist"], 1e-9)

	autoVal, ok := mustScanFloat(t, db, `SELECT value FROM calibration_state WHERE key='AssignAutoDist'`)
	require.True(t, ok)
	require.InDelta(t, 0.47, autoVal, 1e-9)
	var autoGen string
	require.NoError(t, db.QueryRow(`SELECT model_gen FROM calibration_state WHERE key='AssignAutoDist'`).Scan(&autoGen))
	require.Equal(t, common.MLModelGen, autoGen)

	suggestVal, ok := mustScanFloat(t, db, `SELECT value FROM calibration_state WHERE key='AssignSuggestDist'`)
	require.True(t, ok)
	require.InDelta(t, 0.58, suggestVal, 1e-9)

	// The threshold cache must already be invalidated: resolveThreshold picks
	// up the new value on the very next call, no manual invalidation needed.
	v, src := resolveThreshold("AssignAutoDist", 0.45)
	require.InDelta(t, 0.47, v, 1e-9)
	require.Equal(t, sourceCalibrated, src)
}

// ── Case 3: 61%-dominant positives -> held_skewed, state untouched ───────

func TestRunKNNTier_SkewedPositives_HeldSkewedStateUntouched(t *testing.T) {
	resetCalibState(t)
	db := testCalibDB(t)
	SetCalibrationDB(db)
	knnInsertDecidedSuggestions(t, db, 20)

	persons := []string{"pDom", "pO1", "pO2", "pO3", "pO4"}
	faceSeq := 0
	next := func() string {
		faceSeq++
		return fmt.Sprintf("f%04d", faceSeq)
	}
	for _, p := range persons {
		knnInsertPerson(t, db, p)
		ex := next()
		knnInsertFace(t, db, ex, []float32{1, 0}, false)
		knnInsertFacePersonRow(t, db, ex, p, true, false)
	}
	// dominant person: 70 positives (dist 0.10) -- 70/110 = 63.6% > 60%.
	for i := 0; i < 70; i++ {
		f := next()
		knnInsertFace(t, db, f, knnVecAtDist(0.10), false)
		knnInsertFacePersonRow(t, db, f, "pDom", false, true)
	}
	for _, p := range []string{"pO1", "pO2", "pO3", "pO4"} {
		for i := 0; i < 10; i++ {
			f := next()
			knnInsertFace(t, db, f, knnVecAtDist(0.10), false)
			knnInsertFacePersonRow(t, db, f, p, false, true)
		}
	}
	// negatives: far away (dist 0.95, never false-accepted within the grid).
	for _, p := range persons {
		for i := 0; i < 5; i++ {
			f := next()
			knnInsertFace(t, db, f, knnVecAtDist(0.95), false)
			knnInsertNegative(t, db, f, p)
		}
	}

	s := NewFaceService(db)
	require.NoError(t, s.runKNNTier(context.Background()))

	hist := queryCalibHistory(t, db)
	require.Len(t, hist, 1)
	require.Equal(t, "held_skewed", hist[0].Outcome)
	require.Equal(t, 0, countCalibState(t, db))
}

// ── Case 4: suggested value crosses profile Max -> clamped ───────────────

// mergeBuildFixture inserts persons p0..p(n-1) and the requested counts of
// decided merge_suggestions rows against distinct person pairs.
func mergeBuildFixture(t *testing.T, db *sql.DB, nPersons int) []string {
	t.Helper()
	persons := make([]string, nPersons)
	for i := range persons {
		persons[i] = fmt.Sprintf("mp%02d", i)
		mergeInsertPerson(t, db, persons[i])
	}
	return persons
}

func TestRunMergeTier_SuggestedAboveMax_Clamped(t *testing.T) {
	resetCalibState(t)
	db := testCalibDB(t)
	SetCalibrationDB(db)

	persons := mergeBuildFixture(t, db, 10)
	cursor := 0
	mergeInsertDecidedPairs(t, db, persons, &cursor, 24, 0.30, "accepted")
	mergeInsertDecidedPairs(t, db, persons, &cursor, 1, 0.70, "accepted") // largest accepted, drives the cut point
	mergeInsertDecidedPairs(t, db, persons, &cursor, 6, 0.80, "rejected")

	s := NewFaceService(db)
	require.NoError(t, s.runMergeTier(context.Background()))

	hist := queryCalibHistory(t, db)
	require.Len(t, hist, 1)
	require.Equal(t, "merge", hist[0].Tier)
	require.Equal(t, "clamped", hist[0].Outcome)
	// cut=0.70 clamps to the profile Max (0.65), then boundAdjust's max-step
	// limits the actual move from the 0.55 default to 0.57.
	require.InDelta(t, 0.57, hist[0].NewValues["ClusterMergeEps"], 1e-9)

	val, ok := mustScanFloat(t, db, `SELECT value FROM calibration_state WHERE key='ClusterMergeEps'`)
	require.True(t, ok)
	require.InDelta(t, 0.57, val, 1e-9)
}

// ── Case 5: a conf-blocked key is never written; the other key still is ──

func TestRunKNNTier_ConfBlockedKeyNotWritten_OtherKeyStillApplied(t *testing.T) {
	resetCalibState(t)
	db := testCalibDB(t)
	SetCalibrationDB(db)
	knnInsertDecidedSuggestions(t, db, 20)
	knnBuildCase2Fixture(t, db)
	config.Cfg = &config.Config{
		AssignAutoDist: 0.50,
		Explicit:       map[string]bool{"AssignAutoDist": true},
	}

	s := NewFaceService(db)
	require.NoError(t, s.runKNNTier(context.Background()))

	hist := queryCalibHistory(t, db)
	require.Len(t, hist, 1)
	require.Equal(t, "applied", hist[0].Outcome)
	// AssignAutoDist is conf-blocked: old==new==the conf value, never adjusted.
	require.InDelta(t, 0.50, hist[0].OldValues["AssignAutoDist"], 1e-9)
	require.InDelta(t, 0.50, hist[0].NewValues["AssignAutoDist"], 1e-9)
	// AssignSuggestDist is untouched by config -- still adjusted normally.
	require.InDelta(t, 0.58, hist[0].NewValues["AssignSuggestDist"], 1e-9)

	_, ok := mustScanFloat(t, db, `SELECT value FROM calibration_state WHERE key='AssignAutoDist'`)
	require.False(t, ok, "a conf-blocked key must never be written to calibration_state")

	suggestVal, ok := mustScanFloat(t, db, `SELECT value FROM calibration_state WHERE key='AssignSuggestDist'`)
	require.True(t, ok)
	require.InDelta(t, 0.58, suggestVal, 1e-9)
}

// ── Case 6: throttle -- rerunning with no new evidence silently skips ────

func TestRunMergeTier_Throttle_SecondRunWithNoNewEvidenceSkipsSilently(t *testing.T) {
	resetCalibState(t)
	db := testCalibDB(t)
	SetCalibrationDB(db)

	persons := mergeBuildFixture(t, db, 10)
	cursor := 0
	mergeInsertDecidedPairs(t, db, persons, &cursor, 25, 0.30, "accepted")
	mergeInsertDecidedPairs(t, db, persons, &cursor, 6, 0.60, "rejected")

	s := NewFaceService(db)
	require.NoError(t, s.runMergeTier(context.Background()))
	require.Len(t, queryCalibHistory(t, db), 1, "first run must write exactly one history row")

	// Rerun immediately with no newly decided rows: throttle must silently
	// skip (no second history row).
	require.NoError(t, s.runMergeTier(context.Background()))
	require.Len(t, queryCalibHistory(t, db), 1, "second run with no new decided rows must not add a history row")
}

// ── Case 7: an invariant violation discards the tier and touches no state ─

func TestRunMergeTier_InvariantViolation_DiscardedStateUntouched(t *testing.T) {
	resetCalibState(t)
	db := testCalibDB(t)
	SetCalibrationDB(db)

	now := "2026-08-01 00:00:00"
	_, err := db.Exec(`INSERT INTO calibration_state(key,value,model_gen,updated_at) VALUES(?,?,?,?)`,
		"ClusterTightEps", 0.40, common.MLModelGen, now)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO calibration_state(key,value,model_gen,updated_at) VALUES(?,?,?,?)`,
		"ClusterMergeEps", 0.50, common.MLModelGen, now)
	require.NoError(t, err)

	persons := mergeBuildFixture(t, db, 10)
	cursor := 0
	mergeInsertDecidedPairs(t, db, persons, &cursor, 24, 0.20, "accepted")
	mergeInsertDecidedPairs(t, db, persons, &cursor, 1, 0.49, "accepted") // drives cut=0.49
	mergeInsertDecidedPairs(t, db, persons, &cursor, 6, 0.60, "rejected")

	s := NewFaceService(db)
	require.NoError(t, s.runMergeTier(context.Background()))

	hist := queryCalibHistory(t, db)
	require.Len(t, hist, 1)
	require.Equal(t, "invariant_violation", hist[0].Outcome)

	// State must be untouched: ClusterTightEps/ClusterMergeEps keep their
	// pre-seeded values.
	tightVal, ok := mustScanFloat(t, db, `SELECT value FROM calibration_state WHERE key='ClusterTightEps'`)
	require.True(t, ok)
	require.InDelta(t, 0.40, tightVal, 1e-9)
	mergeVal, ok := mustScanFloat(t, db, `SELECT value FROM calibration_state WHERE key='ClusterMergeEps'`)
	require.True(t, ok)
	require.InDelta(t, 0.50, mergeVal, 1e-9)
}

// ── Case 8: CAS single-flight -- an in-flight run makes maybeCalibrate a no-op ─

func TestMaybeCalibrate_CASAlreadyInFlight_ReturnsImmediately(t *testing.T) {
	resetCalibState(t)
	db := testCalibDB(t)
	SetCalibrationDB(db)
	config.Cfg = &config.Config{}

	s := NewFaceService(db)
	s.calibrating.Store(true)

	s.maybeCalibrate(context.Background())

	require.Empty(t, queryCalibHistory(t, db), "a CAS-blocked call must do zero tier work")
	require.True(t, s.calibrating.Load(), "the CAS-blocked call must not reset a flag it never set")
}

// ── maybeCalibrate's first-line guard (DB/config not wired) ──────────────

func TestMaybeCalibrate_NoDBWired_NoOp(t *testing.T) {
	resetCalibState(t)
	db := testCalibDB(t)
	// Deliberately NOT calling SetCalibrationDB: this is the state every
	// existing RunClustering/RunPipeline test in this package is in.
	config.Cfg = &config.Config{}

	s := NewFaceService(db)
	s.maybeCalibrate(context.Background())

	require.Empty(t, queryCalibHistory(t, db))
	require.False(t, s.calibrating.Load())
}

// ── twopass tier: bars + happy path (extra coverage beyond the 8 required cases) ──

// twoPassBuildCleanFixture inserts personCount named persons, each with
// facesPerPerson faces at that person's one-hot direction in a
// personCount-dimensional space: identical vectors within a person (cosDist
// 0) and orthogonal across persons (cosDist 1.0), so any T_tight/T_merge
// combo within the builtin profile's bands separates them perfectly.
// Timestamps are seconds apart, so every gap in twoPassGaps sees one moment.
func twoPassBuildCleanFixture(t *testing.T, db *sql.DB, personCount, facesPerPerson int) {
	t.Helper()
	base := time.Now().Add(-time.Hour)
	faceSeq := 0
	for p := 0; p < personCount; p++ {
		personID := fmt.Sprintf("np%02d", p)
		_, err := db.Exec(`INSERT INTO persons(id, name) VALUES(?, ?)`, personID, personID)
		require.NoError(t, err)

		vec := make([]float32, personCount)
		vec[p] = 1

		for f := 0; f < facesPerPerson; f++ {
			faceSeq++
			assetID := fmt.Sprintf("tpasset-%04d", faceSeq)
			faceID := fmt.Sprintf("tpface-%04d", faceSeq)
			ts := base.Add(time.Duration(faceSeq) * time.Second)
			_, err := db.Exec(`INSERT INTO assets(id, file_path, status, taken_at, indexed_at) VALUES(?,?,?,?,?)`,
				assetID, "/tp/"+assetID+".jpg", "indexed", ts, ts)
			require.NoError(t, err)
			_, err = db.Exec(`INSERT INTO face_detections(id, asset_id, bbox, embedding) VALUES(?,?,?,?)`,
				faceID, assetID, `{"x1":0,"y1":0,"x2":1,"y2":1}`, sqlite.SerializeFloat32(vec))
			require.NoError(t, err)
			_, err = db.Exec(`INSERT INTO face_person(face_id, person_id) VALUES(?,?)`, faceID, personID)
			require.NoError(t, err)
		}
	}
}

func TestRunTwoPassTier_InsufficientNamedTruth_HeldInsufficient(t *testing.T) {
	resetCalibState(t)
	db := testCalibDB(t)
	SetCalibrationDB(db)
	s := NewFaceService(db)

	require.NoError(t, s.runTwoPassTier(context.Background()))

	hist := queryCalibHistory(t, db)
	require.Len(t, hist, 1)
	require.Equal(t, "twopass", hist[0].Tier)
	require.Equal(t, "held_insufficient", hist[0].Outcome)
	require.Equal(t, 0, countCalibState(t, db))
}

func TestRunTwoPassTier_CleanSeparation_AppliedAllThreeKeys(t *testing.T) {
	resetCalibState(t)
	db := testCalibDB(t)
	SetCalibrationDB(db)
	twoPassBuildCleanFixture(t, db, 5, 62) // 310 faces >= TwoPassMinNamedFaces, 5 persons >= TwoPassMinNamedPersons
	s := NewFaceService(db)

	require.NoError(t, s.runTwoPassTier(context.Background()))

	hist := queryCalibHistory(t, db)
	require.Len(t, hist, 1)
	require.Equal(t, "twopass", hist[0].Tier)
	require.Contains(t, []string{"applied", "clamped"}, hist[0].Outcome)

	for _, key := range []string{"ClusterTightEps", "MomentGapMinutes", "ClusterMergeEps"} {
		_, ok := hist[0].NewValues[key]
		require.True(t, ok, "history new_values must record %s", key)
	}

	// MomentGapMinutes magnitude pin (Important review finding, fix round
	// 2). This is the single most dangerous seam in the feature: production
	// (runTwoPassTier) converts the selected combo's gap via
	// combo.Gap.Minutes() before handing it to boundAdjust. If a future
	// regression passed the raw time.Duration (nanoseconds) or a
	// seconds-scaled value instead of minutes, boundAdjust's step-limiting
	// would silently clamp that wild number into
	// current+/-Rules.MaxStepMinutes -- the tier would STILL report
	// "applied"/"clamped" with a value that happens to land inside
	// [15,120], and the "the key is present" assertion above would never
	// notice a wrong-but-plausible number. Presence alone cannot distinguish
	// a correct 45 from a coincidentally-in-band wrong one.
	//
	// So: recompute the expected magnitude independently, by calling the
	// exact same grid-scan primitives production calls (TwoPassGridScan /
	// SortTwoPassResults / SelectTwoPassCombo) but applying Gap.Minutes()
	// and boundAdjust ourselves here in the test, rather than trusting
	// runTwoPassTier's own arithmetic. A unit slip in production's
	// conversion would make this independently-derived expectation diverge
	// from the actual persisted result.
	faces, err := s.calibLoadFaces(context.Background())
	require.NoError(t, err)
	named, err := s.calibLoadNamedTruth(context.Background())
	require.NoError(t, err)
	profile := builtinFactoryProfile()
	results, ok := TwoPassGridScan(context.Background(), faces.vecs, faces.takenAt, faces.indexedAt, faces.idOf, named,
		profile.Thresholds["ClusterTightEps"], profile.Thresholds["ClusterMergeEps"])
	require.True(t, ok)
	SortTwoPassResults(results)
	combo, ok := SelectTwoPassCombo(results)
	require.True(t, ok)

	wantGapMinutes, adjOutcome := boundAdjust(calibCodeDefaults["MomentGapMinutes"], combo.Gap.Minutes(),
		profile.Thresholds["MomentGapMinutes"], profile.Rules.MaxStepMinutes, profile.Rules.MinDeltaMinutes)
	require.NotEqual(t, adjustHysteresis, adjOutcome, "fixture must produce a real adjustment for this pin to be meaningful")
	require.InDelta(t, wantGapMinutes, hist[0].NewValues["MomentGapMinutes"], 1e-9)
	// Sanity floor regardless of the exact number: a minutes-scale value
	// must land in the factory band -- a stray nanoseconds/seconds value
	// would blow this range by many orders of magnitude.
	require.GreaterOrEqual(t, hist[0].NewValues["MomentGapMinutes"], 15.0)
	require.LessOrEqual(t, hist[0].NewValues["MomentGapMinutes"], 120.0)

	// Pin the calibration_state row too, not just the history JSON --
	// resolveThreshold (and therefore momentGap()) reads calibration_state
	// in production, so a magnitude bug isolated to the state write (but not
	// the history write, or vice versa) must not slip past this test.
	stateVal, ok := mustScanFloat(t, db, `SELECT value FROM calibration_state WHERE key='MomentGapMinutes'`)
	require.True(t, ok)
	require.InDelta(t, wantGapMinutes, stateVal, 1e-9)
	var modelGen string
	require.NoError(t, db.QueryRow(`SELECT model_gen FROM calibration_state WHERE key='MomentGapMinutes'`).Scan(&modelGen))
	require.Equal(t, common.MLModelGen, modelGen)
}

// TestRunTwoPassTier_CancelledContext_NoHistoryRow is fix-round-1 case ②:
// bars are met (same fixture as the clean-separation happy path above,
// which without cancellation reaches "applied"/"clamped"), but ctx expires
// before TwoPassGridScan's grid loop finishes -- the tier must abort
// silently (no calibration_history row at all), distinguishing "the process
// is shutting down" from a genuine held_insufficient.
//
// A short deadline, not an already-cancelled context, is used deliberately:
// runTwoPassTier's own truth-loading DB queries (calibLoadFaces/
// calibLoadNamedTruth) share this same ctx and must be given the chance to
// actually complete first -- an already-cancelled ctx fails those with a
// "context canceled" error before ever reaching TwoPassGridScan, which
// would test the wrong thing (a DB-query error, not the grid-scan
// cancellation path this case targets). The deadline (20ms) sits comfortably
// above the handful-of-small-queries DB load (sub-millisecond) and well
// below the ~300ms this fixture's full grid scan takes uncancelled (see
// TestRunTwoPassTier_CleanSeparation_AppliedAllThreeKeys), so it reliably
// fires mid-scan rather than mid-load.
func TestRunTwoPassTier_CancelledContext_NoHistoryRow(t *testing.T) {
	resetCalibState(t)
	db := testCalibDB(t)
	SetCalibrationDB(db)
	twoPassBuildCleanFixture(t, db, 5, 62)
	s := NewFaceService(db)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	require.NoError(t, s.runTwoPassTier(ctx))

	require.Empty(t, queryCalibHistory(t, db), "cancellation must not write a history row")
	require.Equal(t, 0, countCalibState(t, db))
}

func TestMaybeCalibrate_NoConfig_NoOp(t *testing.T) {
	resetCalibState(t)
	db := testCalibDB(t)
	SetCalibrationDB(db)
	// config.Cfg stays nil (resetCalibState's default).

	s := NewFaceService(db)
	s.maybeCalibrate(context.Background())

	require.Empty(t, queryCalibHistory(t, db))
	require.False(t, s.calibrating.Load())
}
