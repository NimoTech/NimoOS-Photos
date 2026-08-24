package service

import (
	"fmt"
	"math"
)

// GreedyMomentClusters runs the conservative first pass: within each moment,
// faces whose cosine distance is <= eps are unioned (single-link inside a
// moment is safe: the moment boundary caps chain length). Cross-moment pairs
// are never compared here. Returns dense cluster labels aligned to input.
// Precondition: vecs and momentLabels are parallel slices (same length, same
// index alignment) — a length mismatch is a programmer error and panics.
func GreedyMomentClusters(vecs [][]float32, momentLabels []int, eps float64) []int {
	if len(momentLabels) != len(vecs) {
		panic(fmt.Sprintf("GreedyMomentClusters: momentLabels/vecs length mismatch (%d != %d)", len(momentLabels), len(vecs)))
	}

	n := len(vecs)
	if n == 0 {
		return nil
	}

	parent := make([]int, n)
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(x int) int {
		for parent[x] != x {
			parent[x] = parent[parent[x]] // path halving
			x = parent[x]
		}
		return x
	}
	union := func(a, b int) {
		ra, rb := find(a), find(b)
		if ra != rb {
			parent[ra] = rb
		}
	}

	// Bucket face indices by moment. A moment's face count is typically a
	// single digit, so the O(m^2) pairwise comparison inside each bucket is
	// cheap; cross-moment pairs are never formed, so chains can never cross
	// a moment boundary.
	buckets := make(map[int][]int)
	for i, m := range momentLabels {
		buckets[m] = append(buckets[m], i)
	}
	for _, idxs := range buckets {
		for a := 0; a < len(idxs); a++ {
			for b := a + 1; b < len(idxs); b++ {
				i, j := idxs[a], idxs[b]
				if cosDist(vecs[i], vecs[j]) <= eps {
					union(i, j)
				}
			}
		}
	}

	// Densify union-find roots into 0..k-1 labels, assigned in input order
	// so the first face of each cluster determines its label.
	labels := make([]int, n)
	rootLabel := make(map[int]int)
	next := 0
	for i := 0; i < n; i++ {
		r := find(i)
		lbl, ok := rootLabel[r]
		if !ok {
			lbl = next
			rootLabel[r] = lbl
			next++
		}
		labels[i] = lbl
	}
	return labels
}

// HACComplete merges the pass-1 clusters bottom-up with complete linkage
// (inter-cluster distance = max pairwise member distance) until the closest
// pair exceeds stopDist. Complete linkage is deliberately chosen over
// single/average: it is the most chaining-resistant, matching the
// "prefer splits over contamination" product stance -- a single-link/DBSCAN
// pass produced a 2612-face garbage cluster in production via exactly this
// kind of transitive chaining. Lance-Williams update on an in-memory
// distance matrix (n = pass-1 cluster count, ~thousands): d(new,k) =
// max(d(i,k), d(j,k)) after merging i and j. The closest active pair is
// found by a linear scan each iteration, which is fine for pass-1 cluster
// counts up to a few thousand.
// Input/output: labels aligned to faces (pass-1 labels in, merged labels out).
// Precondition: vecs and pass1Labels are parallel slices (same length, same
// index alignment) — a length mismatch is a programmer error and panics.
func HACComplete(vecs [][]float32, pass1Labels []int, stopDist float64) []int {
	if len(pass1Labels) != len(vecs) {
		panic(fmt.Sprintf("HACComplete: pass1Labels/vecs length mismatch (%d != %d)", len(pass1Labels), len(vecs)))
	}

	n := len(vecs)
	if n == 0 {
		return nil
	}

	// Compact the (possibly sparse/non-contiguous) pass-1 labels into dense
	// ids 0..m-1, assigned in input order, and bucket face indices per id.
	compactID := make(map[int]int)
	faceCompact := make([]int, n)
	var members [][]int
	for i, l := range pass1Labels {
		c, ok := compactID[l]
		if !ok {
			c = len(compactID)
			compactID[l] = c
			members = append(members, nil)
		}
		faceCompact[i] = c
		members[c] = append(members[c], i)
	}
	m := len(members)

	// Complete-link distance matrix between pass-1 clusters: dist[i][j] is
	// the max pairwise cosine distance between any member of cluster i and
	// any member of cluster j. Symmetric; diagonal is unused.
	dist := make([][]float64, m)
	for i := range dist {
		dist[i] = make([]float64, m)
	}
	for i := 0; i < m; i++ {
		for j := i + 1; j < m; j++ {
			var maxd float64
			for _, a := range members[i] {
				for _, b := range members[j] {
					if d := cosDist(vecs[a], vecs[b]); d > maxd {
						maxd = d
					}
				}
			}
			dist[i][j] = maxd
			dist[j][i] = maxd
		}
	}

	// Union-find over the m compact pass-1 cluster ids records the final
	// merge groups; the distance matrix is updated in place (Lance-Williams
	// complete rule) as clusters merge, and merged-away rows/columns are
	// dropped from the active set so the linear min-pair scan shrinks as
	// merging proceeds.
	parent := make([]int, m)
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(x int) int {
		for parent[x] != x {
			parent[x] = parent[parent[x]] // path halving
			x = parent[x]
		}
		return x
	}

	active := make([]int, m)
	for i := range active {
		active[i] = i
	}

	for len(active) > 1 {
		bestI, bestJ := -1, -1
		bestDist := math.Inf(1)
		for ai := 0; ai < len(active); ai++ {
			for aj := ai + 1; aj < len(active); aj++ {
				i, j := active[ai], active[aj]
				if dist[i][j] < bestDist {
					bestDist = dist[i][j]
					bestI, bestJ = ai, aj // positions within active, not cluster ids
				}
			}
		}
		if bestDist > stopDist {
			break
		}

		i, j := active[bestI], active[bestJ]
		parent[find(j)] = find(i)

		// Lance-Williams complete update: fold j's distances into i, then
		// drop j from the active set (swap-remove; order is irrelevant).
		for _, k := range active {
			if k == i || k == j {
				continue
			}
			if dist[j][k] > dist[i][k] {
				dist[i][k] = dist[j][k]
				dist[k][i] = dist[j][k]
			}
		}
		active[bestJ] = active[len(active)-1]
		active = active[:len(active)-1]
	}

	// Densify final union-find roots into 0..k-1 labels, assigned in face
	// input order so the first face of each merged cluster determines its
	// label (mirrors GreedyMomentClusters' densification convention).
	labels := make([]int, n)
	rootLabel := make(map[int]int)
	next := 0
	for i := 0; i < n; i++ {
		r := find(faceCompact[i])
		lbl, ok := rootLabel[r]
		if !ok {
			lbl = next
			rootLabel[r] = lbl
			next++
		}
		labels[i] = lbl
	}
	return labels
}
