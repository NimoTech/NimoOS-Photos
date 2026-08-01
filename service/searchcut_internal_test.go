package service

import "testing"

// semanticCutIndex computes, over a score-descending "semantic hit
// subsequence", how many leading entries the best-match tier should keep
// (a return value == len(scores) means no tiering). The cut rule is
// priority-based rather than "stricter wins": a significant cliff wins the
// cut when present, and only falls back to the relative threshold when no
// cliff is significant — see the comment atop searchcut.go. Cases cover the
// scenarios listed in spec §2: textbook cliff / uniform distribution no
// tiering / <3 entries no tiering / all high scores no tiering / cliff not
// significant so relative threshold applies / significant cliff outranks
// relative threshold (production regression).
func TestSemanticCutIndex(t *testing.T) {
	cases := []struct {
		name   string
		scores []float64
		alpha  float64
		want   int
	}{
		{
			// "fish" measured in production: 4 real hits 0.66~0.86, then a
			// cliff down to 0.13 unrelated images. The relative threshold
			// 0.7×0.86=0.602 and the cliff signal (max gap 0.53 at index3,
			// far above max(0.10, 2×mean)=0.292) trigger at the same spot,
			// so old and new rules give the same result.
			name:   "textbook cliff",
			scores: []float64{0.86, 0.80, 0.72, 0.66, 0.13, 0.13},
			alpha:  0.7,
			want:   4,
		},
		{
			// Production acceptance regression case (Critical): real fish
			// search score sequence
			// [0.953,0.864,0.86,0.744,0.662, 0.131,0.131,0.131].
			// Relative threshold 0.7×0.953=0.6671; 0.662 falls just 0.005
			// below it, giving relCut=4 — under the old "stricter wins" rule
			// this would be folded away. But the gap 0.662→0.131 of 0.531 far
			// exceeds the significant-cliff threshold max(0.10, 2×mean≈0.235),
			// giving cliffCut=5 with a significant cliff, so the new rule
			// should take the cliff: the cut lands after 0.662, locking the
			// best tier at 5 entries.
			name:   "significant_cliff_outranks_relative_threshold_fish_regression",
			scores: []float64{0.953, 0.864, 0.86, 0.744, 0.662, 0.131, 0.131, 0.131},
			alpha:  0.7,
			want:   5,
		},
		{
			name:   "uniform distribution no tiering",
			scores: []float64{0.50, 0.48, 0.46, 0.44, 0.42},
			alpha:  0.7,
			want:   5,
		},
		{
			name:   "fewer_than_3_no_tiering_2_entries",
			scores: []float64{0.90, 0.10},
			alpha:  0.7,
			want:   2,
		},
		{
			name:   "fewer_than_3_no_tiering_1_entry",
			scores: []float64{0.90},
			alpha:  0.7,
			want:   1,
		},
		{
			name:   "all high scores no tiering",
			scores: []float64{0.90, 0.88, 0.85, 0.83, 0.80},
			alpha:  0.7,
			want:   5,
		},
		{
			// Adjacent gap is a constant 0.05, far below the significant-cliff
			// threshold max(0.10, 2×mean=0.10), but cumulative decay crosses
			// the relative threshold 0.7×1.00=0.70 at index7, so only the
			// relative threshold applies.
			name:   "cliff not significant only relative threshold applies",
			scores: []float64{1.00, 0.95, 0.90, 0.85, 0.80, 0.75, 0.70, 0.65, 0.60},
			alpha:  0.7,
			want:   7,
		},
		{
			name:   "empty slice no tiering",
			scores: nil,
			alpha:  0.7,
			want:   0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := semanticCutIndex(c.scores, c.alpha)
			if got != c.want {
				t.Errorf("semanticCutIndex(%v, %v) = %d, want %d", c.scores, c.alpha, got, c.want)
			}
		})
	}
}

// searchCutAlpha falls back to the default 0.7 when unconfigured (config not
// initialized, as when this test calls it directly).
func TestSearchCutAlphaDefault(t *testing.T) {
	if got := searchCutAlpha(); got != defaultSearchCutAlpha {
		t.Errorf("searchCutAlpha() = %v, want default %v", got, defaultSearchCutAlpha)
	}
}
