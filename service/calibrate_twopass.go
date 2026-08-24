package service

import (
	"context"
	"math"
	"sort"
	"time"
)

// ── T-twopass two-pass (Apple-engine) grid calibration core ────────────────
//
// This is the shared core behind cmd/cluster-analysis's `-mode twopass`
// offline report and the in-service calibration runner: it grid-scans
// T_tight (service.GreedyMomentClusters' conservative moment-local pass-1
// eps) x T_merge (service.HACComplete's complete-linkage stop distance)
// across a handful of moment-segmentation gaps, scoring each combo against
// named-person ground truth. Moved from cmd/cluster-analysis/twopass.go and
// majority.go (report printing / -mode twopass's driver stay in the CLI,
// which now only builds the vecs/idOf/named inputs and calls TwoPassGridScan).

// TwoPassMinNamedPersons/TwoPassMinNamedFaces are the insufficient-data guard
// bars (spec §4.1): below either, a two-pass calibration recommendation is
// not yet trustworthy. TwoPassMaxFaces is the grid-scan face budget (spec
// §9): HACComplete's per-combo cost is worst-case cubic in the pass-1
// cluster count, so above this many faces the current tier holds rather
// than paying for a scan.
const (
	TwoPassMinNamedPersons = 5
	TwoPassMinNamedFaces   = 300
	TwoPassMaxFaces        = 20000
)

// twoPassGaps are the moment-segmentation gap durations swept for the "gap
// sensitivity" requirement: the whole T_tight x T_merge grid is re-run at
// each of these gaps.
var twoPassGaps = []time.Duration{30 * time.Minute, 60 * time.Minute, 120 * time.Minute}

// NamedTruth is one named (non-anonymous) person's ground-truth face set:
// PersonID plus every face ID currently attributed to them.
type NamedTruth struct {
	PersonID string
	FaceIDs  []string
}

// TwoPassCombo is one grid-scan combo's report row: the cluster shape (count,
// max size) plus the named-person ground-truth metrics -- purity and
// fragment count.
type TwoPassCombo struct {
	Gap    time.Duration
	TTight float64
	TMerge float64

	NumClusters int
	MaxSize     int

	// Purity is the mean, over every named person with at least one face in
	// the set, of (that person's face count in their own majority cluster) /
	// (that majority cluster's total size). Purity == 1.0 iff EVERY named
	// person's majority cluster contains only their own faces (mean of
	// per-person values each capped at 1.0 can only reach 1.0 if all of them
	// do) -- same definition and same "meanPurity == 1.0" reading convention
	// as the legacy eps sweep's SweepResult.MeanPurity.
	Purity float64

	// FragCount is the named-person fragment count: summed, over every named
	// person with at least one face in the set, (number of distinct clusters
	// their faces land in) - 1. Zero iff every named person's faces are all
	// in a single cluster (whether or not that cluster is pure).
	FragCount int
}

// twoPassClusterSizes/twoPassMaxClusterSize mirror the offline tool's
// generic clusterSizes/maxClusterSize label-slice helpers (which stay in
// cmd/cluster-analysis/main.go, still used by its other analysis modes) --
// duplicated here, scoped to this file, rather than exported from the CLI.
func twoPassClusterSizes(labels []int) map[int]int {
	sizes := map[int]int{}
	for _, l := range labels {
		sizes[l]++
	}
	return sizes
}

func twoPassMaxClusterSize(sizes map[int]int) (label, size int) {
	for l, s := range sizes {
		if s > size {
			size = s
			label = l
		}
	}
	return
}

