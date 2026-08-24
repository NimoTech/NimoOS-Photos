package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// repeatVec returns n copies of v (same backing data is fine — Match never
// mutates its inputs), used to give one person several exemplars that all
// sit at the same constructed cosine distance from the query vector.
func repeatVec(v []float32, n int) [][]float32 {
	out := make([][]float32, n)
	for i := range out {
		out[i] = v
	}
	return out
}

// vecAtCosineDistance's target `dist` argument is a nominal request: the
// float32 round-trip through the vector components means the value cosDist
// actually reports back differs from the nominal target in the low bits
// (verified: vecAtCosineDistance(4, 0.4) -> cosDist == 0.3999999904632572,
// not 0.4). Every assertion below therefore compares against a `dist`
// computed live with cosDist on the exact same vectors, never against the
// nominal literal passed to vecAtCosineDistance — the branch's established
// float-exactness trick, carried into the two boundary-pin tests where the
// threshold itself is set to that live value so the boundary is hit
// bit-for-bit rather than approximated.

// TestMatchClearWinnerAuto: brief case ① — 5 nearest neighbors, 4 of them
// belong to the same person, median distance comfortably under autoDist
// (0.45) -> "auto".
func TestMatchClearWinnerAuto(t *testing.T) {
	query, near := vecAtCosineDistance(4, 0.4)
	_, far := vecAtCosineDistance(4, 0.8)
	wantDist := cosDist(query, near)

	ix := BuildExemplarIndex(map[string][][]float32{
		"p1": repeatVec(near, 4),
		"p2": repeatVec(far, 1),
	})

	person, dist, decision := ix.Match(query, "face-1", nil, 5, 3, 0.45, 0.6)

	require.Equal(t, "p1", person)
	require.Equal(t, wantDist, dist)
	require.Equal(t, "auto", decision)
}

// TestMatchGrayZoneSuggest: brief case ② — same shape as ① but the median
// distance lands strictly between autoDist (0.45) and suggestDist (0.6) ->
// "suggest".
func TestMatchGrayZoneSuggest(t *testing.T) {
	query, near := vecAtCosineDistance(4, 0.5)
	_, far := vecAtCosineDistance(4, 0.85)
	wantDist := cosDist(query, near)

	ix := BuildExemplarIndex(map[string][][]float32{
		"p1": repeatVec(near, 4),
		"p2": repeatVec(far, 1),
	})

	person, dist, decision := ix.Match(query, "face-1", nil, 5, 3, 0.45, 0.6)

	require.Equal(t, "p1", person)
	require.Equal(t, wantDist, dist)
	require.Equal(t, "suggest", decision)
}

// TestMatchSplitVoteNoStrictPluralityNone: brief case ③ — 5 nearest split
// 2/2/1 across three persons. Neither 2-vote person has a strict plurality
// (both tied for the top count), so the result is "none" even though the
// tied persons sit much closer (0.2/0.25) than the lone third (0.3) —
// vote-tie is checked before distance ever enters the decision.
func TestMatchSplitVoteNoStrictPluralityNone(t *testing.T) {
	query, vA := vecAtCosineDistance(4, 0.2)
	_, vB := vecAtCosineDistance(4, 0.25)
	_, vC := vecAtCosineDistance(4, 0.3)

	ix := BuildExemplarIndex(map[string][][]float32{
		"a": repeatVec(vA, 2),
		"b": repeatVec(vB, 2),
		"c": repeatVec(vC, 1),
	})

	person, dist, decision := ix.Match(query, "face-1", nil, 5, 1, 0.45, 0.6)

	require.Equal(t, "", person)
	require.Equal(t, 0.0, dist)
	require.Equal(t, "none", decision)
}

