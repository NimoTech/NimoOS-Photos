package service

import "github.com/NimoTech/NimoOS-Photos/pkg/config"

// defaultSearchCutAlpha is the relative-threshold multiplier used by
// semanticCutIndex when photos.SearchCutAlpha is unset in the config file or
// when config is not initialized (mirrors defaultSimDisplayFloor/Ceil's
// test-friendly default handling in scan.go).
const defaultSearchCutAlpha = 0.7

// searchCutAlpha returns the currently effective relative-threshold
// multiplier (photos.SearchCutAlpha in the config file).
func searchCutAlpha() float64 {
	if config.Cfg != nil && config.Cfg.SearchCutAlpha > 0 {
		return config.Cfg.SearchCutAlpha
	}
	return defaultSearchCutAlpha
}

// semanticCutIndex computes, over a semantic-hit score subsequence already
// sorted descending, how many leading entries belong to the "best match"
// tier. A return value equal to len(scores) means no tiering — every result
// stays in the best-match tier.
//
// Two independent signals feed the decision, but they are combined by
// priority rather than by picking whichever cuts earlier ("stricter wins"): a
// significant cliff is a materially stronger relevance signal than the
// relative threshold, because a cliff reflects an actual bimodal split in
// the score distribution, whereas the relative threshold is a fixed
// fraction of Top-1 that can slice straight through the middle of a single
// cluster of genuinely relevant hits. Production evidence: a real match
// scored 0.662 against Top-1 0.953 (alpha 0.7 → relative line 0.667) was
// folded away by the relative line by a margin of just 0.005, even though
// the actual cliff sat one position further down (0.662 → 0.131, a gap far
// larger than anything above it) — the cliff was clearly the correct cut,
// but the old "take whichever is stricter" rule let the relative line win
// instead. The rule is now:
//
//  1. Cliff detection: the position after the largest adjacent gap
//     d_i = scores[i]-scores[i+1], but only when that gap is "significant":
//     d_g >= max(0.10, 2*mean(d)). When significant, this is the cut — the
//     relative threshold is not consulted at all.
//  2. Relative threshold (fallback, only when no cliff is significant): the
//     index of the first score below alpha*scores[0].
//
// Boundary guards: fewer than 3 scores never tier (the KNN long tail needs a
// minimum sample to be meaningfully bimodal); the best-match tier always
// keeps at least 1 semantic hit when there is at least one score.
func semanticCutIndex(scores []float64, alpha float64) int {
	n := len(scores)
	if n < 3 {
		return n
	}

	// Signal 1: cliff detection, gated on statistical significance.
	cliffCut := n
	significant := false
	var sum float64
	maxDiff := -1.0
	maxIdx := -1
	for i := 0; i < n-1; i++ {
		d := scores[i] - scores[i+1]
		sum += d
		if d > maxDiff {
			maxDiff = d
			maxIdx = i
		}
	}
	mean := sum / float64(n-1)
	sigThreshold := 0.10
	if 2*mean > sigThreshold {
		sigThreshold = 2 * mean
	}
	if maxDiff >= sigThreshold {
		significant = true
		cliffCut = maxIdx + 1
	}

	cut := cliffCut
	if !significant {
		// Signal 2 (fallback): relative Top-1 threshold.
		relCut := n
		threshold := alpha * scores[0]
		for i := 1; i < n; i++ {
			if scores[i] < threshold {
				relCut = i
				break
			}
		}
		cut = relCut
	}

	if cut < 1 {
		cut = 1
	}
	return cut
}

// applyCutTiering marks the tail of the semantic-hit subsequence in assets
// with BelowCut=true per semanticCutIndex. OCR hits (MatchedBy=="ocr") are
// left untouched — they are excluded from the cut computation entirely and
// always stay in the best-match tier (see semanticCutIndex's doc and the
// design rationale: OCR hits are pinned at score 1.0 and always sort first,
// so including them would place the cliff right between OCR and semantic
// hits, dumping genuinely relevant semantic matches into the folded tier).
//
// assets is expected to already be in its final display order (i.e. called
// after mergeOCRFirst), with semantic hits in descending-score order among
// themselves — true of SmartSearch's KNN ordering.
func applyCutTiering(assets []Asset) {
	var semIdx []int
	var scores []float64
	for i, a := range assets {
		if a.MatchedBy != "semantic" || a.MatchScore == nil {
			continue
		}
		semIdx = append(semIdx, i)
		scores = append(scores, *a.MatchScore)
	}
	cut := semanticCutIndex(scores, searchCutAlpha())
	for i := cut; i < len(semIdx); i++ {
		assets[semIdx[i]].BelowCut = true
	}
}
