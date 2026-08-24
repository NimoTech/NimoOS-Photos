package service

import (
	"fmt"
	"math"
	"math/rand"
	"testing"
	"time"

	"github.com/NimoTech/NimoOS-Photos/pkg/config"
	"github.com/stretchr/testify/require"
)

// The four apple-engine accessors must fall back to their documented defaults
// when config.Cfg is nil (tests construct services without config).
func TestClusterEngineAccessorsDefaultWhenConfigNil(t *testing.T) {
	old := config.Cfg
	config.Cfg = nil
	defer func() { config.Cfg = old }()

	require.Equal(t, "apple", clusterEngine())
	require.Equal(t, 60*time.Minute, momentGap())
	require.Equal(t, 0.35, tightEps())
	require.Equal(t, 0.55, mergeEps())
}

// vecAtCosineDistance returns two unit vectors of the given dimensionality
// whose cosine distance is exactly `dist` (cosDist = 1 - cosTheta). Follows
// the unit-vector-rotated-to-a-target-angle pattern already used in
// faces_test.go / cluster_epsilon_test.go: the base vector is e0, the second
// vector is placed in the e0/e1 plane at the angle that yields the requested
// distance.
func vecAtCosineDistance(dim int, dist float64) (v1, v2 []float32) {
	v1 = make([]float32, dim)
	v1[0] = 1.0

	cosTheta := 1.0 - dist
	sinTheta := math.Sqrt(1 - cosTheta*cosTheta)
	v2 = make([]float32, dim)
	v2[0] = float32(cosTheta)
	v2[1] = float32(sinTheta)
	return v1, v2
}

// Two faces in the same moment at cosine distance 0.3 (<= eps 0.35) must
// land in the same cluster.
func TestGreedyMomentClustersSameMomentWithinEpsMerges(t *testing.T) {
	a, b := vecAtCosineDistance(4, 0.3)

	labels := GreedyMomentClusters([][]float32{a, b}, []int{0, 0}, 0.35)

	require.Equal(t, labels[0], labels[1])
}

// The same two vectors (cosine distance 0.3, within eps) placed in different
// moments must never be compared, so they land in different clusters.
func TestGreedyMomentClustersDifferentMomentsNeverCompared(t *testing.T) {
	a, b := vecAtCosineDistance(4, 0.3)

	labels := GreedyMomentClusters([][]float32{a, b}, []int{0, 1}, 0.35)

	require.NotEqual(t, labels[0], labels[1])
	require.Equal(t, []int{0, 1}, labels)
}

// Within one moment, A-B distance 0.3 and B-C distance 0.3 are each within
// eps 0.35, but A-C distance 0.7 is far outside it. Single-link clustering
// inside a moment is accepted (the moment boundary caps chain length), so
// all three faces must land in the same cluster via the A-B-C chain.
func TestGreedyMomentClustersChainWithinMomentAllMerge(t *testing.T) {
	a := make([]float32, 4)
	a[0] = 1.0

	b := make([]float32, 4)
	b[0] = 0.7 // dot(A,B) = 0.7 -> dist(A,B) = 0.3
	b[1] = float32(math.Sqrt(1 - 0.7*0.7))

	// Solve C on the unit sphere so dot(A,C) = 0.3 (dist 0.7) and
	// dot(B,C) = 0.7 (dist 0.3), i.e. C chains to B without being close to A.
	c := make([]float32, 4)
	c[0] = 0.3
	c1 := (0.7 - float64(b[0])*float64(c[0])) / float64(b[1])
	c[1] = float32(c1)
	remaining := 1 - float64(c[0])*float64(c[0]) - c1*c1
	c[2] = float32(math.Sqrt(remaining))

	require.InDelta(t, 0.3, cosDist(a, b), 1e-6)
	require.InDelta(t, 0.3, cosDist(b, c), 1e-6)
	require.InDelta(t, 0.7, cosDist(a, c), 1e-6)

	labels := GreedyMomentClusters([][]float32{a, b, c}, []int{0, 0, 0}, 0.35)

	require.Equal(t, []int{0, 0, 0}, labels)
}