// TestMatchNegatedPersonExcludedRunnerUpWins: brief case ④ — the nearest
// person (a) would win outright, but (a, face-1) is a confirmed negative,
// so a's exemplars are dropped from the pool entirely before the k-nearest
// window is chosen; the runner-up (b) slides into the freed slots and wins
// instead.
func TestMatchNegatedPersonExcludedRunnerUpWins(t *testing.T) {
	query, vA := vecAtCosineDistance(4, 0.1)
	_, vB := vecAtCosineDistance(4, 0.3)
	_, vC := vecAtCosineDistance(4, 0.7)
	wantDist := cosDist(query, vB)

	ix := BuildExemplarIndex(map[string][][]float32{
		"a": repeatVec(vA, 4),
		"b": repeatVec(vB, 3),
		"c": repeatVec(vC, 2),
	})
	negatives := map[[2]string]bool{{"a", "face-1"}: true}

	person, dist, decision := ix.Match(query, "face-1", negatives, 5, 3, 0.45, 0.6)

	require.Equal(t, "b", person)
	require.Equal(t, wantDist, dist)
	require.Equal(t, "auto", decision)
}

// TestMatchEmptyIndexNone: brief case ⑤ — an index built from zero anchored
// persons always yields "none".
func TestMatchEmptyIndexNone(t *testing.T) {
	ix := BuildExemplarIndex(map[string][][]float32{})

	person, dist, decision := ix.Match([]float32{1, 0, 0, 0}, "face-1", nil, 5, 3, 0.45, 0.6)

	require.Equal(t, "", person)
	require.Equal(t, 0.0, dist)
	require.Equal(t, "none", decision)
}

// TestMatchSmallIndexVotesCountActual: brief case ⑥ — a single anchored
// person with only 2 exemplars (fewer than k=5) and nothing else in the
// index; both exemplars land in the (shrunk) top-k window, so votes are
// counted from what's actually there (2) rather than against the full k.
// minVotes is set to the floor (2) so the sub-k vote count still wins.
func TestMatchSmallIndexVotesCountActual(t *testing.T) {
	query, v := vecAtCosineDistance(4, 0.3)
	wantDist := cosDist(query, v)

	ix := BuildExemplarIndex(map[string][][]float32{
		"solo": repeatVec(v, 2),
	})

	person, dist, decision := ix.Match(query, "face-1", nil, 5, 2, 0.45, 0.6)

	require.Equal(t, "solo", person)
	require.Equal(t, wantDist, dist)
	require.Equal(t, "auto", decision)
}

// TestMatchBoundaryExactlyAutoDistIsAuto pins the <= autoDist edge.
// autoDist is set to the LIVE cosDist value between query and the winner's
// exemplars (not the nominal 0.45 fed into vecAtCosineDistance), so the
// winner's median distance and the threshold are bit-for-bit the same
// float64 — a real boundary hit, not an approximation.
func TestMatchBoundaryExactlyAutoDistIsAuto(t *testing.T) {
	query, v := vecAtCosineDistance(4, 0.45)
	_, far := vecAtCosineDistance(4, 0.9)
	autoDist := cosDist(query, v)

	ix := BuildExemplarIndex(map[string][][]float32{
		"p1": repeatVec(v, 3),
		"p2": repeatVec(far, 2),
	})

	person, dist, decision := ix.Match(query, "face-1", nil, 5, 3, autoDist, 0.6)

	require.Equal(t, "p1", person)
	require.Equal(t, autoDist, dist)
	require.Equal(t, "auto", decision)
}

// TestMatchBoundaryExactlySuggestDistIsNone pins the strict "< suggestDist"
// edge: suggestDist is set to the LIVE cosDist value for the winner's
// exemplars. dist >= suggestDist is "none" by spec, so hitting the
// boundary exactly must NOT fall into "suggest".
func TestMatchBoundaryExactlySuggestDistIsNone(t *testing.T) {
	query, v := vecAtCosineDistance(4, 0.6)
	_, far := vecAtCosineDistance(4, 0.95)
	suggestDist := cosDist(query, v)

	ix := BuildExemplarIndex(map[string][][]float32{
		"p1": repeatVec(v, 3),
		"p2": repeatVec(far, 2),
	})

	person, dist, decision := ix.Match(query, "face-1", nil, 5, 3, 0.45, suggestDist)

	require.Equal(t, "", person)
	require.Equal(t, 0.0, dist)
	require.Equal(t, "none", decision)
}

