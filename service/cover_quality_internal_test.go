package service

import (
	"database/sql"
	"testing"
)

func TestHybridCoverScoreUsesFaceQuality(t *testing.T) {
	bbox := `{"x1":0,"y1":0,"x2":100,"y2":100}`
	aest := sql.NullFloat64{Float64: 8.0, Valid: true}
	w := sql.NullInt64{Int64: 200, Valid: true}
	h := sql.NullInt64{Int64: 200, Valid: true}

	sharp := hybridCoverScore(aest, bbox, w, h, sql.NullFloat64{Float64: 0.95, Valid: true})
	blurry := hybridCoverScore(aest, bbox, w, h, sql.NullFloat64{Float64: 0.30, Valid: true})
	legacy := hybridCoverScore(aest, bbox, w, h, sql.NullFloat64{})

	if !(sharp > blurry) {
		t.Fatalf("high-quality face must outscore low-quality: %v vs %v", sharp, blurry)
	}
	if legacy != sharp/0.95 {
		t.Fatalf("NULL score must be neutral (x1.0): got %v", legacy)
	}
}