// eps is an inclusive bound: a pair whose cosine distance exactly equals eps
// must still merge. eps is derived from the same cosDist call the
// implementation uses internally, so the comparison is an exact boundary
// regardless of floating-point rounding in the vector construction.
func TestGreedyMomentClustersEpsBoundaryEqualMerges(t *testing.T) {
	a, b := vecAtCosineDistance(4, 0.4)
	eps := cosDist(a, b)

	labels := GreedyMomentClusters([][]float32{a, b}, []int{0, 0}, eps)

	require.Equal(t, labels[0], labels[1])
}

// A momentLabels/vecs length mismatch is a programmer error, not a
// recoverable case: the function must panic with a descriptive message
// rather than let bucketing silently over/under-index vecs. Both mismatch
// directions (more labels than vecs, and more vecs than labels) must panic.
func TestGreedyMomentClustersPanicsOnLengthMismatch(t *testing.T) {
	a, b := vecAtCosineDistance(4, 0.3)

	t.Run("more moment labels than vecs", func(t *testing.T) {
		defer func() {
			r := recover()
			require.NotNil(t, r, "expected a panic")
			require.Contains(t, fmt.Sprint(r), "GreedyMomentClusters: momentLabels/vecs length mismatch (3 != 2)")
		}()
		GreedyMomentClusters([][]float32{a, b}, []int{0, 0, 0}, 0.35)
	})

	t.Run("more vecs than moment labels", func(t *testing.T) {
		defer func() {
			r := recover()
			require.NotNil(t, r, "expected a panic")
			require.Contains(t, fmt.Sprint(r), "GreedyMomentClusters: momentLabels/vecs length mismatch (1 != 2)")
		}()
		GreedyMomentClusters([][]float32{a, b}, []int{0}, 0.35)
	})
}

// When config.Cfg is populated, the accessors must reflect it.
func TestClusterEngineAccessorsConfigDriven(t *testing.T) {
	old := config.Cfg
	config.Cfg = &config.Config{
		ClusterEngine:    "dbscan",
		MomentGapMinutes: 30,
		ClusterTightEps:  0.2,
		ClusterMergeEps:  0.4,
		// MomentGapMinutes/ClusterTightEps/ClusterMergeEps are calibratable
		// thresholds: since resolveThreshold's four-layer stack only honors
		// a conf value when it's marked explicit, this must be set for the
		// config values above to take effect.
		Explicit: map[string]bool{
			"MomentGapMinutes": true,
			"ClusterTightEps":  true,
			"ClusterMergeEps":  true,
		},
	}
	defer func() { config.Cfg = old }()

	require.Equal(t, "dbscan", clusterEngine())
	require.Equal(t, 30*time.Minute, momentGap())
	require.Equal(t, 0.2, tightEps())
	require.Equal(t, 0.4, mergeEps())
}

// Zero/empty values in config.Cfg (e.g. a config file predating this field)
// must still fall back to the documented defaults, matching clusterEpsilon()'s
// "non-positive falls back" semantics.
func TestClusterEngineAccessorsFallBackOnZeroValue(t *testing.T) {
	old := config.Cfg
	config.Cfg = &config.Config{}
	defer func() { config.Cfg = old }()

	require.Equal(t, "apple", clusterEngine())
	require.Equal(t, 60*time.Minute, momentGap())
	require.Equal(t, 0.35, tightEps())
	require.Equal(t, 0.55, mergeEps())
}

