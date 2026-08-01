package service

import (
	"math"

	"github.com/NimoTech/NimoOS-Photos/pkg/config"
)

// Pure computation for the OCR "document" classification hybrid criterion.
// On top of the density candidate gate (coverage/line_count), a weighted veto
// combining CLIP zero-shot semantic margin + line geometry regularity kills
// the "text-dense photo misclassified as OCR document" failure mode.
//
// Prompts are hardcoded as code constants (an algorithm implementation detail,
// not user config); their text embeddings are cached in the clip_text_cache
// table keyed by common.MLModelGen (see docverdict.go).
var docPrompts = []string{
	"a scan of a document",
	"a photo of a receipt",
	"a screenshot of a phone or computer screen",
	"a page of a book with text",
	"a whiteboard with handwriting",
	"an identity card or a paper form",
}

var photoPrompts = []string{
	"a photo of a restaurant menu",
	"a photo of a storefront with signs",
	"a poster on a wall",
	"a street scene with signs and billboards",
	"a natural photograph of people or scenery",
}

// Default weight/threshold values (overridable via photos.conf). Initial values
// are empirical; if adjusted after calibrating against the 91-photo production
// set, update both here and the config comments in sync.
const (
	defaultDocWSem       = 0.65
	defaultDocWGeo       = 0.35
	defaultDocScoreFloor = 0.5
	// SigLIP2 image-text cosine similarity itself has a small magnitude (strong
	// correlation ~0.09-0.13, see the calibration comment in scan.go), so the
	// doc-group vs photo-group margin is even smaller; [-0.01, 0.05] is the
	// initial calibration range for linear normalization.
	defaultDocSemFloor = -0.01
	defaultDocSemCeil  = 0.05
)

func docWSem() float64 {
	if config.Cfg != nil && config.Cfg.DocWSem != 0 {
		return config.Cfg.DocWSem
	}
	return defaultDocWSem
}

func docWGeo() float64 {
	if config.Cfg != nil && config.Cfg.DocWGeo != 0 {
		return config.Cfg.DocWGeo
	}
	return defaultDocWGeo
}

func docScoreFloor() float64 {
	if config.Cfg != nil && config.Cfg.DocScoreFloor != 0 {
		return config.Cfg.DocScoreFloor
	}
	return defaultDocScoreFloor
}

func docSemFloor() float64 {
	if config.Cfg != nil && config.Cfg.DocSemFloor != 0 {
		return config.Cfg.DocSemFloor
	}
	return defaultDocSemFloor
}

func docSemCeil() float64 {
	if config.Cfg != nil && config.Cfg.DocSemCeil != 0 {
		return config.Cfg.DocSemCeil
	}
	return defaultDocSemCeil
}

// docSemMargin returns the difference between the image vector's max cosine
// similarity against the "document group" prompts and against the "photo
// group" prompts. Positive = semantically more like a flat text carrier;
// negative = more like a natural photo. Returns 0 (neutral) for an empty group.
func docSemMargin(imgVec []float32, docVecs, photoVecs [][]float32) float64 {
	maxSim := func(vecs [][]float32) (float64, bool) {
		best, ok := -2.0, false
		for _, v := range vecs {
			if len(v) != len(imgVec) {
				continue
			}
			// cosDist returns cosine distance (caller convention: 1-cosDist =
			// similarity, see faces.go ClusterConfidence); check faces.go's
			// actual implementation before relying on this.
			sim := 1.0 - cosDist(imgVec, v)
			if sim > best {
				best, ok = sim, true
			}
		}
		return best, ok
	}
	d, okD := maxSim(docVecs)
	p, okP := maxSim(photoVecs)
	if !okD || !okP {
		return 0
	}
	return d - p
}

