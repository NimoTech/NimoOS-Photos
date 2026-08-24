package service

import (
	"math"
	"strings"
	"testing"
)

func TestBoundAdjust(t *testing.T) {
	spec := ThresholdSpec{Default: 0.45, Min: 0.38, Max: 0.52}

	t.Run("within range and step, over minDelta -> applied", func(t *testing.T) {
		current := 0.45
		suggested := 0.46
		got, outcome := boundAdjust(current, suggested, spec, 0.02, 0.005)
		if outcome != adjustApplied {
			t.Fatalf("outcome = %v, want applied", outcome)
		}
		if got != suggested {
			t.Fatalf("value = %v, want %v", got, suggested)
		}
	})

	t.Run("suggested over Max clamps then applied", func(t *testing.T) {
		current := 0.50
		suggested := 0.60 // above spec.Max (0.52)
		// step is large enough to not itself bind after clamp
		got, outcome := boundAdjust(current, suggested, spec, 0.05, 0.005)
		if outcome != adjustClamped {
			t.Fatalf("outcome = %v, want clamped", outcome)
		}
		if got != spec.Max {
			t.Fatalf("value = %v, want %v (clamped to Max)", got, spec.Max)
		}
	})

	t.Run("suggested within range but move limited by maxStep -> applied", func(t *testing.T) {
		current := 0.45
		suggested := 0.50 // 0.05 away, but maxStep is 0.02
		got, outcome := boundAdjust(current, suggested, spec, 0.02, 0.005)
		if outcome != adjustApplied {
			t.Fatalf("outcome = %v, want applied", outcome)
		}
		want := 0.47
		if !floatEqualEps(got, want, 1e-9) {
			t.Fatalf("value = %v, want %v", got, want)
		}
	})

	t.Run("post-step move under minDelta -> held_hysteresis, value unchanged", func(t *testing.T) {
		current := 0.45
		suggested := 0.451 // within range and step, but tiny
		got, outcome := boundAdjust(current, suggested, spec, 0.02, 0.005)
		if outcome != adjustHysteresis {
			t.Fatalf("outcome = %v, want held_hysteresis", outcome)
		}
		if got != current {
			t.Fatalf("value = %v, want current %v", got, current)
		}
	})

	t.Run("clamp lands within minDelta of current -> hysteresis takes precedence over clamped", func(t *testing.T) {
		current := 0.52   // already at spec.Max
		suggested := 0.60 // clamps to spec.Max (0.52), same as current
		got, outcome := boundAdjust(current, suggested, spec, 0.05, 0.005)
		if outcome != adjustHysteresis {
			t.Fatalf("outcome = %v, want held_hysteresis (precedence over clamped)", outcome)
		}
		if got != current {
			t.Fatalf("value = %v, want current %v", got, current)
		}
	})

	// --- fix round 1: negative-direction coverage (finding 1) ---

	t.Run("suggested below current, step-limited downward -> applied", func(t *testing.T) {
		current := 0.45
		suggested := 0.40 // 0.05 below current, but maxStep is 0.02
		got, outcome := boundAdjust(current, suggested, spec, 0.02, 0.005)
		if outcome != adjustApplied {
			t.Fatalf("outcome = %v, want applied", outcome)
		}
		want := 0.43
		if !floatEqualEps(got, want, 1e-9) {
			t.Fatalf("value = %v, want %v", got, want)
		}
	})

	t.Run("suggested below Min clamps up -> clamped", func(t *testing.T) {
		current := 0.40
		suggested := 0.30 // below spec.Min (0.38)
		got, outcome := boundAdjust(current, suggested, spec, 0.05, 0.005)
		if outcome != adjustClamped {
			t.Fatalf("outcome = %v, want clamped", outcome)
		}
		if got != spec.Min {
			t.Fatalf("value = %v, want %v (clamped to Min)", got, spec.Min)
		}
	})

	// --- fix round 1: safety-clamp postcondition when current starts
	// outside [Min, Max] (finding 2) ---

	t.Run("current above Max, step-limited move still overshoots -> safety-clamped into range", func(t *testing.T) {
		current := 0.60 // outside [Min, Max] (band narrowed under it)
		suggested := 0.50
		got, outcome := boundAdjust(current, suggested, spec, 0.02, 0.005)
		if outcome != adjustClamped {
			t.Fatalf("outcome = %v, want clamped", outcome)
		}
		if got != spec.Max {
			t.Fatalf("value = %v, want %v (safety-clamped to Max)", got, spec.Max)
		}
	})

	t.Run("current above Max, tiny move -> clamped, NOT held by hysteresis", func(t *testing.T) {
		current := 0.525 // just outside [Min, Max]
		suggested := 0.52
		got, outcome := boundAdjust(current, suggested, spec, 0.02, 0.01)
		if outcome != adjustClamped {
			t.Fatalf("outcome = %v, want clamped (hysteresis must not hold an out-of-range current)", outcome)
		}
		if got != spec.Max {
			t.Fatalf("value = %v, want %v", got, spec.Max)
		}
	})

	t.Run("regression: in-range tiny move still held by hysteresis", func(t *testing.T) {
		current := 0.45 // within [Min, Max]
		suggested := 0.451
		got, outcome := boundAdjust(current, suggested, spec, 0.02, 0.005)
		if outcome != adjustHysteresis {
			t.Fatalf("outcome = %v, want held_hysteresis", outcome)
		}
		if got != current {
			t.Fatalf("value = %v, want current %v", got, current)
		}
	})

	// --- fix round 1: poisoned-suggestion guard (finding 3) ---

	t.Run("NaN suggested is guarded -> held_hysteresis, current unchanged", func(t *testing.T) {
		current := 0.45
		got, outcome := boundAdjust(current, math.NaN(), spec, 0.02, 0.005)
		if outcome != adjustHysteresis {
			t.Fatalf("outcome = %v, want held_hysteresis", outcome)
		}
		if got != current {
			t.Fatalf("value = %v, want current %v", got, current)
		}
	})

	t.Run("+Inf suggested is guarded -> held_hysteresis, current unchanged", func(t *testing.T) {
		current := 0.45
		got, outcome := boundAdjust(current, math.Inf(1), spec, 0.02, 0.005)
		if outcome != adjustHysteresis {
			t.Fatalf("outcome = %v, want held_hysteresis", outcome)
		}
		if got != current {
			t.Fatalf("value = %v, want current %v", got, current)
		}
	})
}