// TestMatchNegativeKClampsToNoneWithoutPanic is a regression test (folded in
// from the B-T3 review): a misconfigured negative k (e.g. AssignKNNK read as
// a garbage negative) must not panic on pool[:k] -- it's clamped to 0 up
// front, which always yields "none" since an empty top-k window can never
// reach minVotes.
func TestMatchNegativeKClampsToNoneWithoutPanic(t *testing.T) {
	query, v := vecAtCosineDistance(4, 0.1)

	ix := BuildExemplarIndex(map[string][][]float32{
		"p1": repeatVec(v, 4),
	})

	require.NotPanics(t, func() {
		person, dist, decision := ix.Match(query, "face-1", nil, -1, 3, 0.45, 0.6)
		require.Equal(t, "", person)
		require.Equal(t, 0.0, dist)
		require.Equal(t, "none", decision)
	})
}

// TestMatchOneExemplarPersonAutoJoinsWithoutCompetitor is the small-person-
// starvation fix's core case: a person with exactly ONE exemplar, with no
// competing person in the pool, must still win and auto-join when the query
// is close enough. Pre-fix, the raw minVotes=3 floor made this structurally
// impossible (1 vote can never reach 3), starving freshly-anchored small
// persons versus the old centroid snap. Post-fix, effectiveFloor =
// max(1, min(3, 1)) = 1, which winnerVotes(1) clears.
func TestMatchOneExemplarPersonAutoJoinsWithoutCompetitor(t *testing.T) {
	query, v := vecAtCosineDistance(4, 0.1)
	wantDist := cosDist(query, v)

	ix := BuildExemplarIndex(map[string][][]float32{
		"solo": repeatVec(v, 1),
	})

	person, dist, decision := ix.Match(query, "face-1", nil, 5, 3, 0.45, 0.6)

	require.Equal(t, "solo", person)
	require.Equal(t, wantDist, dist)
	require.Equal(t, "auto", decision)
}

// TestMatchTwoExemplarPersonAutoJoins is the two-exemplar analog: effectiveFloor
// = max(1, min(3, 2)) = 2, and both exemplars land in the k-window, so
// winnerVotes (2) clears it.
func TestMatchTwoExemplarPersonAutoJoins(t *testing.T) {
	query, v := vecAtCosineDistance(4, 0.1)
	wantDist := cosDist(query, v)

	ix := BuildExemplarIndex(map[string][][]float32{
		"solo": repeatVec(v, 2),
	})

	person, dist, decision := ix.Match(query, "face-1", nil, 5, 3, 0.45, 0.6)

	require.Equal(t, "solo", person)
	require.Equal(t, wantDist, dist)
	require.Equal(t, "auto", decision)
}

// TestMatchTwoExemplarPersonAutoJoinsWithCompetitorPresent confirms a weaker
// competing person in the index doesn't disturb the two-exemplar person's
// win: solo's plurality (2 votes) already beats the competitor's (1 vote)
// outright -- no tie -- and the floor actually checked is solo's own
// min(3, 2) = 2, not the raw minVotes = 3.
func TestMatchTwoExemplarPersonAutoJoinsWithCompetitorPresent(t *testing.T) {
	query, near := vecAtCosineDistance(4, 0.1)
	_, far := vecAtCosineDistance(4, 0.3)
	wantDist := cosDist(query, near)

	ix := BuildExemplarIndex(map[string][][]float32{
		"solo":  repeatVec(near, 2),
		"other": repeatVec(far, 1),
	})

	person, dist, decision := ix.Match(query, "face-1", nil, 5, 3, 0.45, 0.6)

	require.Equal(t, "solo", person)
	require.Equal(t, wantDist, dist)
	require.Equal(t, "auto", decision)
}

