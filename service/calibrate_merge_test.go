package service

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// ── Pure-function tests: MergeCutPoint ──────────────────────────────────────

// TestMergeCutPoint_LargestAcceptedBelowMinRejected is case ① from the
// requirement: accepted={0.60,0.62,0.65}, rejected={0.63} -> cut=0.62 (the
// largest accepted dist strictly below the smallest rejected dist; 0.65 is
// NOT below 0.63, so it's excluded).
func TestMergeCutPoint_LargestAcceptedBelowMinRejected(t *testing.T) {
	truth := MergeTruth{
		AcceptedDists: []float64{0.60, 0.62, 0.65},
		RejectedDists: []float64{0.63},
	}
	cut, ok := MergeCutPoint(truth)
	require.True(t, ok)
	require.InDelta(t, 0.62, cut, 1e-9)
}

// TestMergeCutPoint_MinRejectedBelowAllAccepted is case ②: the smallest
// rejected dist is below every accepted dist -> no accepted dist qualifies,
// ok=false.
func TestMergeCutPoint_MinRejectedBelowAllAccepted(t *testing.T) {
	truth := MergeTruth{
		AcceptedDists: []float64{0.60, 0.62, 0.65},
		RejectedDists: []float64{0.10},
	}
	_, ok := MergeCutPoint(truth)
	require.False(t, ok)
}

// TestMergeCutPoint_EmptyRejected is case ③: RejectedDists is empty
// (unbounded above) -> ok=false.
func TestMergeCutPoint_EmptyRejected(t *testing.T) {
	truth := MergeTruth{
		AcceptedDists: []float64{0.60, 0.62},
		RejectedDists: nil,
	}
	_, ok := MergeCutPoint(truth)
	require.False(t, ok)
}

// TestMergeCutPoint_EmptyAccepted is case ④: AcceptedDists is empty ->
// ok=false.
func TestMergeCutPoint_EmptyAccepted(t *testing.T) {
	truth := MergeTruth{
		AcceptedDists: nil,
		RejectedDists: []float64{0.63},
	}
	_, ok := MergeCutPoint(truth)
	require.False(t, ok)
}

// ── MergeInsufficient: bars, with exact boundary pins (case ⑤) ─────────────

// TestMergeInsufficient_29DecidedIsInsufficient pins the "just under the
// decided-count bar" boundary: 9 accepted + 5 rejected + 15 more accepted
// (irrelevant split, only the totals matter to this bar) = 29 decided total,
// which alone is below MergeMinDecided=30 regardless of how the per-status
// and per-person bars land.
func TestMergeInsufficient_29DecidedIsInsufficient(t *testing.T) {
	accepted := make([]float64, 15)
	for i := range accepted {
		accepted[i] = 0.1 + float64(i)*0.001
	}
	rejected := make([]float64, 14)
	for i := range rejected {
		rejected[i] = 0.9 + float64(i)*0.001
	}
	persons := map[string]bool{}
	for i := 0; i < 8; i++ {
		persons[uuid.NewString()] = true
	}
	truth := MergeTruth{AcceptedDists: accepted, RejectedDists: rejected, DistinctPersons: persons}
	require.Equal(t, 29, len(truth.AcceptedDists)+len(truth.RejectedDists))
	require.True(t, MergeInsufficient(truth))
}

// TestMergeInsufficient_30DecidedEachBarExactlyMet pins the boundary where
// every bar is exactly met (not merely cleared): accepted=10 (=MergeMinAccepted),
// rejected=20 (decided total=30=MergeMinDecided), persons=8 (=MergeMinPersons)
// -> sufficient.
func TestMergeInsufficient_30DecidedEachBarExactlyMet(t *testing.T) {
	accepted := make([]float64, MergeMinAccepted) // 10
	for i := range accepted {
		accepted[i] = 0.1 + float64(i)*0.001
	}
	rejected := make([]float64, MergeMinDecided-MergeMinAccepted) // 20, clears MergeMinRejected=5
	for i := range rejected {
		rejected[i] = 0.9 + float64(i)*0.001
	}
	persons := map[string]bool{}
	for i := 0; i < MergeMinPersons; i++ { // 8
		persons[uuid.NewString()] = true
	}
	truth := MergeTruth{AcceptedDists: accepted, RejectedDists: rejected, DistinctPersons: persons}
	require.Equal(t, MergeMinDecided, len(truth.AcceptedDists)+len(truth.RejectedDists))
	require.False(t, MergeInsufficient(truth))
}

