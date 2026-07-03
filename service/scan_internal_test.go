package service

import "testing"

// displayScore rescales the raw cosine similarity produced by scanSearchAssets
// into a UI-friendly [0,1] band. The nllb-siglip model's noise floor
// (~0.02) and confident-match ceiling (~0.25) sit far below openai CLIP's
// old distribution, so the raw value must be recalibrated before it reaches
// the frontend's percentage display. OCR exact hits bypass the noise floor
// entirely (sim=1.0 already) and must stay pinned at 1.
func TestDisplayScore(t *testing.T) {
	cases := []struct {
		name string
		raw  float64
		want float64
	}{
		{"floor maps to 0", simDisplayFloor, 0},
		{"ceil maps to 1", simDisplayCeil, 1},
		{"OCR exact hit stays 1", 1.0, 1},
		{"midpoint maps to ~0.5", 0.135, 0.5},
		{"negative clamps to 0", -0.5, 0},
		{"above ceil clamps to 1", 0.9, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := displayScore(c.raw)
			if diff := got - c.want; diff > 1e-9 || diff < -1e-9 {
				t.Errorf("displayScore(%v) = %v, want %v", c.raw, got, c.want)
			}
		})
	}
}
