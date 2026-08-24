package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/NimoTech/NimoOS-Photos/service"
)

// twopassGaps mirrors service's unexported twoPassGaps: the fixed moment-
// segmentation gaps TwoPassGridScan sweeps, kept here only so this file's
// per-gap report loop knows the gap set and its print order (service.
// TwoPassGridScan itself owns the actual sweep).
var twopassGaps = []time.Duration{30 * time.Minute, 60 * time.Minute, 120 * time.Minute}

// twopassTightSpec/twopassMergeSpec mirror service/calibrate_profile.go's
// builtinFactoryProfile() ClusterTightEps/ClusterMergeEps bands as of this
// writing ({Default:0.35,Min:0.28,Max:0.42} / {Default:0.55,Min:0.48,
// Max:0.65}). Hardcoded as literals because builtinFactoryProfile is
// unexported from service (same CLI-side-mirror pattern as the other
// -mode flags in this tool) -- keep these two in sync with that function if
// its bounds ever change.
var (
	twopassTightSpec = service.ThresholdSpec{Default: 0.35, Min: 0.28, Max: 0.42}
	twopassMergeSpec = service.ThresholdSpec{Default: 0.55, Min: 0.48, Max: 0.65}
)

func printTwoPassTable(rs []service.TwoPassCombo) {
	fmt.Printf("%7s %7s %7s %8s %8s %8s %6s\n", "gap", "Ttight", "Tmerge", "#clust", "maxSize", "purity", "frag#")
	for _, r := range rs {
		fmt.Printf("%7s %7.2f %7.2f %8d %8d %8.3f %6d\n", r.Gap, r.TTight, r.TMerge, r.NumClusters, r.MaxSize, r.Purity, r.FragCount)
	}
}

// printTwoPassRecommendation prints the selection criterion verbatim
// alongside the combo it picks out of rs (rs must already be sorted by
// service.SortTwoPassResults, so rs[0] is the winner when one exists).
func printTwoPassRecommendation(label string, rs []service.TwoPassCombo) {
	fmt.Printf("\n-- Selection criterion (%s): among purity==1.0 combos, minimize named-person fragment count, tie-break on smaller max cluster --\n", label)
	if len(rs) == 0 || rs[0].Purity < 1.0 {
		fmt.Println("  NO combo in this grid reaches purity=1.0 -- widen the T_tight/T_merge grid before picking a default.")
		return
	}
	best := rs[0]
	fmt.Printf("  RECOMMENDED: gap=%s T_tight=%.2f T_merge=%.2f  #clusters=%d maxSize=%d purity=%.3f fragCount=%d\n",
		best.Gap, best.TTight, best.TMerge, best.NumClusters, best.MaxSize, best.Purity, best.FragCount)
}

// runTwoPass is -mode twopass's entry point: builds the vecs/idOf/named
// adapter inputs from this tool's Face/namedPerson structs, delegates the
// actual T_tight x T_merge x gap grid scan to service.TwoPassGridScan (the
// eval/sort/majority/grid-driver logic all now lives there, shared with the
// in-service calibration runner), then prints the same per-gap plus overall
// report this tool always has.
func runTwoPass(faces []Face, named []namedPerson) {
	N := len(faces)
	vecs := make([][]float32, N)
	takenAt := make([]time.Time, N)
	indexedAt := make([]time.Time, N)
	for i, f := range faces {
		vecs[i] = f.Vec
		takenAt[i] = f.TakenAt
		indexedAt[i] = f.IndexedAt
	}
	idOf := make(map[string]int, N)
	for i, f := range faces {
		idOf[f.ID] = i
	}
	namedTruth := make([]service.NamedTruth, len(named))
	for i, np := range named {
		namedTruth[i] = service.NamedTruth{PersonID: np.id, FaceIDs: np.faceIDs}
	}

	fmt.Printf("\n=== TWO-PASS GRID CALIBRATION: %d faces, T_tight in [%.2f,%.2f] step 0.01 x T_merge in [T_tight+0.10,%.2f] step 0.01 across gap in {30,60,120}min ===\n",
		N, twopassTightSpec.Min, twopassTightSpec.Max, twopassMergeSpec.Max)
	// NOTE: this tool's historical fixed grid bounds were T_tight in
	// [0.30,0.40] and T_merge in [0.45,0.60] (Task 6). Now that the bounds
	// come from the profile's ClusterTightEps/ClusterMergeEps spec (Task 7),
	// they widen slightly to [0.28,0.42] and [T_tight+0.10,0.65] -- see this
	// commit's message for the exact before/after grid sizes.

	// This CLI runs once and exits (no graceful-shutdown signal to honor),
	// so it always passes an uncancellable context -- ctx cancellation only
	// matters to the in-service calibration runner's long-lived context
	// (see service.TwoPassGridScan's doc comment).
	allResults, ok := service.TwoPassGridScan(context.Background(), vecs, takenAt, indexedAt, idOf, namedTruth, twopassTightSpec, twopassMergeSpec)
	if !ok {
		fmt.Fprintf(os.Stderr, "twopass: %d faces exceeds the grid-scan budget (service.TwoPassMaxFaces=%d) -- skipped\n", N, service.TwoPassMaxFaces)
		return
	}

	byGap := make(map[time.Duration][]service.TwoPassCombo, len(twopassGaps))
	for _, r := range allResults {
		byGap[r.Gap] = append(byGap[r.Gap], r)
	}

	for _, gap := range twopassGaps {
		moments := service.SegmentMoments(takenAt, indexedAt, gap)
		numMoments := 0
		for _, m := range moments {
			if m+1 > numMoments {
				numMoments = m + 1
			}
		}
		fmt.Printf("\n=== TWO-PASS GRID: gap=%s (%d moments) ===\n", gap, numMoments)

		gapResults := byGap[gap]
		service.SortTwoPassResults(gapResults)
		printTwoPassTable(gapResults)
		printTwoPassRecommendation(fmt.Sprintf("gap=%s", gap), gapResults)
	}

	fmt.Println("\n=== TWO-PASS OVERALL RECOMMENDATION (across gap=30/60/120min) ===")
	service.SortTwoPassResults(allResults)
	printTwoPassRecommendation("overall, all gaps", allResults)
}
