package main

import (
	"database/sql"
	"fmt"
	"io"
	"log"
	"math"
	"sort"
	"strings"

	"github.com/NimoTech/NimoOS-Photos/service"
)

// ── -mode knn: KNN exemplar-assignment threshold calibration ────────────────
//
// Turns accumulated ground truth -- face_person rows with confirmed=1
// (user-confirmed: this face IS this person) and person_negatives rows
// (user-rejected: this face is NOT this person) -- into AssignAutoDist /
// AssignSuggestDist recommendations for the KNN exemplar matcher
// (service/matcher.go, service/persons.go's assignAutoDist/assignSuggestDist/
// assignK). See README.md's "KNN threshold calibration" section for the
// full write-up.
//
// The truth loading, grid scan, sort, and selection logic itself now lives
// in service/calibrate_knn.go (shared with the in-service calibration
// runner) -- this file keeps only report printing and -mode knn's entry
// point.

// knnDistStats is the requirement's count/min/q1/median/q3/max summary of a
// distance distribution.
type knnDistStats struct {
	Count               int
	Min, Q1, Median, Q3 float64
	Max                 float64
}

func computeKNNDistStats(rows []service.KNNTruthRow) knnDistStats {
	if len(rows) == 0 {
		return knnDistStats{}
	}
	vals := make([]float64, len(rows))
	for i, r := range rows {
		vals[i] = r.Dist
	}
	sort.Float64s(vals)
	return knnDistStats{
		Count:  len(vals),
		Min:    vals[0],
		Q1:     percentile(vals, 0.25),
		Median: percentile(vals, 0.50),
		Q3:     percentile(vals, 0.75),
		Max:    vals[len(vals)-1],
	}
}

// knnReportTAutoLo/Hi and knnReportTSugMargin/Hi mirror service/calibrate_knn.go's
// private grid-bound constants (knnTAutoLo/Hi, knnTSugMargin/Hi), purely for
// this report header's descriptive text -- the actual scan bounds live only
// in service.KNNGridScan; these must be kept in sync with it by hand.
const (
	knnReportTAutoLo    = 0.35
	knnReportTAutoHi    = 0.55
	knnReportTAutoStep  = 0.01
	knnReportTSugMargin = 0.05
	knnReportTSugHi     = 0.70
)

// knnHistogramBinWidth/Hi bucket the [0, 1]-ish cosine-distance range into
// 0.05-wide bins for the coarse ASCII histogram, with a final overflow bin
// for anything >= knnHistogramHi (cosine distance can in principle reach up
// to 2.0 for anti-parallel vectors, but real face embeddings essentially
// never do).
const (
	knnHistogramBinWidth = 0.05
	knnHistogramHi       = 1.00
	knnHistogramBarWidth = 40
)

// knnHistogram renders a coarse fixed-width ASCII histogram of vals bucketed
// into knnHistogramBinWidth-wide bins from 0 to knnHistogramHi, with a final
// overflow bin. Returns one line per bin, longest bar knnHistogramBarWidth
// characters.
func knnHistogram(vals []float64) []string {
	nBins := int(math.Ceil(knnHistogramHi/knnHistogramBinWidth)) + 1 // +1 overflow bin
	counts := make([]int, nBins)
	for _, v := range vals {
		b := int(v / knnHistogramBinWidth)
		if b >= nBins-1 || v >= knnHistogramHi {
			b = nBins - 1
		}
		counts[b]++
	}
	maxCount := 0
	for _, c := range counts {
		if c > maxCount {
			maxCount = c
		}
	}
	lines := make([]string, nBins)
	for i, c := range counts {
		var label string
		if i == nBins-1 {
			label = fmt.Sprintf(">=%.2f", knnHistogramHi)
		} else {
			lo := float64(i) * knnHistogramBinWidth
			label = fmt.Sprintf("[%.2f,%.2f)", lo, lo+knnHistogramBinWidth)
		}
		barLen := 0
		if maxCount > 0 {
			barLen = c * knnHistogramBarWidth / maxCount
		}
		lines[i] = fmt.Sprintf("%-13s %6d %s", label, c, strings.Repeat("#", barLen))
	}
	return lines
}

func printKNNDistribution(w io.Writer, rows []service.KNNTruthRow) {
	if len(rows) == 0 {
		fmt.Fprintln(w, "  (no usable rows)")
		return
	}
	s := computeKNNDistStats(rows)
	fmt.Fprintf(w, "  count=%d min=%.4f q1=%.4f median=%.4f q3=%.4f max=%.4f\n",
		s.Count, s.Min, s.Q1, s.Median, s.Q3, s.Max)
	vals := make([]float64, len(rows))
	for i, r := range rows {
		vals[i] = r.Dist
	}
	for _, line := range knnHistogram(vals) {
		fmt.Fprintf(w, "  %s\n", line)
	}
}

func printKNNGridTable(w io.Writer, rs []service.KNNComboResult) {
	fmt.Fprintf(w, "%7s %8s %6s %6s %7s %7s %6s\n", "Tauto", "Tsuggest", "FA", "TA", "grayP", "grayN", "miss")
	for _, r := range rs {
		fmt.Fprintf(w, "%7.2f %8.2f %6d %6d %7d %7d %6d\n",
			r.TAuto, r.TSuggest, r.FalseAccept, r.TrueAccept, r.GrayPositives, r.GrayNegatives, r.Miss)
	}
}