// --- HACComplete: second pass, complete-linkage HAC across pass-1 clusters ---
//
// This is the soul test of the whole plan: complete linkage is chosen
// specifically because it resists chaining, unlike single-link/DBSCAN which
// once produced a 2612-face garbage cluster in production. Three singleton
// pass-1 clusters A, B, C with d(A,B)=0.4, d(B,C)=0.4, d(A,C)=0.9 and
// stopDist=0.55: A and B merge first (0.4 <= 0.55). After the merge, the
// complete-link distance from {A,B} to C is max(d(A,C), d(B,C)) =
// max(0.9, 0.4) = 0.9, which is > 0.55, so C must NOT merge in. A single-link
// (or DBSCAN-style) implementation would instead use min(0.9, 0.4) = 0.4 and
// incorrectly chain all three into one cluster -- that is exactly the failure
// mode this algorithm exists to prevent.
func TestHACCompleteAntiPercolationRegression(t *testing.T) {
	dim := 4

	a := make([]float32, dim)
	a[0] = 1.0 // A = e0

	b := make([]float32, dim)
	b[0] = 0.6 // dot(A,B) = 0.6 -> dist(A,B) = 0.4
	b[1] = float32(math.Sqrt(1 - 0.6*0.6))

	// Solve C so dot(A,C) = 0.1 (dist 0.9) and dot(B,C) = 0.6 (dist 0.4).
	c := make([]float32, dim)
	c[0] = 0.1
	c1 := (0.6 - float64(b[0])*float64(c[0])) / float64(b[1])
	c[1] = float32(c1)
	remaining := 1 - float64(c[0])*float64(c[0]) - c1*c1
	c[2] = float32(math.Sqrt(remaining))

	require.InDelta(t, 0.4, cosDist(a, b), 1e-6)
	require.InDelta(t, 0.4, cosDist(b, c), 1e-6)
	require.InDelta(t, 0.9, cosDist(a, c), 1e-6)

	labels := HACComplete([][]float32{a, b, c}, []int{0, 1, 2}, 0.55)

	require.Equal(t, labels[0], labels[1], "A and B must merge (dist 0.4 <= stopDist 0.55)")
	require.NotEqual(t, labels[0], labels[2], "C must stay separate: complete-link dist to {A,B} is max(0.9,0.4)=0.9 > 0.55")
}

// When every pass-1 cluster is mutually far beyond stopDist, no merges must
// happen at all: the output must be a relabeling of the input, one cluster
// per input label.
func TestHACCompleteAllMutuallyFarNoMerges(t *testing.T) {
	dim := 4
	vecs := make([][]float32, dim)
	pass1 := make([]int, dim)
	for i := 0; i < dim; i++ {
		v := make([]float32, dim)
		v[i] = 1.0 // orthogonal basis vectors: pairwise cosDist = 1.0
		vecs[i] = v
		pass1[i] = i
	}

	labels := HACComplete(vecs, pass1, 0.5)

	seen := map[int]bool{}
	for _, l := range labels {
		require.False(t, seen[l], "each orthogonal cluster must keep its own label")
		seen[l] = true
	}
	require.Len(t, seen, dim)
}

// A very large stopDist must merge every pass-1 cluster into one, regardless
// of how far apart they start.
func TestHACCompleteHugeStopDistMergesIntoSingleCluster(t *testing.T) {
	dim := 5
	vecs := make([][]float32, dim)
	pass1 := make([]int, dim)
	for i := 0; i < dim; i++ {
		v := make([]float32, dim)
		v[i] = 1.0
		vecs[i] = v
		pass1[i] = i
	}

	labels := HACComplete(vecs, pass1, 2.0) // 2.0 >= max possible cosDist

	for i := 1; i < len(labels); i++ {
		require.Equal(t, labels[0], labels[i], "huge stopDist must collapse everything into one cluster")
	}
}

// Empty input must not panic and must return an empty/nil result.
func TestHACCompleteEmptyInputNoPanic(t *testing.T) {
	require.NotPanics(t, func() {
		labels := HACComplete(nil, nil, 0.5)
		require.Empty(t, labels)
	})
}