// docGeoScore computes layout regularity [0,1] from the retained lines'
// normalized four-corner coordinates (TL,TR,BR,BL order): the mean of three
// features — horizontality (top-edge angle), uniform height (line-height CV),
// and alignment (min of left-edge/center std).
// With fewer than 3 lines the statistics are unreliable, so it returns 0.5
// (neutral, doesn't vote).
// Note: computed in normalized coordinate space — a non-square image distorts
// the angle value, but a horizontal line (dy≈0) has angle≈0 under any aspect
// ratio, so the discriminating direction is unaffected.
func docGeoScore(boxes [][]float64) float64 {
	type line struct{ angle, height, left, center float64 }
	lines := make([]line, 0, len(boxes))
	for _, b := range boxes {
		if len(b) != 8 {
			continue
		}
		topDx, topDy := b[2]-b[0], b[3]-b[1]
		if topDx == 0 && topDy == 0 {
			continue
		}
		angle := math.Abs(math.Atan2(topDy, topDx)) * 180 / math.Pi
		if angle > 90 { // fold into [0,90]
			angle = 180 - angle
		}
		hL := math.Hypot(b[6]-b[0], b[7]-b[1]) // TL→BL
		hR := math.Hypot(b[4]-b[2], b[5]-b[3]) // TR→BR
		h := (hL + hR) / 2
		if h <= 0 {
			continue
		}
		lines = append(lines, line{
			angle:  angle,
			height: h,
			left:   math.Min(b[0], b[6]),
			center: (b[0] + b[2] + b[4] + b[6]) / 4,
		})
	}
	if len(lines) < 3 {
		return 0.5
	}

	mean := func(get func(line) float64) float64 {
		s := 0.0
		for _, l := range lines {
			s += get(l)
		}
		return s / float64(len(lines))
	}
	std := func(get func(line) float64, m float64) float64 {
		s := 0.0
		for _, l := range lines {
			d := get(l) - m
			s += d * d
		}
		return math.Sqrt(s / float64(len(lines)))
	}
	clamp01 := func(x float64) float64 { return math.Max(0, math.Min(1, x)) }

	// Horizontality: mean angle 0° → score 1; >=15° → score 0.
	horiz := clamp01(1 - mean(func(l line) float64 { return l.angle })/15)
	// Uniform height: line-height coefficient of variation 0 → score 1; >=0.75 → score 0.
	mh := mean(func(l line) float64 { return l.height })
	uniform := 0.0
	if mh > 0 {
		uniform = clamp01(1 - (std(func(l line) float64 { return l.height }, mh)/mh)/0.75)
	}
	// Alignment: min of left-edge/center std (works for both left-aligned documents
	// and centered receipts), 0 → score 1; >=0.15 → score 0.
	ml := mean(func(l line) float64 { return l.left })
	mc := mean(func(l line) float64 { return l.center })
	align := clamp01(1 - math.Min(
		std(func(l line) float64 { return l.left }, ml),
		std(func(l line) float64 { return l.center }, mc))/0.15)

	return (horiz + uniform + align) / 3
}

// docVerdict linearly normalizes the semantic margin, weights it against the
// geometry regularity score, and classifies as a document once past
// docScoreFloor. Computed for every asset with OCR lines — the density
// candidate gate is enforced by the query layer's hasOcrExpr; this function
// does not re-check density.
func docVerdict(semMargin, geo float64) bool {
	floor, ceil := docSemFloor(), docSemCeil()
	sem := 0.5
	if ceil > floor {
		sem = math.Max(0, math.Min(1, (semMargin-floor)/(ceil-floor)))
	}
	// 1e-9 tolerance: offsets floating-point division/weighting rounding error at
	// boundary values (e.g. the neutral tier landing exactly on floor); doesn't
	// change the classification semantics — a score genuinely below floor differs
	// by far more than this tolerance.
	return docWSem()*sem+docWGeo()*geo >= docScoreFloor()-1e-9
}

// hasOcrExpr is the single SQL fragment criterion for "OCR/document"
// classification (used at a SELECT column position, depends on the outer
// alias a). The density gate (coverage/line_count) is enforced unconditionally
// at the outer level; is_doc only vetoes, and can never bypass the gate (no
// rescue path):
//   - Density gate fails → always false, is_doc is never consulted.
//   - Density gate passes + is_doc IS NULL (not computed yet / backfilling /
//     ML has been offline for a long time) → falls back to the old dual
//     density thresholds (the density gate already guarantees non-empty text,
//     coverage and line_count met, which is equivalent to the old criterion
//     itself), matching per-asset behavior before this feature shipped — a
//     smooth degradation.
//   - Density gate passes + is_doc=0 → vetoed by the hybrid criterion, not
//     counted as an OCR document.
//   - Density gate passes + is_doc=1 → confirmed by the hybrid criterion,
//     counted as an OCR document.
//
// Shared by 11 query call sites; adjust thresholds in only this one place in
// docscore.go.
const hasOcrExpr = `EXISTS(SELECT 1 FROM asset_ocr ocr WHERE ocr.asset_id=a.id AND ocr.text<>'' AND COALESCE(ocr.coverage,1)>=0.05 AND COALESCE(ocr.line_count,0)>=8 AND COALESCE(ocr.is_doc,1)=1)`
