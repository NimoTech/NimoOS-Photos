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

	sharp := hybridCoverScore(aest, bbox, w, h, 0.95)
	blurry := hybridCoverScore(aest, bbox, w, h, 0.30)

	if !(sharp > blurry) {
		t.Fatalf("high-quality face must outscore low-quality: %v vs %v", sharp, blurry)
	}
	// NULL-neutrality (quality=1.0) is now faceQualityFactor's responsibility;
	// see TestFaceQualityFactor for that assertion.
	legacy := hybridCoverScore(aest, bbox, w, h, 1.0)
	wantAest := (aest.Float64 - 1) / 9
	wantRatio := 100.0 * 100.0 / (200.0 * 200.0)
	if legacy != wantAest*wantRatio {
		t.Fatalf("quality=1.0 must equal aest*ratio: got %v, want %v", legacy, wantAest*wantRatio)
	}
}
