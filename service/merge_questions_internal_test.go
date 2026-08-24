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

// The two cluster-merge-question accessors must fall back to their
// documented defaults when config.Cfg is nil, mirroring the
// clusterEpsilon()/mergeEps() nil-config-fallback pattern.
func TestMergeSuggestAccessorsDefaultWhenConfigNil(t *testing.T) {
	old := config.Cfg
	config.Cfg = nil
	defer func() { config.Cfg = old }()

	require.Equal(t, 0.06, mergeSuggestBand())
	require.Equal(t, 30, mergeSuggestCap())
}

// mkPerson builds a synthetic mcPerson for the pure-function tests below: a
// single vec duplicated is enough for complete-linkage distance tests
// (max pairwise distance of a person against itself, or against another
// single-vec person, degenerates to the pairwise cosine distance).
func mkPerson(id, name string, vecs ...[]float32) mcPerson {
	return mcPerson{id: id, name: name, vecs: vecs, centroid: ComputeCentroid(vecs)}
}

// TestGenerateMergeCandidates_GrayBandPairSuggested: two unnamed clusters
// whose complete-linkage distance falls in (ClusterMergeEps,
// ClusterMergeEps+MergeSuggestBand] must be surfaced as a candidate.
func TestGenerateMergeCandidates_GrayBandPairSuggested(t *testing.T) {
	eps, band := 0.55, 0.06
	va, vb := vecAtCosineDistance(8, eps+0.02) // 0.57, inside (0.55, 0.61]
	a := mkPerson("a", "", va)
	b := mkPerson("b", "", vb)

	cands := generateMergeCandidates([]mcPerson{a, b}, eps, band, 30, nil)

	require.Len(t, cands, 1)
	require.InDelta(t, eps+0.02, cands[0].dist, 1e-6)
}

// TestGenerateMergeCandidates_BelowEpsExcluded: a pair whose distance is at
// or below ClusterMergeEps would already have been merged by pass-2's own
// HAC, so it must never be re-surfaced as a "gray band" candidate.
func TestGenerateMergeCandidates_BelowEpsExcluded(t *testing.T) {
	eps, band := 0.55, 0.06
	// Clearly below eps (not exactly at it -- vecAtCosineDistance's
	// float32 rounding can land a hair above the requested distance,
	// which the inclusive boundary itself is not the point of this test).
	va, vb := vecAtCosineDistance(8, eps-0.05)
	a := mkPerson("a", "", va)
	b := mkPerson("b", "", vb)

	cands := generateMergeCandidates([]mcPerson{a, b}, eps, band, 30, nil)

	require.Empty(t, cands, "a pair at or below ClusterMergeEps must not be surfaced (HAC would already have merged it)")
}

// TestGenerateMergeCandidates_AboveBandExcluded: a pair whose distance is
// past ClusterMergeEps+MergeSuggestBand is too far apart to be a plausible
// "almost merged" candidate.
func TestGenerateMergeCandidates_AboveBandExcluded(t *testing.T) {
	eps, band := 0.55, 0.06
	va, vb := vecAtCosineDistance(8, eps+band+0.10)
	a := mkPerson("a", "", va)
	b := mkPerson("b", "", vb)

	cands := generateMergeCandidates([]mcPerson{a, b}, eps, band, 30, nil)

	require.Empty(t, cands, "a pair beyond the gray band must not be surfaced")
}

// TestGenerateMergeCandidates_NegativeLinkedSuppressed: a pair that would
// otherwise be a valid gray-band candidate is suppressed when a
// face_negative_pairs-style entry exists between any of their member faces.
func TestGenerateMergeCandidates_NegativeLinkedSuppressed(t *testing.T) {
	eps, band := 0.55, 0.06
	va, vb := vecAtCosineDistance(8, eps+0.02)
	a := mcPerson{id: "a", faceIDs: []string{"fa"}, vecs: [][]float32{va}, centroid: va}
	b := mcPerson{id: "b", faceIDs: []string{"fb"}, vecs: [][]float32{vb}, centroid: vb}

	neg := map[string]bool{pairKey("fa", "fb"): true}
	cands := generateMergeCandidates([]mcPerson{a, b}, eps, band, 30, neg)

	require.Empty(t, cands, "a negative-linked pair must be suppressed even though its distance is in the gray band")
}

// TestGenerateMergeCandidates_NamedNamedExcluded: two named clusters in the
// gray band must never be auto-suggested (standing rule), regardless of
// distance.
func TestGenerateMergeCandidates_NamedNamedExcluded(t *testing.T) {
	eps, band := 0.55, 0.06
	va, vb := vecAtCosineDistance(8, eps+0.02)
	a := mkPerson("a", "Alice", va)
	b := mkPerson("b", "Bob", vb)

	cands := generateMergeCandidates([]mcPerson{a, b}, eps, band, 30, nil)

	require.Empty(t, cands, "named<->named pairs must never be suggested")
}

