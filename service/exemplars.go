package service

import (
	"database/sql"
	"math"
	"sort"
)

// exemplarCandidate is one face_detections row (plus its quality signals)
// considered for exemplar selection under a single person.
type exemplarCandidate struct {
	FaceID                string
	Vec                   []float32
	Score                 sql.NullFloat64 // detector score
	Frontality, Sharpness sql.NullFloat64
	Confirmed             bool
}

// SelectExemplars picks up to cap exemplar faces for one person:
//  1. hard quality gate (score/frontality/sharpness thresholds; NULL fails the
//     gate — pre-gen4 rows without signals never become exemplars);
//  2. confirmed faces that pass the gate are seeded first (user-verified
//     identity is the strongest signal);
//  3. remaining slots filled by farthest-point sampling over cosine distance,
//     seeded from the confirmed set (or the highest-score face when none),
//     so the set spans appearance variation (age/glasses/lighting) instead of
//     clustering around the mode.
//
// Deterministic preference order, used both to rank a confirmed-overflow
// (len(confirmed) >= cap) and to break ties during sampling: highest Score
// first, then FaceID ascending. Same input therefore always yields the same
// output slice, in the same order.
//
// Pure function: no DB access, no config reads — the caller (Task 4) passes
// exemplarQualityGate()'s thresholds and exemplarCap().
func SelectExemplars(cands []exemplarCandidate, cap int, minScore, minFront, minSharp float64) []string {
	if cap <= 0 {
		return nil
	}

	// 1) Hard quality gate. NULL (Valid=false) always fails, regardless of
	// threshold value.
	gated := make([]exemplarCandidate, 0, len(cands))
	for _, c := range cands {
		if !c.Score.Valid || c.Score.Float64 < minScore {
			continue
		}
		if !c.Frontality.Valid || c.Frontality.Float64 < minFront {
			continue
		}
		if !c.Sharpness.Valid || c.Sharpness.Float64 < minSharp {
			continue
		}
		gated = append(gated, c)
	}
	if len(gated) == 0 {
		return nil
	}

	// Deterministic preference order: highest score first, then FaceID.
	sort.Slice(gated, func(i, j int) bool {
		if gated[i].Score.Float64 != gated[j].Score.Float64 {
			return gated[i].Score.Float64 > gated[j].Score.Float64
		}
		return gated[i].FaceID < gated[j].FaceID
	})

	// 2) Confirmed-first seeding.
	var confirmed, rest []exemplarCandidate
	for _, c := range gated {
		if c.Confirmed {
			confirmed = append(confirmed, c)
		} else {
			rest = append(rest, c)
		}
	}

	if len(confirmed) >= cap {
		out := make([]string, cap)
		for i := 0; i < cap; i++ {
			out[i] = confirmed[i].FaceID
		}
		return out
	}

	selected := make([]exemplarCandidate, 0, cap)
	selected = append(selected, confirmed...)

	pool := rest
	// 3) Farthest-point sampling, seeded from the confirmed set, or (when
	// there's none) the highest-score gated face — gated/rest is already
	// sorted score-desc/FaceID-asc, so pool[0] is that face.
	if len(selected) == 0 {
		selected = append(selected, pool[0])
		pool = pool[1:]
	}

	for len(selected) < cap && len(pool) > 0 {
		bestIdx := -1
		bestDist := -1.0
		for i, cand := range pool {
			minD := math.Inf(1)
			for _, s := range selected {
				if d := cosDist(cand.Vec, s.Vec); d < minD {
					minD = d
				}
			}
			if bestIdx == -1 || minD > bestDist || (minD == bestDist && cand.FaceID < pool[bestIdx].FaceID) {
				bestIdx, bestDist = i, minD
			}
		}
		selected = append(selected, pool[bestIdx])
		pool = append(pool[:bestIdx], pool[bestIdx+1:]...)
	}

	out := make([]string, len(selected))
	for i, s := range selected {
		out[i] = s.FaceID
	}
	return out
}
