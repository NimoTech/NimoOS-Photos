package service

import (
	"fmt"
	"sort"
	"time"
)

// SegmentMoments assigns each face a moment label by walking capture times in
// ascending order and starting a new moment whenever the gap between adjacent
// times exceeds `gap`. Faces with a zero takenAt fall back to indexedAt.
// Returns one label per input index (labels are dense, starting at 0).
// Mirrors the "moment" grouping of Apple's first clustering pass.
// Precondition: takenAt and indexedAt are parallel slices (same length, same
// index alignment) — a length mismatch is a programmer error and panics.
func SegmentMoments(takenAt []time.Time, indexedAt []time.Time, gap time.Duration) []int {
	if len(takenAt) != len(indexedAt) {
		panic(fmt.Sprintf("SegmentMoments: takenAt/indexedAt length mismatch (%d != %d)", len(takenAt), len(indexedAt)))
	}

	n := len(takenAt)
	labels := make([]int, n)
	if n == 0 {
		return labels
	}

	// Resolve the effective timestamp per face: takenAt, falling back to
	// indexedAt when takenAt is the zero value.
	effective := make([]time.Time, n)
	for i, t := range takenAt {
		if t.IsZero() {
			effective[i] = indexedAt[i]
		} else {
			effective[i] = t
		}
	}

	// Sort indexes by effective time (input order = faces slice order; output
	// labels must align back to that order).
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		return effective[order[a]].Before(effective[order[b]])
	})

	moment := 0
	labels[order[0]] = moment
	for k := 1; k < n; k++ {
		prev := effective[order[k-1]]
		cur := effective[order[k]]
		if cur.Sub(prev) > gap {
			moment++
		}
		labels[order[k]] = moment
	}

	return labels
}