// A single pass-1 cluster (no pair exists to merge) must not panic and must
// return one label shared by all its members.
func TestHACCompleteSingleClusterNoPanic(t *testing.T) {
	a, b := vecAtCosineDistance(4, 0.3)
	c := make([]float32, 4)
	c[0] = 1.0

	var labels []int
	require.NotPanics(t, func() {
		labels = HACComplete([][]float32{a, b, c}, []int{0, 0, 0}, 0.5)
	})

	require.Equal(t, labels[0], labels[1])
	require.Equal(t, labels[0], labels[2])
}

// A pass1Labels/vecs length mismatch is a programmer error and must panic
// with a descriptive message, in both directions, mirroring the convention
// established by GreedyMomentClusters/SegmentMoments.
func TestHACCompletePanicsOnLengthMismatch(t *testing.T) {
	a, b := vecAtCosineDistance(4, 0.3)

	t.Run("more pass1 labels than vecs", func(t *testing.T) {
		defer func() {
			r := recover()
			require.NotNil(t, r, "expected a panic")
			require.Contains(t, fmt.Sprint(r), "HACComplete: pass1Labels/vecs length mismatch (3 != 2)")
		}()
		HACComplete([][]float32{a, b}, []int{0, 0, 0}, 0.5)
	})

	t.Run("more vecs than pass1 labels", func(t *testing.T) {
		defer func() {
			r := recover()
			require.NotNil(t, r, "expected a panic")
			require.Contains(t, fmt.Sprint(r), "HACComplete: pass1Labels/vecs length mismatch (1 != 2)")
		}()
		HACComplete([][]float32{a, b}, []int{0}, 0.5)
	})
}

// Performance smoke test at production scale (memory notes record a 5095-face
// rebuild for the People line). The synthetic input deliberately does NOT
// produce 5095 pass-1 singletons: it builds 1800 pass-1 clusters (sized 2-3
// members each, round-robin distributed to sum to exactly 5095 faces) so the
// distance-matrix + linear-scan-merge implementation is exercised at a
// realistic pass-1 cluster count (within the ~1500-2500 range the brief
// calls out), not the trivial all-singletons case. Cluster centers are
// pseudo-random unit vectors in a 16-dim space (fixed seed for
// reproducibility) with small per-member jitter, so a handful of centers land
// close enough to merge while most stay far apart -- similar to a real
// tight-eps pass-1 output feeding the merge pass. The only assertion is
// shape + a 10s wall-clock budget; exact cluster assignments are not
// meaningful for random data.
func TestHACCompletePerformanceSmokeAt5095Scale(t *testing.T) {
	const totalFaces = 5095
	const clusterCount = 1800
	const dim = 16

	rng := rand.New(rand.NewSource(42))

	randUnitVec := func() []float32 {
		v := make([]float32, dim)
		var norm float64
		for i := range v {
			f := rng.NormFloat64()
			v[i] = float32(f)
			norm += f * f
		}
		norm = math.Sqrt(norm)
		for i := range v {
			v[i] = float32(float64(v[i]) / norm)
		}
		return v
	}

	base := totalFaces / clusterCount
	remainder := totalFaces % clusterCount

	vecs := make([][]float32, 0, totalFaces)
	pass1 := make([]int, 0, totalFaces)
	for c := 0; c < clusterCount; c++ {
		size := base
		if c < remainder {
			size++
		}
		center := randUnitVec()
		for m := 0; m < size; m++ {
			member := make([]float32, dim)
			var norm float64
			for i := range member {
				jitter := 0.02 * rng.NormFloat64()
				f := float64(center[i]) + jitter
				member[i] = float32(f)
				norm += f * f
			}
			norm = math.Sqrt(norm)
			for i := range member {
				member[i] = float32(float64(member[i]) / norm)
			}
			vecs = append(vecs, member)
			pass1 = append(pass1, c)
		}
	}
	require.Len(t, vecs, totalFaces)

	start := time.Now()
	labels := HACComplete(vecs, pass1, 0.55)
	elapsed := time.Since(start)

	require.Len(t, labels, totalFaces)
	require.Less(t, elapsed, 10*time.Second, "HACComplete must finish the 5095-face/1800-cluster smoke test in under 10s, took %s", elapsed)
	t.Logf("HACComplete perf smoke: %d faces / %d pass-1 clusters -> %d in %s", totalFaces, clusterCount, len(distinctLabels(labels)), elapsed)
}