func TestCheckCalibrationInvariants(t *testing.T) {
	t.Run("assign gap violated (0.53-0.50 < 0.05)", func(t *testing.T) {
		vals := map[string]float64{
			"AssignAutoDist":    0.50,
			"AssignSuggestDist": 0.53,
			"ClusterTightEps":   0.35,
			"ClusterMergeEps":   0.55,
		}
		if err := checkCalibrationInvariants(vals); err == nil {
			t.Fatal("expected error for AssignAutoDist/AssignSuggestDist gap violation, got nil")
		}
	})

	t.Run("assign gap exactly at boundary is legal (<=)", func(t *testing.T) {
		vals := map[string]float64{
			"AssignAutoDist":    0.50,
			"AssignSuggestDist": 0.55,
			"ClusterTightEps":   0.35,
			"ClusterMergeEps":   0.55,
		}
		if err := checkCalibrationInvariants(vals); err != nil {
			t.Fatalf("expected boundary case to be legal, got error: %v", err)
		}
	})

	t.Run("cluster gap violated (0.55-0.46 < 0.10)", func(t *testing.T) {
		vals := map[string]float64{
			"AssignAutoDist":    0.45,
			"AssignSuggestDist": 0.60,
			"ClusterTightEps":   0.46,
			"ClusterMergeEps":   0.55,
		}
		if err := checkCalibrationInvariants(vals); err == nil {
			t.Fatal("expected error for ClusterTightEps/ClusterMergeEps gap violation, got nil")
		}
	})

	t.Run("cluster gap exactly at boundary is legal (<=)", func(t *testing.T) {
		vals := map[string]float64{
			"AssignAutoDist":    0.45,
			"AssignSuggestDist": 0.60,
			"ClusterTightEps":   0.45,
			"ClusterMergeEps":   0.55,
		}
		if err := checkCalibrationInvariants(vals); err != nil {
			t.Fatalf("expected boundary case to be legal, got error: %v", err)
		}
	})

	t.Run("violation error names the invariant", func(t *testing.T) {
		vals := map[string]float64{
			"AssignAutoDist":    0.50,
			"AssignSuggestDist": 0.53,
			"ClusterTightEps":   0.35,
			"ClusterMergeEps":   0.55,
		}
		err := checkCalibrationInvariants(vals)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "AssignAutoDist") || !strings.Contains(err.Error(), "AssignSuggestDist") {
			t.Fatalf("error %q does not name the violated invariant", err.Error())
		}
	})
}

func TestDominantShare(t *testing.T) {
	t.Run("empty returns 0", func(t *testing.T) {
		if got := dominantShare(nil); got != 0 {
			t.Fatalf("dominantShare(nil) = %v, want 0", got)
		}
		if got := dominantShare([]string{}); got != 0 {
			t.Fatalf("dominantShare([]) = %v, want 0", got)
		}
	})

	t.Run("3 of 5 same id -> 0.6, boundary is not >0.60 itself", func(t *testing.T) {
		ids := []string{"a", "a", "a", "b", "c"}
		got := dominantShare(ids)
		if !floatEqualEps(got, 0.6, 1e-9) {
			t.Fatalf("dominantShare = %v, want 0.6", got)
		}
		// pin the boundary semantics used by callers: exactly 0.60 is NOT skewed.
		if got > 0.60 {
			t.Fatalf("dominantShare = %v must not be > 0.60 for the 3/5 case", got)
		}
	})

	t.Run("all same id -> 1.0", func(t *testing.T) {
		ids := []string{"a", "a", "a"}
		got := dominantShare(ids)
		if !floatEqualEps(got, 1.0, 1e-9) {
			t.Fatalf("dominantShare = %v, want 1.0", got)
		}
	})

	t.Run("4 of 5 same id -> 0.8, exceeds 0.60 skew threshold", func(t *testing.T) {
		ids := []string{"a", "a", "a", "a", "b"}
		got := dominantShare(ids)
		if got <= 0.60 {
			t.Fatalf("dominantShare = %v, want > 0.60", got)
		}
	})
}

// floatEqualEps compares two floats with a tolerance, avoiding flaky
// equality checks on binary-float arithmetic artifacts.
func floatEqualEps(a, b, eps float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= eps
}
