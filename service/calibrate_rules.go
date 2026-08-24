package service

import (
	"fmt"
	"math"
)

// invariantEps tolerates binary-float arithmetic artifacts (e.g. 0.55-0.05
// not landing on exactly 0.50) so a legal boundary case is never flipped to
// a false violation by float representation error.
const invariantEps = 1e-9

// adjustOutcome is the result of a single boundAdjust call. Its string
// values must match the calibration_history table's CHECK constraint
// (Task 1): "applied", "clamped", "held_hysteresis".
type adjustOutcome string

const (
	adjustApplied    adjustOutcome = "applied"
	adjustClamped    adjustOutcome = "clamped"
	adjustHysteresis adjustOutcome = "held_hysteresis"
)

// boundAdjust moves current toward suggested under the commercial safety
// rules, in this exact order:
//  1. clamp suggested into [spec.Min, spec.Max]   (outcome becomes clamped
//     when this changed the value);
//  2. limit the move to at most maxStep from current;
//  3. safety clamp: re-clamp the step-limited result into [spec.Min,
//     spec.Max]. This matters when current itself starts outside the band
//     (e.g. a hot-updated profile narrowed the band under an existing
//     calibrated value) — stepping from an out-of-range current toward an
//     in-range suggestion can otherwise still overshoot back out of range;
//  4. hysteresis: |result - current| < minDelta -> return (current,
//     held_hysteresis) — nothing is written for this key. Hysteresis only
//     ever suppresses a move when current is already within [spec.Min,
//     spec.Max]; when current starts outside that band, hysteresis is
//     bypassed so the value comes back in range in this single call, and
//     the outcome is reported as clamped.
//
// Postcondition: whenever the outcome is adjustApplied or adjustClamped,
// the returned value is guaranteed to lie within [spec.Min, spec.Max].
//
// A suggested value that is NaN or +/-Inf is treated as poisoned input and
// never touches the adjustment logic: it is guarded up front and reported
// as held_hysteresis (current unchanged) so it can never be persisted into
// calibration_state.
//
// Outcome precedence: hysteresis > clamped > applied.
func boundAdjust(current, suggested float64, spec ThresholdSpec, maxStep, minDelta float64) (float64, adjustOutcome) {
	// Guard: a poisoned suggestion (NaN/Inf) must never enter calibration
	// state. Treat it as if nothing were suggested this run.
	if math.IsNaN(suggested) || math.IsInf(suggested, 0) {
		return current, adjustHysteresis
	}

	// Step 1: clamp into [Min, Max].
	clamped := suggested
	wasClamped := false
	if clamped < spec.Min {
		clamped = spec.Min
		wasClamped = true
	} else if clamped > spec.Max {
		clamped = spec.Max
		wasClamped = true
	}

	// Step 2: limit the move to at most maxStep from current.
	result := clamped
	delta := result - current
	if delta > maxStep {
		result = current + maxStep
	} else if delta < -maxStep {
		result = current - maxStep
	}

	// Step 3: safety clamp — re-clamp the step-limited result into
	// [Min, Max], covering the case where current started outside the
	// band and stepping toward it still overshoots.
	if result < spec.Min {
		result = spec.Min
		wasClamped = true
	} else if result > spec.Max {
		result = spec.Max
		wasClamped = true
	}

	// current outside [Min, Max] is a safety condition on its own: bring
	// it back in range in this single call, bypassing hysteresis.
	if current < spec.Min || current > spec.Max {
		return result, adjustClamped
	}

	// Step 4: hysteresis takes precedence over everything else (current
	// is within range here, so suppressing a too-small move is safe).
	move := result - current
	if move < 0 {
		move = -move
	}
	if move < minDelta {
		return current, adjustHysteresis
	}

	if wasClamped {
		return result, adjustClamped
	}
	return result, adjustApplied
}

// checkCalibrationInvariants validates the WOULD-BE effective value set
// (all five keys; unchanged ones filled with their current effective
// values). Returns a non-nil error naming the violated invariant:
//
//	AssignAutoDist  <= AssignSuggestDist - 0.05
//	ClusterTightEps <= ClusterMergeEps  - 0.10
//
// Boundary equality is legal (<= semantics); invariantEps tolerates
// binary-float artifacts so a legal boundary case is never rejected.
func checkCalibrationInvariants(vals map[string]float64) error {
	if v, ok := vals["AssignAutoDist"]; ok {
		if s, ok := vals["AssignSuggestDist"]; ok {
			if v > s-0.05+invariantEps {
				return fmt.Errorf("calibration invariant violated: AssignAutoDist (%.4f) must be <= AssignSuggestDist (%.4f) - 0.05", v, s)
			}
		}
	}
	if v, ok := vals["ClusterTightEps"]; ok {
		if m, ok := vals["ClusterMergeEps"]; ok {
			if v > m-0.10+invariantEps {
				return fmt.Errorf("calibration invariant violated: ClusterTightEps (%.4f) must be <= ClusterMergeEps (%.4f) - 0.10", v, m)
			}
		}
	}
	return nil
}

// dominantShare returns the largest single-person share among the truth
// rows' PersonID field (0 when empty). The knn tier holds as held_skewed
// when this exceeds 0.60 over its positives (exactly 0.60 is not skewed;
// callers must use a strict > 0.60 comparison).
func dominantShare(personIDs []string) float64 {
	if len(personIDs) == 0 {
		return 0
	}
	counts := make(map[string]int, len(personIDs))
	for _, id := range personIDs {
		counts[id]++
	}
	max := 0
	for _, c := range counts {
		if c > max {
			max = c
		}
	}
	return float64(max) / float64(len(personIDs))
}