// PickMajorityLabel selects a named person's "majority cluster" from a
// histogram of (cluster label -> that person's face count in that label).
// The label with the highest count wins. On an exact count tie, iterating a
// Go map is non-deterministic, so a naive "first one seen wins" reduction
// makes purity (bestCnt/sizes[bestLbl]) flip between runs on identical
// input -- unacceptable for a calibration gate that treats purity==1.0 as a
// hard selection filter. Ties resolve pessimistically so purity==1.0 is
// never awarded by iteration-order luck: prefer the LARGER cluster (a
// bigger denominator yields the lower, more conservative purity reading),
// and if sizes also tie, prefer the smaller label number (pure, arbitrary
// but fixed, determinism). sizes maps every cluster label to its total
// member count. Returns bestLbl=-1, bestCnt=0 for an empty labelCount.
func PickMajorityLabel(labelCount map[int]int, sizes map[int]int) (bestLbl, bestCnt int) {
	bestLbl = -1
	for l, c := range labelCount {
		switch {
		case bestLbl == -1:
			bestLbl, bestCnt = l, c
		case c > bestCnt:
			bestLbl, bestCnt = l, c
		case c == bestCnt && (sizes[l] > sizes[bestLbl] || (sizes[l] == sizes[bestLbl] && l < bestLbl)):
			bestLbl = l
		}
	}
	return bestLbl, bestCnt
}

// EvalTwoPassCombo computes the TwoPassCombo metrics for one final label
// assignment (the output of GreedyMomentClusters -> HACComplete for a given
// T_tight/T_merge/gap combo). vec index i corresponds to faceIDs[i]; idOf
// maps faceID -> index.
func EvalTwoPassCombo(labels []int, gap time.Duration, tTight, tMerge float64, idOf map[string]int, named []NamedTruth) TwoPassCombo {
	sizes := twoPassClusterSizes(labels)
	_, maxSz := twoPassMaxClusterSize(sizes)

	var puritySum float64
	fragCount := 0
	n := 0
	for _, np := range named {
		labelCount := map[int]int{}
		total := 0
		for _, fid := range np.FaceIDs {
			gi, ok := idOf[fid]
			if !ok {
				continue // face not in this run's set
			}
			total++
			labelCount[labels[gi]]++
		}
		if total == 0 {
			continue
		}
		n++
		bestLbl, bestCnt := PickMajorityLabel(labelCount, sizes)
		purity := float64(bestCnt) / float64(sizes[bestLbl])
		puritySum += purity
		fragCount += len(labelCount) - 1 // clusters beyond this person's single majority one
	}
	meanPurity := 0.0
	if n > 0 {
		meanPurity = puritySum / float64(n)
	}

	return TwoPassCombo{
		Gap: gap, TTight: tTight, TMerge: tMerge,
		NumClusters: len(sizes), MaxSize: maxSz,
		Purity: meanPurity, FragCount: fragCount,
	}
}

// SortTwoPassResults orders combos so the recommended ones surface first:
// purity==1.0 combos sort before any imperfect ones, then minimize
// named-person fragment count, tie-break on smaller max cluster size, and
// finally (T_tight, T_merge, gap) for a stable, reproducible order among
// true ties.
func SortTwoPassResults(rs []TwoPassCombo) {
	sort.SliceStable(rs, func(i, j int) bool {
		pi, pj := rs[i].Purity >= 1.0, rs[j].Purity >= 1.0
		if pi != pj {
			return pi // pure combos first
		}
		if rs[i].FragCount != rs[j].FragCount {
			return rs[i].FragCount < rs[j].FragCount
		}
		if rs[i].MaxSize != rs[j].MaxSize {
			return rs[i].MaxSize < rs[j].MaxSize
		}
		if rs[i].TTight != rs[j].TTight {
			return rs[i].TTight < rs[j].TTight
		}
		if rs[i].TMerge != rs[j].TMerge {
			return rs[i].TMerge < rs[j].TMerge
		}
		return rs[i].Gap < rs[j].Gap
	})
}

// twoPassRound2 rounds to 2 decimals to avoid float64 accumulation drift
// (e.g. repeated +0.01/+0.10 landing on 0.3799999999999999 instead of 0.38),
// which would otherwise break grid threshold comparisons and any downstream
// equality checks.
func twoPassRound2(x float64) float64 {
	return math.Round(x*100) / 100
}