// TestMatchFloorIsNoOpWhenTotalExemplarsMeetsMinVotes explicitly regression-
// pins the fix's "unaffected" case: when a person's total exemplar count is
// already >= minVotes, effectiveFloor collapses to minVotes exactly, so
// this (and every pre-existing minVotes-based test above) behaves exactly
// as it did before the fix.
func TestMatchFloorIsNoOpWhenTotalExemplarsMeetsMinVotes(t *testing.T) {
	query, near := vecAtCosineDistance(4, 0.35)
	_, far := vecAtCosineDistance(4, 0.9)
	wantDist := cosDist(query, near)

	ix := BuildExemplarIndex(map[string][][]float32{
		"p1": repeatVec(near, 3), // total == minVotes exactly
		"p2": repeatVec(far, 2),
	})

	person, dist, decision := ix.Match(query, "face-1", nil, 5, 3, 0.45, 0.6)

	require.Equal(t, "p1", person)
	require.Equal(t, wantDist, dist)
	require.Equal(t, "auto", decision)
}

// TestMatchZeroExemplarPersonNeverWins keeps gap B frozen: a person entered
// into the index with zero vectors (the honest construction -- an empty
// slice, exactly what an all-gated-out SelectExemplars result collapses to)
// contributes no pool entries at all, so it can never be voted for, let
// alone win, regardless of the floor logic above.
func TestMatchZeroExemplarPersonNeverWins(t *testing.T) {
	query, v := vecAtCosineDistance(4, 0.05)

	ix := BuildExemplarIndex(map[string][][]float32{
		"empty": {},
		"real":  repeatVec(v, 3),
	})
	require.Equal(t, 0, ix.totalExemplars["empty"],
		"a person entered with zero vectors must have zero total exemplars")

	person, _, decision := ix.Match(query, "face-1", nil, 5, 3, 0.45, 0.6)

	require.Equal(t, "real", person)
	require.NotEqual(t, "empty", person)
	require.Equal(t, "auto", decision)
}

// TestMatchTwoOneExemplarPersonsTieIsFloorAgnostic: two persons with exactly
// one exemplar each, both landing in the k-window with exactly one vote
// apiece, must still tie -> "none" -- the tie check runs before the
// per-winner floor is even computed, so the fix cannot turn a genuine tie
// into a win just because each side's floor is as low as 1.
func TestMatchTwoOneExemplarPersonsTieIsFloorAgnostic(t *testing.T) {
	query, vA := vecAtCosineDistance(4, 0.1)
	_, vB := vecAtCosineDistance(4, 0.15)

	ix := BuildExemplarIndex(map[string][][]float32{
		"a": repeatVec(vA, 1),
		"b": repeatVec(vB, 1),
	})

	person, dist, decision := ix.Match(query, "face-1", nil, 5, 3, 0.45, 0.6)

	require.Equal(t, "", person)
	require.Equal(t, 0.0, dist)
	require.Equal(t, "none", decision)
}

// TestMatchDeterministic: the same inputs, including an index rebuilt from
// an equivalent (but freshly-allocated) map, must produce byte-identical
// results across repeated calls — no map-iteration-order leakage anywhere
// in the nearest-k selection or the vote tally.
func TestMatchDeterministic(t *testing.T) {
	query, vA := vecAtCosineDistance(4, 0.3)
	_, vB := vecAtCosineDistance(4, 0.5)
	_, vC := vecAtCosineDistance(4, 0.7)

	build := func() *exemplarIndex {
		return BuildExemplarIndex(map[string][][]float32{
			"a": repeatVec(vA, 3),
			"b": repeatVec(vB, 3),
			"c": repeatVec(vC, 3),
		})
	}

	p1, d1, dec1 := build().Match(query, "face-1", nil, 5, 2, 0.45, 0.6)
	p2, d2, dec2 := build().Match(query, "face-1", nil, 5, 2, 0.45, 0.6)

	require.Equal(t, p1, p2)
	require.Equal(t, d1, d2)
	require.Equal(t, dec1, dec2)
}
