package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ── PickMajorityLabel: moved from cmd/cluster-analysis/majority_test.go ────

// TestPickMajorityLabel_TieResolvesPessimistically reproduces the reviewer's
// finding: cluster 10 and cluster 20 each get exactly one of this person's
// faces (a count tie), but cluster 10 has 3 total members and cluster 20 has
// only 1. A naive "first seen in map iteration wins" reduction would flip
// between purity=1.0 (cluster 20 wins) and purity=0.333 (cluster 10 wins)
// depending on Go's randomized map iteration order. The fix must
// deterministically prefer the larger cluster on a count tie, so purity is
// always the pessimistic 1/3, never the optimistic (and unearned) 1/1.
// Looped to prove stability across many calls, since a single lucky pass
// would not catch iteration-order dependence.
func TestPickMajorityLabel_TieResolvesPessimistically(t *testing.T) {
	labelCount := map[int]int{10: 1, 20: 1}
	sizes := map[int]int{10: 3, 20: 1}

	for i := 0; i < 20; i++ {
		bestLbl, bestCnt := PickMajorityLabel(labelCount, sizes)
		if bestLbl != 10 {
			t.Fatalf("iteration %d: bestLbl = %d, want 10 (larger cluster must win the count tie)", i, bestLbl)
		}
		if bestCnt != 1 {
			t.Fatalf("iteration %d: bestCnt = %d, want 1", i, bestCnt)
		}
		purity := float64(bestCnt) / float64(sizes[bestLbl])
		const wantPurity = 1.0 / 3.0
		if purity != wantPurity {
			t.Fatalf("iteration %d: purity = %v, want %v (deterministic pessimistic tie-break)", i, purity, wantPurity)
		}
	}
}

// TestPickMajorityLabel_CountBreaksTieFirst confirms an unambiguous count
// difference (no tie at all) is decided on count alone, regardless of
// either cluster's size.
func TestPickMajorityLabel_CountBreaksTieFirst(t *testing.T) {
	labelCount := map[int]int{1: 5, 2: 2}
	sizes := map[int]int{1: 10, 2: 2}
	bestLbl, bestCnt := PickMajorityLabel(labelCount, sizes)
	if bestLbl != 1 || bestCnt != 5 {
		t.Fatalf("got (%d,%d), want (1,5)", bestLbl, bestCnt)
	}
}

// TestPickMajorityLabel_FullTieBreaksToSmallerLabel confirms that when both
// count AND size tie, the tie-break falls through to the smaller label
// number -- pure, arbitrary, but fixed determinism, never map-iteration
// luck.
func TestPickMajorityLabel_FullTieBreaksToSmallerLabel(t *testing.T) {
	labelCount := map[int]int{7: 2, 3: 2}
	sizes := map[int]int{7: 4, 3: 4}
	for i := 0; i < 20; i++ {
		bestLbl, _ := PickMajorityLabel(labelCount, sizes)
		if bestLbl != 3 {
			t.Fatalf("iteration %d: bestLbl = %d, want 3 (smaller label wins a full count+size tie)", i, bestLbl)
		}
	}
}

// TestPickMajorityLabel_Empty confirms the documented empty-input contract.
func TestPickMajorityLabel_Empty(t *testing.T) {
	bestLbl, bestCnt := PickMajorityLabel(map[int]int{}, map[int]int{})
	if bestLbl != -1 || bestCnt != 0 {
		t.Fatalf("got (%d,%d), want (-1,0)", bestLbl, bestCnt)
	}
}

// ── EvalTwoPassCombo / SortTwoPassResults ───────────────────────────────────

// TestEvalTwoPassCombo_PurityAndFragCount builds a 3-cluster label
// assignment by hand and checks the purity/fragCount arithmetic directly:
// person A's 2 faces are split across two clusters (a fragment, and the
// smaller one is a false majority pick only if it wins the count -- here it
// doesn't: A's majority cluster (label 0, size 2) is fully A, so purity for
// A is 1.0), person B's single face sits alone in a pure cluster (purity
// 1.0), for a mean purity of 1.0 and a fragCount of 1 (A's one extra
// cluster).
func TestEvalTwoPassCombo_PurityAndFragCount(t *testing.T) {
	// faces: 0,1 -> label 0 (both A); 2 -> label 1 (A, fragment); 3 -> label 2 (B)
	labels := []int{0, 0, 1, 2}
	idOf := map[string]int{"a1": 0, "a2": 1, "a3": 2, "b1": 3}
	named := []NamedTruth{
		{PersonID: "A", FaceIDs: []string{"a1", "a2", "a3"}},
		{PersonID: "B", FaceIDs: []string{"b1"}},
	}

	combo := EvalTwoPassCombo(labels, 60*time.Minute, 0.35, 0.55, idOf, named)
	require.Equal(t, 3, combo.NumClusters)
	require.Equal(t, 2, combo.MaxSize)
	require.InDelta(t, 1.0, combo.Purity, 1e-9)
	require.Equal(t, 1, combo.FragCount)
}

