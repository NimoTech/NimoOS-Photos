package service

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Three photos 10min apart, then another 10min apart: all within a 60min gap
// budget, so they must land in the same moment.
func TestSegmentMomentsAllWithinGapSameMoment(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	takenAt := []time.Time{
		base,
		base.Add(10 * time.Minute),
		base.Add(20 * time.Minute),
	}
	indexedAt := make([]time.Time, len(takenAt))

	labels := SegmentMoments(takenAt, indexedAt, 60*time.Minute)

	require.Equal(t, []int{0, 0, 0}, labels)
}

// A 2h gap between the second and third photo exceeds the moment gap budget,
// so the run must split into two moments.
func TestSegmentMomentsGapExceedsBudgetSplits(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	takenAt := []time.Time{
		base,
		base.Add(10 * time.Minute),
		base.Add(10*time.Minute + 2*time.Hour),
	}
	indexedAt := make([]time.Time, len(takenAt))

	labels := SegmentMoments(takenAt, indexedAt, 60*time.Minute)

	require.Equal(t, []int{0, 0, 1}, labels)
}

// A zero-value taken_at falls back to indexed_at so it still participates on
// the same timeline as its neighbors.
func TestSegmentMomentsZeroTakenAtFallsBackToIndexedAt(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	takenAt := []time.Time{
		base,
		{}, // zero value, must fall back to indexedAt
		base.Add(20 * time.Minute),
	}
	indexedAt := []time.Time{
		base,
		base.Add(10 * time.Minute),
		base.Add(20 * time.Minute),
	}

	labels := SegmentMoments(takenAt, indexedAt, 60*time.Minute)

	require.Equal(t, []int{0, 0, 0}, labels)
}

// When every takenAt is zero, the function must not panic and must segment
// purely off indexedAt.
func TestSegmentMomentsAllZeroTakenAtUsesIndexedAt(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	takenAt := make([]time.Time, 3)
	indexedAt := []time.Time{
		base,
		base.Add(10 * time.Minute),
		base.Add(10*time.Minute + 2*time.Hour),
	}

	labels := SegmentMoments(takenAt, indexedAt, 60*time.Minute)

	require.Equal(t, []int{0, 0, 1}, labels)
}

// A single face must always get label 0.
func TestSegmentMomentsSingleFace(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	takenAt := []time.Time{base}
	indexedAt := []time.Time{base}

	labels := SegmentMoments(takenAt, indexedAt, 60*time.Minute)

	require.Equal(t, []int{0}, labels)
}

// Input is given out of chronological order (t+20m, t+0m, t+10m, t+10m+2h).
// Sorted by effective time the run is: t+0m, t+10m, t+20m (all within the
// 60min gap budget, same moment), then a 1h50m gap to t+10m+2h (new moment).
// Output labels must realign back to the original (unsorted) input order.
func TestSegmentMomentsScrambledInputRealignsToInputOrder(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	takenAt := []time.Time{
		base.Add(20 * time.Minute),
		base,
		base.Add(10 * time.Minute),
		base.Add(10*time.Minute + 2*time.Hour),
	}
	indexedAt := make([]time.Time, len(takenAt))

	labels := SegmentMoments(takenAt, indexedAt, 60*time.Minute)

	require.Equal(t, []int{0, 0, 0, 1}, labels)
}

// A gap exactly equal to the budget (not exceeding it) must NOT split the
// moment: the split condition is strictly-greater-than, so an exact match
// keeps both photos in the same moment.
func TestSegmentMomentsExactGapEqualToBudgetStaysSameMoment(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	takenAt := []time.Time{
		base,
		base.Add(60 * time.Minute),
	}
	indexedAt := make([]time.Time, len(takenAt))

	labels := SegmentMoments(takenAt, indexedAt, 60*time.Minute)

	require.Equal(t, []int{0, 0}, labels)
}

// A takenAt/indexedAt length mismatch is a programmer error: it must panic
// with a descriptive message rather than let indexedAt[i] silently
// out-of-range (or under-fill) against takenAt.
func TestSegmentMomentsPanicsOnLengthMismatch(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	defer func() {
		r := recover()
		require.NotNil(t, r, "expected a panic")
		require.Contains(t, fmt.Sprint(r), "SegmentMoments: takenAt/indexedAt length mismatch (2 != 1)")
	}()
	SegmentMoments([]time.Time{base, base.Add(time.Minute)}, []time.Time{base}, 60*time.Minute)
}
