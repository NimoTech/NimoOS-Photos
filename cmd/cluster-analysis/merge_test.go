package main

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// ── Report-formatting tests ─────────────────────────────────────────────────
//
// The truth-loading / cut-point core these tests build on lives in
// service/calibrate_merge.go and service/calibrate_merge_test.go (shared
// with the in-service calibration runner); the tests here only exercise
// report-formatting code that stayed in this CLI package
// (computeMergeDistStats/printMergeDistribution and runMerge's full printed
// output).

func mergeOpenFixtureDB(t *testing.T, name string) *sql.DB {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), name))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	return db
}

func mergeInsertPerson(t *testing.T, db *sql.DB, personID string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO persons(id, name) VALUES(?, '')`, personID)
	require.NoError(t, err)
}

// mergeInsertSuggestion inserts one merge_suggestions row honoring the
// table's CHECK(person_a < person_b): callers pass the two person IDs in
// any order, this orders them canonically before insert.
func mergeInsertSuggestion(t *testing.T, db *sql.DB, personX, personY string, dist float64, status string) {
	t.Helper()
	pa, pb := personX, personY
	if pa > pb {
		pa, pb = pb, pa
	}
	var decidedAt interface{}
	if status != "open" {
		decidedAt = "2026-08-23 00:00:00"
	}
	_, err := db.Exec(`
		INSERT INTO merge_suggestions(id, person_a, person_b, into_is_a, dist, status, created_at, decided_at)
		VALUES(?, ?, ?, 1, ?, ?, CURRENT_TIMESTAMP, ?)`,
		uuid.NewString(), pa, pb, dist, status, decidedAt)
	require.NoError(t, err)
}

// mergeBulkFixture inserts nPairs decided merge_suggestions rows split
// between accepted (low dist) and rejected (high dist), each on a fresh
// pair of persons, so nPairs pairs contribute 2*nPairs distinct persons --
// used to hit (or stay under) the insufficient-data bars without caring
// about exact recommendation numbers.
func mergeBulkFixture(t *testing.T, db *sql.DB, nAccepted, nRejected int) {
	t.Helper()
	seq := 0
	next := func() string { seq++; return uuid.NewString() }
	for i := 0; i < nAccepted; i++ {
		pa, pb := next(), next()
		mergeInsertPerson(t, db, pa)
		mergeInsertPerson(t, db, pb)
		mergeInsertSuggestion(t, db, pa, pb, 0.30, "accepted")
	}
	for i := 0; i < nRejected; i++ {
		pa, pb := next(), next()
		mergeInsertPerson(t, db, pa)
		mergeInsertPerson(t, db, pb)
		mergeInsertSuggestion(t, db, pa, pb, 0.80, "rejected")
	}
}

// TestMergeMode_SmokeFixture_KnownDistributionsAndCutPoint exercises the
// full -mode merge report against a small hand-verifiable fixture: 3
// accepted at 0.60/0.62/0.65 and 1 rejected at 0.63 -- the same case ① from
// service/calibrate_merge_test.go, so the printed RECOMMENDED line's value
// (0.6200) is asserted against the exact same known-good cut point. Padded
// with mergeBulkFixture filler rows (accepted=0.30, rejected=0.80 -- both
// clear of the four known values above, so they cannot change min(rejected)
// or the winning accepted dist) to clear every insufficient-data bar, so the
// warning-absence assertion below is exercising the "sufficient" branch of
// the report, not merely small-fixture noise.
func TestMergeMode_SmokeFixture_KnownDistributionsAndCutPoint(t *testing.T) {
	db := mergeOpenFixtureDB(t, "merge-smoke.db")

	pA, pB, pC, pD := "pA", "pB", "pC", "pD"
	for _, p := range []string{pA, pB, pC, pD} {
		mergeInsertPerson(t, db, p)
	}
	mergeInsertSuggestion(t, db, pA, pB, 0.60, "accepted")
	mergeInsertSuggestion(t, db, pA, pC, 0.62, "accepted")
	mergeInsertSuggestion(t, db, pA, pD, 0.65, "accepted")
	mergeInsertSuggestion(t, db, pB, pC, 0.63, "rejected")
	mergeBulkFixture(t, db, 10, 20) // -> accepted=13, rejected=21, decided=34, persons>=64

	truth, err := service.LoadMergeTruth(db)
	require.NoError(t, err)
	require.Subset(t, truth.AcceptedDists, []float64{0.60, 0.62, 0.65})
	require.Subset(t, truth.RejectedDists, []float64{0.63})
	require.False(t, service.MergeInsufficient(truth))

	stats := computeMergeDistStats(truth.AcceptedDists)
	require.Equal(t, 13, stats.Count)

	var buf strings.Builder
	runMerge(&buf, db)
	out := buf.String()
	require.NotContains(t, out, "INSUFFICIENT DATA")
	require.Contains(t, out, "RECOMMENDED: ClusterMergeEps=0.6200")
}

// TestMergeMode_InsufficientDataWarning_AppearsBelowBarsDisappearsAbove
// mirrors TestKNNMode_InsufficientDataWarning_AppearsBelowBarsDisappearsAbove:
// below every T-merge bar the warning must appear; clearing every bar makes
// it disappear.
func TestMergeMode_InsufficientDataWarning_AppearsBelowBarsDisappearsAbove(t *testing.T) {
	// Below every bar: 5 accepted (< 10), 4 rejected (< 5), decided=9 (< 30),
	// persons=18 (>= 8, deliberately NOT the bar being tested here).
	dbSmall := mergeOpenFixtureDB(t, "merge-small.db")
	mergeBulkFixture(t, dbSmall, 5, 4)
	var bufSmall strings.Builder
	runMerge(&bufSmall, dbSmall)
	require.Contains(t, bufSmall.String(), "INSUFFICIENT DATA")

	// Above every bar: 20 accepted (>= 10), 15 rejected (>= 5), decided=35
	// (>= 30), persons=70 (>= 8).
	dbBig := mergeOpenFixtureDB(t, "merge-big.db")
	mergeBulkFixture(t, dbBig, 20, 15)
	var bufBig strings.Builder
	runMerge(&bufBig, dbBig)
	require.NotContains(t, bufBig.String(), "INSUFFICIENT DATA")
}

// TestMergeMode_NoValidCutPoint_PrintsNoValidCutMessage covers case ② at the
// report level: when the smallest rejected dist is below every accepted
// dist, runMerge must print the "no valid cut point" message, not a bogus
// RECOMMENDED line.
func TestMergeMode_NoValidCutPoint_PrintsNoValidCutMessage(t *testing.T) {
	db := mergeOpenFixtureDB(t, "merge-novalid.db")
	pA, pB, pC := "pA", "pB", "pC"
	for _, p := range []string{pA, pB, pC} {
		mergeInsertPerson(t, db, p)
	}
	mergeInsertSuggestion(t, db, pA, pB, 0.60, "accepted")
	mergeInsertSuggestion(t, db, pA, pC, 0.10, "rejected")

	var buf strings.Builder
	runMerge(&buf, db)
	out := buf.String()
	require.Contains(t, out, "NO valid cut point")
	require.NotContains(t, out, "RECOMMENDED:")
}