// TestSortTwoPassResults_PurityFirstThenFragThenMaxSize confirms the sort
// key order: purity==1.0 combos sort before imperfect ones regardless of
// their other fields, and among purity==1.0 combos, fewer fragments wins,
// then smaller max cluster size.
func TestSortTwoPassResults_PurityFirstThenFragThenMaxSize(t *testing.T) {
	rs := []TwoPassCombo{
		{TTight: 0.30, TMerge: 0.50, Purity: 0.90, FragCount: 0, MaxSize: 5},
		{TTight: 0.31, TMerge: 0.51, Purity: 1.0, FragCount: 2, MaxSize: 3},
		{TTight: 0.32, TMerge: 0.52, Purity: 1.0, FragCount: 1, MaxSize: 10},
		{TTight: 0.33, TMerge: 0.53, Purity: 1.0, FragCount: 1, MaxSize: 4},
	}
	SortTwoPassResults(rs)
	require.InDelta(t, 0.33, rs[0].TTight, 1e-9, "purity=1.0, frag=1, smallest maxSize wins")
	require.InDelta(t, 0.32, rs[1].TTight, 1e-9, "purity=1.0, frag=1, larger maxSize")
	require.InDelta(t, 0.31, rs[2].TTight, 1e-9, "purity=1.0 but frag=2, sorts after both frag=1 combos")
	require.InDelta(t, 0.30, rs[3].TTight, 1e-9, "impure combo sorts last regardless of frag/maxSize")
}

// ── SelectTwoPassCombo ───────────────────────────────────────────────────

// TestSelectTwoPassCombo_PureHeadWins covers the case where the
// (already-sorted) grid has at least one purity==1.0 combo at its head.
func TestSelectTwoPassCombo_PureHeadWins(t *testing.T) {
	rs := []TwoPassCombo{
		{TTight: 0.33, TMerge: 0.53, Purity: 1.0, FragCount: 0},
		{TTight: 0.30, TMerge: 0.50, Purity: 0.80, FragCount: 3},
	}
	combo, ok := SelectTwoPassCombo(rs)
	require.True(t, ok)
	require.Equal(t, rs[0], combo)
}

// TestSelectTwoPassCombo_NoPureCombo is new test case ② from the task
// brief: no combo in the (sorted) grid reaches purity==1.0 -> ok=false (no
// pure combo -> hold, never fall back to the closest-but-impure one).
func TestSelectTwoPassCombo_NoPureCombo(t *testing.T) {
	rs := []TwoPassCombo{
		{TTight: 0.30, TMerge: 0.50, Purity: 0.95, FragCount: 0},
		{TTight: 0.31, TMerge: 0.51, Purity: 0.80, FragCount: 1},
	}
	SortTwoPassResults(rs)
	_, ok := SelectTwoPassCombo(rs)
	require.False(t, ok)
}

// TestSelectTwoPassCombo_EmptyGrid covers an empty grid (e.g. TwoPassGridScan
// declined to scan): ok must be false.
func TestSelectTwoPassCombo_EmptyGrid(t *testing.T) {
	_, ok := SelectTwoPassCombo(nil)
	require.False(t, ok)
}

// ── TwoPassGridScan ──────────────────────────────────────────────────────

// twoPassSmokeFixture builds a tiny, deterministic two-person face set: 4
// faces for "A" tightly clustered around one point, 4 faces for "B" tightly
// clustered around a far point, all captured within the same moment (no gap
// exceeds any of the swept gaps) so GreedyMomentClusters/HACComplete see one
// moment per gap. Distances are chosen so a mid-range T_tight/T_merge always
// separates A from B perfectly (purity 1.0 reachable well within the
// builtin profile's grid bounds).
func twoPassSmokeFixture() (vecs [][]float32, takenAt, indexedAt []time.Time, idOf map[string]int, named []NamedTruth) {
	// A's vectors sit at cosine distance ~0 from each other (identical
	// direction, tiny norm-preserving perturbation via a second dimension);
	// B's vectors are near-orthogonal to A's, so cosDist(A,B) ~= 1.0, far
	// above any grid value used below.
	aVecs := [][]float32{{1, 0}, {0.99, 0.01}, {0.98, 0.02}, {0.97, 0.03}}
	bVecs := [][]float32{{0, 1}, {0.01, 0.99}, {0.02, 0.98}, {0.03, 0.97}}

	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	idOf = map[string]int{}

	aIDs := []string{"a0", "a1", "a2", "a3"}
	bIDs := []string{"b0", "b1", "b2", "b3"}
	allIDs := append(append([]string{}, aIDs...), bIDs...)
	allVecs := append(append([][]float32{}, aVecs...), bVecs...)

	for i, id := range allIDs {
		vecs = append(vecs, allVecs[i])
		takenAt = append(takenAt, base.Add(time.Duration(i)*time.Second))
		indexedAt = append(indexedAt, base.Add(time.Duration(i)*time.Second))
		idOf[id] = i
	}

	named = []NamedTruth{
		{PersonID: "A", FaceIDs: aIDs},
		{PersonID: "B", FaceIDs: bIDs},
	}
	return
}