// TestMergeInsufficient_EachBarIndividually confirms each of the four bars
// (decided>=30, accepted>=10, rejected>=5, persons>=8) gates independently.
func TestMergeInsufficient_EachBarIndividually(t *testing.T) {
	mkPersons := func(n int) map[string]bool {
		m := map[string]bool{}
		for i := 0; i < n; i++ {
			m[uuid.NewString()] = true
		}
		return m
	}
	mkDists := func(n int) []float64 {
		d := make([]float64, n)
		for i := range d {
			d[i] = 0.1 + float64(i)*0.001
		}
		return d
	}

	// Baseline: exactly sufficient on every bar.
	base := MergeTruth{
		AcceptedDists:   mkDists(MergeMinAccepted),                   // 10
		RejectedDists:   mkDists(MergeMinDecided - MergeMinAccepted), // 20
		DistinctPersons: mkPersons(MergeMinPersons),                  // 8
	}
	require.False(t, MergeInsufficient(base), "baseline clears every bar")

	// Accepted just under its own bar (9), decided total drops to 29 too.
	t1 := base
	t1.AcceptedDists = mkDists(MergeMinAccepted - 1)
	require.True(t, MergeInsufficient(t1), "accepted just under its bar")

	// Rejected just under its own bar (4), keep decided total at/above 30
	// by padding accepted so only the rejected bar is being exercised.
	t2 := MergeTruth{
		AcceptedDists:   mkDists(MergeMinDecided - (MergeMinRejected - 1)),
		RejectedDists:   mkDists(MergeMinRejected - 1),
		DistinctPersons: mkPersons(MergeMinPersons),
	}
	require.True(t, MergeInsufficient(t2), "rejected just under its bar")

	// Persons just under its bar (7), decided counts otherwise ample.
	t3 := base
	t3.DistinctPersons = mkPersons(MergeMinPersons - 1)
	require.True(t, MergeInsufficient(t3), "persons just under its bar")

	// Decided total just under its bar (29) even though per-status splits
	// individually clear their own bars (accepted=15, rejected=14).
	t4 := MergeTruth{
		AcceptedDists:   mkDists(15),
		RejectedDists:   mkDists(14),
		DistinctPersons: mkPersons(MergeMinPersons),
	}
	require.True(t, MergeInsufficient(t4), "decided total just under its bar despite per-status bars clearing")
}

// ── Fixture DB test: LoadMergeTruth (case ⑥) ────────────────────────────────

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
// table's CHECK(person_a < person_b): callers pass the two person IDs in any
// order, this orders them canonically before insert.
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

// TestLoadMergeTruth_OnlyDecidedRowsDistinctPersonsUnion is case ⑥:
// LoadMergeTruth must only load decided (accepted/rejected) rows -- an
// 'open' row must be entirely excluded, both from the dist slices and from
// DistinctPersons -- and DistinctPersons must be the union of person_a and
// person_b across the decided rows only.
func TestLoadMergeTruth_OnlyDecidedRowsDistinctPersonsUnion(t *testing.T) {
	db := mergeOpenFixtureDB(t, "merge-loading.db")

	pA, pB, pC, pD, pE, pF := "pA", "pB", "pC", "pD", "pE", "pF"
	for _, p := range []string{pA, pB, pC, pD, pE, pF} {
		mergeInsertPerson(t, db, p)
	}

	mergeInsertSuggestion(t, db, pA, pB, 0.62, "accepted")
	mergeInsertSuggestion(t, db, pC, pD, 0.63, "rejected")
	mergeInsertSuggestion(t, db, pE, pF, 0.99, "open") // must be excluded entirely

	truth, err := LoadMergeTruth(db)
	require.NoError(t, err)

	require.Equal(t, []float64{0.62}, truth.AcceptedDists)
	require.Equal(t, []float64{0.63}, truth.RejectedDists)

	require.Len(t, truth.DistinctPersons, 4)
	require.True(t, truth.DistinctPersons[pA])
	require.True(t, truth.DistinctPersons[pB])
	require.True(t, truth.DistinctPersons[pC])
	require.True(t, truth.DistinctPersons[pD])
	require.False(t, truth.DistinctPersons[pE], "pE only appears in an open row, must not count")
	require.False(t, truth.DistinctPersons[pF], "pF only appears in an open row, must not count")
}