// twoPassFrange returns lo, lo+step, ..., hi inclusive, rounded to 2
// decimals (see twoPassRound2).
func twoPassFrange(lo, hi, step float64) []float64 {
	if hi < lo {
		return nil
	}
	n := int(math.Round((hi-lo)/step)) + 1
	out := make([]float64, n)
	for i := 0; i < n; i++ {
		out[i] = twoPassRound2(lo + float64(i)*step)
	}
	return out
}

// TwoPassGridScan grid-scans T_tight x T_merge at each gap in twoPassGaps,
// reusing SegmentMoments per gap and GreedyMomentClusters per (gap, T_tight)
// across the T_merge sub-loop -- only HACComplete, the expensive step, runs
// once per full (gap, T_tight, T_merge) combo -- mirroring the CLI's
// original runTwoPass reuse structure.
//
// The grid bounds come from the passed specs: T_tight ranges over
// [tightSpec.Min, tightSpec.Max] step 0.01. T_merge ranges over
// [max(T_tight+0.10, mergeSpec.Min), mergeSpec.Max] step 0.01 -- the +0.10
// floor keeps the ClusterTightEps<=ClusterMergeEps-0.10 invariant
// (spec §4.1) satisfiable: a T_merge any closer to T_tight than that would
// produce a tight/merge pair the production config validator would itself
// reject, so the grid must never generate one. A T_tight whose floor already
// exceeds mergeSpec.Max (only possible with a deliberately narrow mergeSpec)
// contributes zero T_merge values and is silently skipped for that gap.
//
// Returns ok=false without scanning when len(vecs) > TwoPassMaxFaces (the
// current tier holds rather than paying for the scan).
//
// ctx is checked for cancellation at least once per (gap, T_tight) iteration
// -- concretely, right before each GreedyMomentClusters call and right
// before each HACComplete call, since the grid can run for minutes on a
// large library and the in-service calibration runner invokes this on the
// app's long-lived context, which must not block graceful shutdown. On
// cancellation this returns (nil, false) immediately, exactly like the
// TwoPassMaxFaces budget guard above -- callers distinguish the two via
// ctx.Err() (see runTwoPassTier in calibrate_run.go: a nil ctx.Err() means a
// genuine "no result", a non-nil one means shutdown, not insufficiency).
func TwoPassGridScan(ctx context.Context, vecs [][]float32, takenAt, indexedAt []time.Time,
	idOf map[string]int, named []NamedTruth,
	tightSpec, mergeSpec ThresholdSpec) ([]TwoPassCombo, bool) {
	if len(vecs) > TwoPassMaxFaces {
		return nil, false
	}

	tTightGrid := twoPassFrange(tightSpec.Min, tightSpec.Max, 0.01)

	var allResults []TwoPassCombo
	for _, gap := range twoPassGaps {
		moments := SegmentMoments(takenAt, indexedAt, gap)

		for _, tt := range tTightGrid {
			if ctx.Err() != nil {
				return nil, false
			}
			// GreedyMomentClusters depends only on (vecs, moments, T_tight),
			// never on T_merge, so it is computed once here and reused across
			// the entire T_merge sub-loop below.
			pass1 := GreedyMomentClusters(vecs, moments, tt)

			mergeLo := math.Max(twoPassRound2(tt+0.10), mergeSpec.Min)
			for _, tm := range twoPassFrange(mergeLo, mergeSpec.Max, 0.01) {
				if ctx.Err() != nil {
					return nil, false
				}
				labels := HACComplete(vecs, pass1, tm)
				allResults = append(allResults, EvalTwoPassCombo(labels, gap, tt, tm, idOf, named))
			}
		}
	}
	return allResults, true
}

// SelectTwoPassCombo returns the winning combo after rs has already been
// sorted by SortTwoPassResults: the first result, provided it has
// Purity >= 1.0 (the hard purity==1.0 selection filter). An empty grid, or a
// grid where no combo reaches purity 1.0, yields ok=false.
func SelectTwoPassCombo(rs []TwoPassCombo) (TwoPassCombo, bool) {
	if len(rs) == 0 || rs[0].Purity < 1.0 {
		return TwoPassCombo{}, false
	}
	return rs[0], true
}