// TestTwoPassGridScan_SmokeFixtureFindsPureCombo confirms the grid-scan
// driver runs end-to-end on the builtin profile's bounds and finds at least
// one purity==1.0 combo for this cleanly-separated two-person fixture.
func TestTwoPassGridScan_SmokeFixtureFindsPureCombo(t *testing.T) {
	vecs, takenAt, indexedAt, idOf, named := twoPassSmokeFixture()
	profile := builtinFactoryProfile()
	tightSpec := profile.Thresholds["ClusterTightEps"]
	mergeSpec := profile.Thresholds["ClusterMergeEps"]

	results, ok := TwoPassGridScan(context.Background(), vecs, takenAt, indexedAt, idOf, named, tightSpec, mergeSpec)
	require.True(t, ok)
	require.NotEmpty(t, results)

	SortTwoPassResults(results)
	best, ok := SelectTwoPassCombo(results)
	require.True(t, ok, "cleanly-separated two-person fixture must reach purity=1.0 somewhere in the grid")
	require.InDelta(t, 1.0, best.Purity, 1e-9)
	require.Equal(t, 0, best.FragCount)
}

// TestTwoPassGridScan_OverMaxFacesDeclinesWithoutScanning is new test case ①
// from the task brief: len(vecs) > TwoPassMaxFaces must return ok=false
// without running the grid at all (empty result, and -- since takenAt/
// indexedAt below are deliberately mismatched in length with vecs -- a
// SegmentMoments panic would fail this test if the scan were attempted).
func TestTwoPassGridScan_OverMaxFacesDeclinesWithoutScanning(t *testing.T) {
	n := TwoPassMaxFaces + 1
	vecs := make([][]float32, n)
	for i := range vecs {
		vecs[i] = []float32{1, 0}
	}
	// Deliberately length-0, not length-n: proves TwoPassGridScan returns
	// before ever touching takenAt/indexedAt (SegmentMoments would panic on
	// this mismatch otherwise).
	var takenAt, indexedAt []time.Time

	profile := builtinFactoryProfile()
	results, ok := TwoPassGridScan(context.Background(), vecs, takenAt, indexedAt, nil, nil,
		profile.Thresholds["ClusterTightEps"], profile.Thresholds["ClusterMergeEps"])
	require.False(t, ok)
	require.Nil(t, results)
}

// TestTwoPassGridScan_CancelledContext_ReturnsWithoutScanning is fix-round-1
// case ①: a pre-cancelled ctx must return (nil, false) without ever running
// GreedyMomentClusters/HACComplete (the expensive steps the ctx.Err() checks
// guard) -- proven here by never reaching purity=1.0/any combo at all,
// despite using the exact same cleanly-separated fixture that
// TestTwoPassGridScan_SmokeFixtureFindsPureCombo shows DOES produce results
// when ctx is not cancelled.
func TestTwoPassGridScan_CancelledContext_ReturnsWithoutScanning(t *testing.T) {
	vecs, takenAt, indexedAt, idOf, named := twoPassSmokeFixture()
	profile := builtinFactoryProfile()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	results, ok := TwoPassGridScan(ctx, vecs, takenAt, indexedAt, idOf, named,
		profile.Thresholds["ClusterTightEps"], profile.Thresholds["ClusterMergeEps"])
	require.False(t, ok)
	require.Nil(t, results)
}

// TestTwoPassGridScan_GridBoundsHonorPassedSpecs is new test case ③ from
// the task brief: passing a narrow tight/merge spec must confine every
// produced combo's TTight/TMerge to that spec's [Min,Max] band, proving the
// grid bounds are actually read from the passed specs rather than some
// hardcoded constant.
func TestTwoPassGridScan_GridBoundsHonorPassedSpecs(t *testing.T) {
	vecs, takenAt, indexedAt, idOf, named := twoPassSmokeFixture()

	tightSpec := ThresholdSpec{Default: 0.32, Min: 0.30, Max: 0.33}
	mergeSpec := ThresholdSpec{Default: 0.55, Min: 0.50, Max: 0.55}

	results, ok := TwoPassGridScan(context.Background(), vecs, takenAt, indexedAt, idOf, named, tightSpec, mergeSpec)
	require.True(t, ok)
	require.NotEmpty(t, results)

	for _, r := range results {
		require.GreaterOrEqual(t, r.TTight, tightSpec.Min)
		require.LessOrEqual(t, r.TTight, tightSpec.Max)
		require.GreaterOrEqual(t, r.TMerge, mergeSpec.Min)
		require.LessOrEqual(t, r.TMerge, mergeSpec.Max)
		// The +0.10 tight/merge separation floor must also hold for every
		// produced combo (see TwoPassGridScan's doc comment).
		require.GreaterOrEqual(t, r.TMerge, r.TTight+0.10-1e-9)
	}
}