// knnWarningBlock renders the prominent "not trustworthy yet" warning,
// showing current counts against the bars per the requirement.
func knnWarningBlock(nPositives, nNegatives, nPersons int) string {
	return fmt.Sprintf(`
!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!
!! INSUFFICIENT DATA -- this recommendation is NOT trustworthy yet.
!!   positives      = %4d  (need >= %d)
!!   negatives      = %4d  (need >= %d)
!!   distinct persons = %2d  (need >= %d)
!! The distributions above are still useful early signal, but do not adopt
!! the recommended thresholds below until ground truth accumulates past
!! these bars.
!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!`,
		nPositives, service.KNNMinPositives, nNegatives, service.KNNMinNegatives, nPersons, service.KNNMinPersons)
}

// printKNNRecommendation prints the selection criterion and its winner (rs
// must already be sorted by service.SortKNNResults), followed by the
// insufficient-data warning when it applies -- the warning always trails the
// recommendation, per the requirement that the recommendation block itself
// "carry" the warning.
func printKNNRecommendation(w io.Writer, rs []service.KNNComboResult, insufficient bool, nPositives, nNegatives, nPersons int) {
	fmt.Fprintln(w, "\n-- Selection criterion: zero false-accepts in the auto zone -> maximize true-accepts, tie-break larger gray coverage of negatives --")
	if _, _, ok := service.SelectKNNCombo(rs); !ok {
		fmt.Fprintln(w, "  NO combo in this grid reaches zero false-accepts -- widen the T_auto grid downward before picking a default.")
	} else {
		best := rs[0]
		fmt.Fprintf(w, "  RECOMMENDED: T_auto=%.2f T_suggest=%.2f  falseAccept=%d trueAccept=%d grayPositives=%d grayNegatives=%d miss=%d\n",
			best.TAuto, best.TSuggest, best.FalseAccept, best.TrueAccept, best.GrayPositives, best.GrayNegatives, best.Miss)
	}
	if insufficient {
		fmt.Fprintln(w, knnWarningBlock(nPositives, nNegatives, nPersons))
	}
}

// runKNN is -mode knn's entry point. w is the report writer (os.Stdout from
// main(), a buffer from tests) -- unlike the rest of this tool's modes,
// which fmt.Printf straight to stdout, this mode takes an explicit writer so
// the insufficient-data warning and grid recommendation can be asserted on
// directly in tests without hijacking the process's real stdout. Unlike
// before the KNN core moved into service, this no longer takes the caller's
// already-loaded []Face -- service.LoadKNNTruth loads faces itself.
func runKNN(w io.Writer, db *sql.DB, k int) {
	truth, err := service.LoadKNNTruth(db, k)
	if err != nil {
		log.Fatalf("knn: %v", err)
	}

	fmt.Fprintf(w, "\n=== KNN CALIBRATION: loading truth (k=%d) ===\n", k)
	fmt.Fprintf(w, "Exemplar templates: %d persons, %d faces\n", truth.ExemplarPersons, truth.ExemplarFaces)
	fmt.Fprintf(w, "Confirmed positives: usable=%d skipped(face not loaded)=%d skipped(no exemplar after self-exclusion)=%d\n",
		len(truth.Positives), truth.PosSkippedNoFace, truth.PosSkippedNoExemplar)
	fmt.Fprintf(w, "Rejected negatives:  usable=%d skipped(face not loaded)=%d skipped(no exemplar after self-exclusion)=%d\n",
		len(truth.Negatives), truth.NegSkippedNoFace, truth.NegSkippedNoExemplar)
	fmt.Fprintf(w, "Distinct persons in usable truth: %d\n", len(truth.DistinctPersons))

	insufficient := service.KNNInsufficient(len(truth.Positives), len(truth.Negatives), len(truth.DistinctPersons))
	if insufficient {
		fmt.Fprintln(w, knnWarningBlock(len(truth.Positives), len(truth.Negatives), len(truth.DistinctPersons)))
	}

	fmt.Fprintln(w, "\n=== Positive distance distribution (confirmed=1 vs own person) ===")
	printKNNDistribution(w, truth.Positives)
	fmt.Fprintln(w, "\n=== Negative distance distribution (person_negatives, rejected pairs) ===")
	printKNNDistribution(w, truth.Negatives)

	fmt.Fprintf(w, "\n=== KNN grid scan: T_auto in [%.2f,%.2f] x T_suggest in [T_auto+%.2f,%.2f] (step %.2f) ===\n",
		knnReportTAutoLo, knnReportTAutoHi, knnReportTSugMargin, knnReportTSugHi, knnReportTAutoStep)
	results := service.KNNGridScan(truth.Positives, truth.Negatives)
	service.SortKNNResults(results)
	printKNNGridTable(w, results)
	printKNNRecommendation(w, results, insufficient, len(truth.Positives), len(truth.Negatives), len(truth.DistinctPersons))
}
