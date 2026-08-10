package service

import (
	"database/sql"
	"testing"
)

func nf(v float64) sql.NullFloat64 { return sql.NullFloat64{Float64: v, Valid: true} }

func TestFaceQualityFactor(t *testing.T) {
	// All NULL: fully neutral.
	if got := faceQualityFactor(sql.NullFloat64{}, sql.NullFloat64{}, sql.NullFloat64{}); got != 1.0 {
		t.Fatalf("all-NULL must be neutral 1.0, got %v", got)
	}
	// Frontal+sharp must outrank profile+blurred at equal detector score.
	good := faceQualityFactor(nf(0.9), nf(1.0), nf(0.9))
	bad := faceQualityFactor(nf(0.9), nf(0.1), nf(0.2))
	if !(good > bad) {
		t.Fatalf("quality signals must rank: good=%v bad=%v", good, bad)
	}
	// Range compression: even a zero signal halves, never annihilates.
	floor := faceQualityFactor(nf(1.0), nf(0.0), nf(0.0))
	if !(floor >= 0.25) {
		t.Fatalf("compressed floor must be >= 0.25, got %v", floor)
	}
	// Out-of-range detector score clamps (B8 semantics preserved).
	if got := faceQualityFactor(nf(1.7), sql.NullFloat64{}, sql.NullFloat64{}); got != 1.0 {
		t.Fatalf("detScore must clamp to 1.0, got %v", got)
	}
}
