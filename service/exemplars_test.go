package service

import (
	"database/sql"
	"fmt"
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

// nf (shorthand for a valid sql.NullFloat64) is already defined in
// cover_quality_signals_internal_test.go — reused here.

// TestSelectExemplarsGateRejectsLowSharpness: a face at the sharpness floor
// minus a hair (0.2 against a 0.3 threshold), full marks otherwise, must not
// be selected — the hard quality gate is a hard floor, not a weighted score.
func TestSelectExemplarsGateRejectsLowSharpness(t *testing.T) {
	cands := []exemplarCandidate{
		{FaceID: "good", Vec: []float32{1, 0, 0, 0}, Score: nf(1.0), Frontality: nf(1.0), Sharpness: nf(1.0)},
		{FaceID: "blurry", Vec: []float32{0, 1, 0, 0}, Score: nf(1.0), Frontality: nf(1.0), Sharpness: nf(0.2)},
	}

	got := SelectExemplars(cands, 2, 0.75, 0.5, 0.3)

	require.Equal(t, []string{"good"}, got)
}

// TestSelectExemplarsGateRejectsNullSignal: a NULL quality signal always
// fails the gate, even when every threshold is 0 — pre-gen4 rows without
// detector signals must never become exemplars.
func TestSelectExemplarsGateRejectsNullSignal(t *testing.T) {
	cands := []exemplarCandidate{
		{FaceID: "signaled", Vec: []float32{1, 0, 0, 0}, Score: nf(0.0), Frontality: nf(0.0), Sharpness: nf(0.0)},
		{FaceID: "null-score", Vec: []float32{0, 1, 0, 0}, Score: sql.NullFloat64{}, Frontality: nf(1.0), Sharpness: nf(1.0)},
	}

	got := SelectExemplars(cands, 2, 0, 0, 0)

	require.Equal(t, []string{"signaled"}, got)
}

// TestSelectExemplarsConfirmedSeededFirst: with 30 gated candidates, the
// confirmed ones must always survive into the result regardless of what
// farthest-point sampling would otherwise pick.
func TestSelectExemplarsConfirmedSeededFirst(t *testing.T) {
	full := nf(1.0)
	var cands []exemplarCandidate
	confirmedIDs := []string{"conf-a", "conf-b", "conf-c"}
	for _, id := range confirmedIDs {
		cands = append(cands, exemplarCandidate{
			FaceID: id, Vec: []float32{1, 0, 0, 0}, Score: full, Frontality: full, Sharpness: full, Confirmed: true,
		})
	}
	for i := 0; i < 27; i++ {
		cands = append(cands, exemplarCandidate{
			FaceID: fmt.Sprintf("free-%02d", i), Vec: []float32{1, 0, 0, 0}, Score: full, Frontality: full, Sharpness: full,
		})
	}
	require.Len(t, cands, 30)

	got := SelectExemplars(cands, 5, 0.75, 0.5, 0.3)

	require.Len(t, got, 5)
	for _, id := range confirmedIDs {
		require.Contains(t, got, id)
	}
}

// TestSelectExemplarsConfirmedOverflowTakesTopScored: when the confirmed set
// alone already meets or exceeds cap, the result is confirmed-only, ranked
// by score descending then FaceID ascending (documented deterministic
// preference) — diversity sampling never even runs.
func TestSelectExemplarsConfirmedOverflowTakesTopScored(t *testing.T) {
	full := nf(1.0)
	cands := []exemplarCandidate{
		{FaceID: "conf-mid", Vec: []float32{1, 0, 0, 0}, Score: nf(0.90), Frontality: full, Sharpness: full, Confirmed: true},
		{FaceID: "conf-top", Vec: []float32{0, 1, 0, 0}, Score: nf(0.99), Frontality: full, Sharpness: full, Confirmed: true},
		{FaceID: "conf-low", Vec: []float32{0, 0, 1, 0}, Score: nf(0.80), Frontality: full, Sharpness: full, Confirmed: true},
		{FaceID: "conf-extra", Vec: []float32{0, 0, 0, 1}, Score: nf(0.95), Frontality: full, Sharpness: full, Confirmed: true},
		// Not confirmed, extremely diverse — must NOT displace a confirmed
		// face once confirmed count >= cap.
		{FaceID: "free-diverse", Vec: []float32{-1, 0, 0, 0}, Score: nf(1.0), Frontality: full, Sharpness: full},
	}

	got := SelectExemplars(cands, 3, 0.75, 0.5, 0.3)

	require.Equal(t, []string{"conf-top", "conf-extra", "conf-mid"}, got)
}

// TestSelectExemplarsDiversitySampling: 20 near-duplicate gated faces
// clustered around one direction plus 4 gated faces spread far apart (and
// far from the cluster). With cap=8, farthest-point sampling must pull in
// all 4 far faces instead of piling onto the tight cluster's mode.
func TestSelectExemplarsDiversitySampling(t *testing.T) {
	const dim = 6
	near := make([]float32, dim)
	near[0] = 1.0

	cosA := 0.2
	sinA := math.Sqrt(1 - cosA*cosA)

	var cands []exemplarCandidate
	for i := 0; i < 20; i++ {
		cands = append(cands, exemplarCandidate{
			FaceID: fmt.Sprintf("near-%02d", i), Vec: near, Score: nf(1.0), Frontality: nf(1.0), Sharpness: nf(1.0),
		})
	}

	farIDs := make([]string, 4)
	for k := 0; k < 4; k++ {
		v := make([]float32, dim)
		v[0] = float32(cosA)
		v[k+1] = float32(sinA)
		id := fmt.Sprintf("far-%d", k)
		farIDs[k] = id
		cands = append(cands, exemplarCandidate{
			FaceID: id, Vec: v, Score: nf(0.85), Frontality: nf(1.0), Sharpness: nf(1.0),
		})
	}

	got := SelectExemplars(cands, 8, 0.75, 0.5, 0.3)

	require.Len(t, got, 8)
	for _, id := range farIDs {
		require.Contains(t, got, id)
	}
}

// TestSelectExemplarsCapBoundaryUnderCapReturnsAll: fewer gated candidates
// than cap means everything gated gets selected, no sampling needed.
func TestSelectExemplarsCapBoundaryUnderCapReturnsAll(t *testing.T) {
	full := nf(1.0)
	cands := []exemplarCandidate{
		{FaceID: "a", Vec: []float32{1, 0, 0, 0}, Score: full, Frontality: full, Sharpness: full},
		{FaceID: "b", Vec: []float32{0, 1, 0, 0}, Score: full, Frontality: full, Sharpness: full},
		{FaceID: "c", Vec: []float32{0, 0, 1, 0}, Score: full, Frontality: full, Sharpness: full},
	}

	got := SelectExemplars(cands, 10, 0.75, 0.5, 0.3)

	require.Len(t, got, 3)
	require.Contains(t, got, "a")
	require.Contains(t, got, "b")
	require.Contains(t, got, "c")
}

// TestSelectExemplarsAllFailGateReturnsEmpty: an all-fail candidate set is a
// legal outcome — the matcher skips exemplar-less persons (see B3) — and
// must yield an empty (not nil-panicking, not error) result.
func TestSelectExemplarsAllFailGateReturnsEmpty(t *testing.T) {
	cands := []exemplarCandidate{
		{FaceID: "a", Vec: []float32{1, 0, 0, 0}, Score: nf(0.1), Frontality: nf(0.1), Sharpness: nf(0.1)},
		{FaceID: "b", Vec: []float32{0, 1, 0, 0}, Score: sql.NullFloat64{}, Frontality: nf(1.0), Sharpness: nf(1.0)},
	}

	got := SelectExemplars(cands, 5, 0.75, 0.5, 0.3)

	require.Empty(t, got)
}

// TestSelectExemplarsDeterministicOrder: identical input must produce an
// identical output slice (same elements, same order) on every call — the
// selection has no randomness or map-iteration-order dependence.
func TestSelectExemplarsDeterministicOrder(t *testing.T) {
	const dim = 6
	near := make([]float32, dim)
	near[0] = 1.0
	cosA := 0.2
	sinA := math.Sqrt(1 - cosA*cosA)

	build := func() []exemplarCandidate {
		var cands []exemplarCandidate
		for i := 0; i < 20; i++ {
			cands = append(cands, exemplarCandidate{
				FaceID: fmt.Sprintf("near-%02d", i), Vec: near, Score: nf(1.0), Frontality: nf(1.0), Sharpness: nf(1.0),
			})
		}
		for k := 0; k < 4; k++ {
			v := make([]float32, dim)
			v[0] = float32(cosA)
			v[k+1] = float32(sinA)
			cands = append(cands, exemplarCandidate{
				FaceID: fmt.Sprintf("far-%d", k), Vec: v, Score: nf(0.85), Frontality: nf(1.0), Sharpness: nf(1.0),
			})
		}
		return cands
	}

	got1 := SelectExemplars(build(), 8, 0.75, 0.5, 0.3)
	got2 := SelectExemplars(build(), 8, 0.75, 0.5, 0.3)

	require.Equal(t, got1, got2)
}

// The following three tests pin regressions flagged in Task 2's review
// (carried over into Task 3, see progress.md): the hard quality gate must
// never be bypassed by Confirmed status (anti-drift invariant), the gate
// must be a >= comparison (not >) at the threshold, and cap<=0 is a no-op
// that returns nil rather than panicking or falling through to cap=0.

// TestSelectExemplarsConfirmedBelowGateExcluded: a Confirmed face that
// fails the hard quality gate must NOT be selected. Confirmed-first
// seeding only applies to faces that already passed the gate — Confirmed
// is never itself a bypass, otherwise a single bad manual confirmation
// could drift a person's exemplar set toward low-quality faces.
func TestSelectExemplarsConfirmedBelowGateExcluded(t *testing.T) {
	cands := []exemplarCandidate{
		{FaceID: "confirmed-blurry", Vec: []float32{1, 0, 0, 0}, Score: nf(1.0), Frontality: nf(1.0), Sharpness: nf(0.1), Confirmed: true},
		{FaceID: "unconfirmed-good", Vec: []float32{0, 1, 0, 0}, Score: nf(1.0), Frontality: nf(1.0), Sharpness: nf(1.0)},
	}

	got := SelectExemplars(cands, 5, 0.75, 0.5, 0.3)

	require.Equal(t, []string{"unconfirmed-good"}, got)
}

// TestSelectExemplarsGateAtThresholdPasses: a face whose score, frontality,
// and sharpness are each exactly equal to the threshold (not above it)
// must pass the gate — the comparison is >=, not a strict >.
func TestSelectExemplarsGateAtThresholdPasses(t *testing.T) {
	cands := []exemplarCandidate{
		{FaceID: "at-threshold", Vec: []float32{1, 0, 0, 0}, Score: nf(0.75), Frontality: nf(0.5), Sharpness: nf(0.3)},
	}

	got := SelectExemplars(cands, 5, 0.75, 0.5, 0.3)

	require.Equal(t, []string{"at-threshold"}, got)
}

// TestSelectExemplarsCapZeroOrNegativeReturnsNil: cap<=0 is a no-op — nil,
// not an empty-but-non-nil slice, not a panic — checked before the gate
// even runs.
func TestSelectExemplarsCapZeroOrNegativeReturnsNil(t *testing.T) {
	cands := []exemplarCandidate{
		{FaceID: "a", Vec: []float32{1, 0, 0, 0}, Score: nf(1.0), Frontality: nf(1.0), Sharpness: nf(1.0)},
	}

	require.Nil(t, SelectExemplars(cands, 0, 0.75, 0.5, 0.3))
	require.Nil(t, SelectExemplars(cands, -1, 0.75, 0.5, 0.3))
}