// distinctLabels returns the set of distinct values in labels, used only for
// the perf smoke test's diagnostic log line.
func distinctLabels(labels []int) map[int]bool {
	seen := map[int]bool{}
	for _, l := range labels {
		seen[l] = true
	}
	return seen
}

// Worst-case performance regression: 3000 pass-1 singleton clusters (the
// plan's own sanctioned upper bound for the linear min-pair scan) with
// stopDist set above the maximum possible cosine distance, forcing a full
// merge chain all the way down from 3000 active clusters to 1. This is the
// true worst case for the O(active^2)-per-iteration scan (Task 4's review
// measured ~5.65s on this dev machine at this exact scale/shape). The
// default mergeEps (0.55) never approaches this shape in production -- most
// pass-1 clusters stay far apart and merging stops early -- but a future
// threshold bump or an unusual embedding could. The budget below (20s) sits
// comfortably above the measured 5.65s baseline to avoid slow-machine
// flakiness, while still catching an accidental regression to worse than
// O(m^3) (e.g. an unbounded active-list scan or matrix rebuild per merge).
func TestHACCompleteWorstCasePerformanceAt3000SingletonClusters(t *testing.T) {
	const m = 3000
	const dim = 8

	rng := rand.New(rand.NewSource(7))
	vecs := make([][]float32, m)
	pass1 := make([]int, m)
	for i := 0; i < m; i++ {
		v := make([]float32, dim)
		for d := range v {
			v[d] = float32(rng.NormFloat64())
		}
		vecs[i] = v
		pass1[i] = i // every face is its own pass-1 singleton cluster
	}

	start := time.Now()
	// 2.5 exceeds the maximum possible cosDist (2.0), guaranteeing every
	// merge step succeeds until only one active cluster remains.
	labels := HACComplete(vecs, pass1, 2.5)
	elapsed := time.Since(start)

	for i := 1; i < len(labels); i++ {
		require.Equal(t, labels[0], labels[i], "stopDist above max possible cosDist must collapse all singleton clusters into one")
	}
	require.Less(t, elapsed, 20*time.Second, "HACComplete worst-case (3000 singleton clusters, full merge chain) must finish under the 20s budget, took %s", elapsed)
	t.Logf("HACComplete worst-case perf: %d singleton pass-1 clusters, full merge chain, took %s", m, elapsed)
}

// The complete-link merge comparison is documented as inclusive (a pair at
// exactly stopDist must still merge, matching GreedyMomentClusters' eps
// boundary semantics). stopDist is derived from the same live cosDist call
// used to construct the vectors (the eps-boundary trick already used by
// TestGreedyMomentClustersEpsBoundaryEqualMerges), so the comparison is an
// exact boundary regardless of float32 rounding. Nudging stopDist below that
// exact value by a small epsilon must flip the outcome to "no merge".
func TestHACCompleteMergesAtExactStopDistBoundary(t *testing.T) {
	a, b := vecAtCosineDistance(4, 0.4)
	stopDist := cosDist(a, b)

	atBoundary := HACComplete([][]float32{a, b}, []int{0, 1}, stopDist)
	require.Equal(t, atBoundary[0], atBoundary[1], "a pair at exactly stopDist must merge")

	belowBoundary := HACComplete([][]float32{a, b}, []int{0, 1}, stopDist-1e-9)
	require.NotEqual(t, belowBoundary[0], belowBoundary[1], "a pair just below stopDist must not merge")
}