// TestGenerateMergeCandidates_OneNamedWinsInto: when exactly one side is
// named, that side must be "into" (the merge target) regardless of size.
func TestGenerateMergeCandidates_OneNamedWinsInto(t *testing.T) {
	eps, band := 0.55, 0.06
	va, vb := vecAtCosineDistance(8, eps+0.02)
	// "Alice" (named) has fewer members than the unnamed cluster, but must
	// still win "into".
	alice := mkPerson("alice", "Alice", va)
	unnamed := mkPerson("blob", "", vb, vb, vb)

	cands := generateMergeCandidates([]mcPerson{alice, unnamed}, eps, band, 30, nil)

	require.Len(t, cands, 1)
	require.Equal(t, "alice", cands[0].intoID)
	require.Equal(t, "blob", cands[0].fromID)
}

// TestGenerateMergeCandidates_LargerUnnamedWinsInto: when neither side is
// named, the larger cluster (by member count) must win "into".
func TestGenerateMergeCandidates_LargerUnnamedWinsInto(t *testing.T) {
	eps, band := 0.55, 0.06
	va, vb := vecAtCosineDistance(8, eps+0.02)
	small := mkPerson("small", "", va)
	big := mkPerson("big", "", vb, vb, vb)

	cands := generateMergeCandidates([]mcPerson{small, big}, eps, band, 30, nil)

	require.Len(t, cands, 1)
	require.Equal(t, "big", cands[0].intoID)
	require.Equal(t, "small", cands[0].fromID)
}

// TestGenerateMergeCandidates_CapHonored: with more gray-band candidates
// than MergeSuggestCap, only the closest `cap` (dist ascending) survive.
func TestGenerateMergeCandidates_CapHonored(t *testing.T) {
	eps, band := 0.55, 0.06

	// Build a "hub" person and several "spoke" persons, each at a distinct
	// distance from the hub inside the gray band -- keeps every pair
	// independent (no cross-spoke comparisons land in-band) so the cap is
	// the only thing trimming the candidate list.
	hubVec := make([]float32, 8)
	hubVec[0] = 1.0
	hub := mkPerson("hub", "", hubVec)

	persons := []mcPerson{hub}
	dists := []float64{0.56, 0.57, 0.58, 0.59, 0.60, 0.61}
	for i, d := range dists {
		_, v := vecAtCosineDistance(8, d)
		persons = append(persons, mkPerson(fmt.Sprintf("spoke%d", i), "", v))
	}

	cands := generateMergeCandidates(persons, eps, band, 3, nil)

	require.Len(t, cands, 3, "cap=3 must trim the candidate list to the 3 closest pairs")
	require.InDelta(t, 0.56, cands[0].dist, 1e-6)
	require.InDelta(t, 0.57, cands[1].dist, 1e-6)
	require.InDelta(t, 0.58, cands[2].dist, 1e-6)
}

// TestGenerateMergeCandidates_PerformanceAt1200ClusterScale mirrors
// TestHACCompletePerformanceSmokeAt5095Scale's shape (cluster_engine_test.go):
// a synthetic pass at the production scale noted in OVERVIEW.md's "Two-pass
// clustering engine" section (2026-08-20 production run: 1182 final
// clusters). 1199 "ordinary" clusters of 20 members plus one 235-member
// cluster (OVERVIEW.md's documented production max cluster size) stress the
// realistic worst case: one big cluster compared against everyone else.
//
// Without the centroid prefilter in generateMergeCandidates, this would pay
// full O(|A|*|B|) complete-linkage cost for essentially every one of the
// ~720K person pairs (random unit vectors in low dimensions are almost all
// far apart, so almost none would actually reach the distance-band check) --
// on the order of ~300M cosDist calls. The prefilter turns the vast
// majority of those into a single cheap centroid-to-centroid cosDist call
// instead, which is what keeps this comfortably inside a single-digit-second
// budget; see the test log for the actual number measured on this run.
func TestGenerateMergeCandidates_PerformanceAt1200ClusterScale(t *testing.T) {
	const clusterCount = 1200
	const dim = 16
	const avgSize = 20
	const megaSize = 235 // OVERVIEW.md's documented production max cluster size

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

	jitterMember := func(center []float32) []float32 {
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
		return member
	}

	persons := make([]mcPerson, 0, clusterCount)
	totalFaces := 0
	for c := 0; c < clusterCount; c++ {
		size := avgSize
		if c == 0 {
			size = megaSize
		}
		center := randUnitVec()
		vecs := make([][]float32, size)
		faceIDs := make([]string, size)
		for m := 0; m < size; m++ {
			vecs[m] = jitterMember(center)
			faceIDs[m] = fmt.Sprintf("c%d-f%d", c, m)
		}
		totalFaces += size
		persons = append(persons, mcPerson{
			id:       fmt.Sprintf("p%d", c),
			faceIDs:  faceIDs,
			vecs:     vecs,
			centroid: ComputeCentroid(vecs),
		})
	}

	start := time.Now()
	candidates := generateMergeCandidates(persons, 0.55, 0.06, 30, nil)
	elapsed := time.Since(start)

	t.Logf("generateMergeCandidates perf: %d clusters (%d total faces, one %d-member mega cluster) -> %d candidates in %s",
		clusterCount, totalFaces, megaSize, len(candidates), elapsed)
	require.Less(t, elapsed, 10*time.Second,
		"generation stage must stay comfortably within a single-digit-seconds budget at ~1200-cluster production scale, took %s", elapsed)
}
