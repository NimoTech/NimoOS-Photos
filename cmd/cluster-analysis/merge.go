package main

import (
	"database/sql"
	"fmt"
	"io"
	"log"
	"sort"

	"github.com/NimoTech/NimoOS-Photos/service"
)

// ── -mode merge: T-merge cluster-merge cut-point calibration ───────────────
//
// Turns the accumulated, user-decided cluster-merge questions
// (merge_suggestions rows with status accepted/rejected, pkg/sqlite/db.go)
// into a proposed ClusterMergeEps cut point. The truth loading, insufficient-
// data bars, and cut-point selection logic itself lives in
// service/calibrate_merge.go (shared with the in-service calibration
// runner) -- this file keeps only report printing and -mode merge's entry
// point. See README.md's "T-merge cut-point calibration" section for the
// full write-up.

// mergeDistStats mirrors knnDistStats (count/min/q1/median/q3/max) but over
// a plain []float64 -- MergeTruth's AcceptedDists/RejectedDists are already
// raw distances, unlike KNNTruthSet's richer []KNNTruthRow.
type mergeDistStats struct {
	Count               int
	Min, Q1, Median, Q3 float64
	Max                 float64
}

func computeMergeDistStats(vals []float64) mergeDistStats {
	if len(vals) == 0 {
		return mergeDistStats{}
	}
	sorted := append([]float64{}, vals...)
	sort.Float64s(sorted)
	return mergeDistStats{
		Count:  len(sorted),
		Min:    sorted[0],
		Q1:     percentile(sorted, 0.25),
		Median: percentile(sorted, 0.50),
		Q3:     percentile(sorted, 0.75),
		Max:    sorted[len(sorted)-1],
	}
}

// printMergeDistribution reuses knnHistogram's coarse ASCII bucketing
// (defined in knn.go) directly over vals -- unlike printKNNDistribution, no
// []service.KNNTruthRow -> []float64 unwrap is needed since MergeTruth's
// fields already are []float64.
func printMergeDistribution(w io.Writer, vals []float64) {
	if len(vals) == 0 {
		fmt.Fprintln(w, "  (no usable rows)")
		return
	}
	s := computeMergeDistStats(vals)
	fmt.Fprintf(w, "  count=%d min=%.4f q1=%.4f median=%.4f q3=%.4f max=%.4f\n",
		s.Count, s.Min, s.Q1, s.Median, s.Q3, s.Max)
	for _, line := range knnHistogram(vals) {
		fmt.Fprintf(w, "  %s\n", line)
	}
}

// mergeWarningBlock renders the prominent "not trustworthy yet" warning,
// mirroring knnWarningBlock's shape but against the four T-merge bars.
func mergeWarningBlock(nAccepted, nRejected, nPersons int) string {
	decided := nAccepted + nRejected
	return fmt.Sprintf(`
!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!
!! INSUFFICIENT DATA -- this recommendation is NOT trustworthy yet.
!!   decided (accepted+rejected) = %4d  (need >= %d)
!!   accepted                    = %4d  (need >= %d)
!!   rejected                    = %4d  (need >= %d)
!!   distinct persons            = %4d  (need >= %d)
!! The distributions above are still useful early signal, but do not adopt
!! the recommended cut point below until ground truth accumulates past
!! these bars.
!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!`,
		decided, service.MergeMinDecided,
		nAccepted, service.MergeMinAccepted,
		nRejected, service.MergeMinRejected,
		nPersons, service.MergeMinPersons)
}

// printMergeRecommendation prints the selection criterion and its winning
// cut point (or the no-valid-cut message), followed by the insufficient-data
// warning when it applies -- same trailing-warning convention as
// printKNNRecommendation.
func printMergeRecommendation(w io.Writer, truth service.MergeTruth, insufficient bool) {
	fmt.Fprintln(w, "\n-- Selection criterion: largest accepted dist strictly below every rejected dist (zero-false-accept style) --")
	if cut, ok := service.MergeCutPoint(truth); ok {
		fmt.Fprintf(w, "  RECOMMENDED: ClusterMergeEps=%.4f\n", cut)
	} else {
		fmt.Fprintln(w, "  NO valid cut point -- no accepted dist lies strictly below every rejected dist (or one of the two truth sets is empty).")
	}
	if insufficient {
		fmt.Fprintln(w, mergeWarningBlock(len(truth.AcceptedDists), len(truth.RejectedDists), len(truth.DistinctPersons)))
	}
}

// runMerge is -mode merge's entry point. w is the report writer (os.Stdout
// from main(), a buffer from tests) -- same convention as runKNN.
func runMerge(w io.Writer, db *sql.DB) {
	truth, err := service.LoadMergeTruth(db)
	if err != nil {
		log.Fatalf("merge: %v", err)
	}

	fmt.Fprintln(w, "\n=== T-MERGE CALIBRATION: loading decided cluster-merge questions ===")
	fmt.Fprintf(w, "Accepted: %d  Rejected: %d  Distinct persons: %d\n",
		len(truth.AcceptedDists), len(truth.RejectedDists), len(truth.DistinctPersons))

	insufficient := service.MergeInsufficient(truth)
	if insufficient {
		fmt.Fprintln(w, mergeWarningBlock(len(truth.AcceptedDists), len(truth.RejectedDists), len(truth.DistinctPersons)))
	}

	fmt.Fprintln(w, "\n=== Accepted dist distribution (user confirmed: same person, merge) ===")
	printMergeDistribution(w, truth.AcceptedDists)
	fmt.Fprintln(w, "\n=== Rejected dist distribution (user confirmed: different people, do not merge) ===")
	printMergeDistribution(w, truth.RejectedDists)

	printMergeRecommendation(w, truth, insufficient)
}
