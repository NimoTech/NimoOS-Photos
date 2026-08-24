package service

import "sort"

// exemplarEntry is one exemplar vector inside the flattened brute-force KNN
// index, tagged with the owning person and its ordinal position within
// that person's exemplar list as supplied to BuildExemplarIndex — the
// ordinal is the final leg of the deterministic nearest-k tie-break.
type exemplarEntry struct {
	personID string
	vec      []float32
	ordinal  int
}

// exemplarIndex is a flat, brute-force KNN index over every anchored
// person's exemplar vectors, built once per clustering pass. totalExemplars
// is each person's STATIC exemplar count as supplied to BuildExemplarIndex —
// independent of any later Match call's negation filtering — used to derive
// the per-person effective vote floor (see Match's doc comment).
type exemplarIndex struct {
	entries        []exemplarEntry
	totalExemplars map[string]int
}

// BuildExemplarIndex flattens all anchored persons' exemplar vectors into
// one in-memory slice for brute-force KNN (exemplar total is bounded by
// cap*persons, well under 10k on any realistic library).
//
// Determinism: Go map iteration order is randomized, so persons are
// visited in sorted personID order before flattening. Combined with the
// per-person exemplar ordinal, this makes the entries slice — and every
// nearest-k tie-break that falls back to (personID, ordinal) — identical
// across runs for the same input map, independent of map iteration order.
func BuildExemplarIndex(persons map[string][][]float32) *exemplarIndex {
	ids := make([]string, 0, len(persons))
	for id := range persons {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	ix := &exemplarIndex{totalExemplars: make(map[string]int, len(ids))}
	for _, id := range ids {
		for i, vec := range persons[id] {
			ix.entries = append(ix.entries, exemplarEntry{personID: id, vec: vec, ordinal: i})
		}
		ix.totalExemplars[id] = len(persons[id])
	}
	return ix
}

// scoredCandidate is one exemplar entry scored against the query vector
// for a single Match call.
type scoredCandidate struct {
	entry exemplarEntry
	dist  float64
}

// Match returns the winning person for a face vector, or "" when no person
// clears the suggest bar. Procedure: take the k nearest exemplars overall
// (after dropping any exemplar whose owner is negated for this face); the
// person holding the plurality of those k wins IF that plurality is strict
// (a tie for the top vote count, e.g. 2/2/1, never picks a winner, so it
// yields "none" regardless of how close the tied persons' exemplars are)
// AND its vote count clears an effective floor. dist is the median distance
// of the winner's exemplars among the k (even count -> mean of the middle
// two). Faces negated for a person are excluded from voting for that person.
//
// Effective vote floor: minVotes is a floor on CONSENSUS among a person's
// available exemplars, not a flat minimum every person must reach regardless
// of how many exemplars they even have. A person with fewer total exemplars
// than minVotes (e.g. 1-2 under the default 3) could otherwise never win —
// its total vote count is capped below the floor no matter how close the
// match — starving freshly-anchored small persons of both auto-join and
// suggestions, a regression against the old centroid snap. So the floor
// actually applied to the winner is min(minVotes, that person's STATIC total
// exemplar count in this index — independent of this call's negation
// filtering), floored again at 1 for clarity (a 0-exemplar person can never
// be a winner in the first place, since it contributes no pool entries; the
// floor-at-1 guard just documents that intent rather than relying on it).
// A 1-exemplar person therefore degenerates to nearest-exemplar matching,
// gated purely by the distance thresholds below — exactly one vote is
// already full consensus for a person who only has one template.
//
//	dist <= autoDist              -> decision "auto"
//	autoDist < dist < suggestDist -> decision "suggest"
//	otherwise                     -> decision "none"
//
// personID is non-empty only when decision is "auto" or "suggest": per the
// doc line above, "no person clears the suggest bar" always means
// personID == "" and dist == 0, whether the cause is an empty/negated-out
// pool, a vote-count tie, a sub-floor winning vote count, or a winner
// whose median distance simply misses the suggest bar.
//
// Precondition: k may be any value, including negative (e.g. a misconfigured
// AssignKNNK) -- a negative k is clamped to 0 up front, which then always
// yields "none" (an empty top-k window can never reach minVotes >= 1).
func (ix *exemplarIndex) Match(vec []float32, faceID string,
	negatives map[[2]string]bool, k, minVotes int,
	autoDist, suggestDist float64) (personID string, dist float64, decision string) {

	if k < 0 {
		k = 0
	}

	// Drop exemplars owned by a person explicitly negated for this face —
	// removed from the pool entirely (not merely un-voted), so a further
	// exemplar belonging to a different person can slide into the k
	// window in its place; this is what lets a runner-up win after a
	// negation instead of just losing the negated person's votes.
	pool := make([]scoredCandidate, 0, len(ix.entries))
	for _, e := range ix.entries {
		if negatives[[2]string{e.personID, faceID}] {
			continue
		}
		pool = append(pool, scoredCandidate{entry: e, dist: cosDist(vec, e.vec)})
	}
	if len(pool) == 0 {
		return "", 0, "none"
	}

	// Deterministic nearest-k selection: distance ascending, then
	// personID, then the exemplar's ordinal within its person. Never
	// depends on map iteration order — the pool was built by walking
	// ix.entries, itself built in sorted personID order.
	sort.Slice(pool, func(i, j int) bool {
		if pool[i].dist != pool[j].dist {
			return pool[i].dist < pool[j].dist
		}
		if pool[i].entry.personID != pool[j].entry.personID {
			return pool[i].entry.personID < pool[j].entry.personID
		}
		return pool[i].entry.ordinal < pool[j].entry.ordinal
	})

	// k may exceed what's actually available (a small index, or a
	// negation that thinned the pool) — vote from whatever made it into
	// the window, not against the full requested k.
	if k > len(pool) {
		k = len(pool)
	}
	top := pool[:k]

	votes := make(map[string]int, len(top))
	for _, c := range top {
		votes[c.entry.personID]++
	}

	// Max-count scan for the plurality winner. This is order-independent
	// under Go's randomized map iteration: `tie` becomes true exactly when
	// two persons' vote counts are equal to the running max, regardless of
	// which one is visited first (a later, larger count always resets
	// tie=false via the `>` branch), and the returned personID is only
	// ever used when tie stays false — so no map-order leak reaches the
	// decision.
	winner := ""
	winnerVotes := 0
	tie := false
	for id, v := range votes {
		switch {
		case v > winnerVotes:
			winner, winnerVotes, tie = id, v, false
		case v == winnerVotes:
			tie = true
		}
	}
	if tie {
		return "", 0, "none"
	}
	effectiveFloor := max(1, min(minVotes, ix.totalExemplars[winner]))
	if winnerVotes < effectiveFloor {
		return "", 0, "none"
	}

	// Median distance of the winner's exemplars among the k.
	winnerDists := make([]float64, 0, winnerVotes)
	for _, c := range top {
		if c.entry.personID == winner {
			winnerDists = append(winnerDists, c.dist)
		}
	}
	sort.Float64s(winnerDists)
	n := len(winnerDists)
	var med float64
	if n%2 == 1 {
		med = winnerDists[n/2]
	} else {
		med = (winnerDists[n/2-1] + winnerDists[n/2]) / 2.0
	}

	switch {
	case med <= autoDist:
		return winner, med, "auto"
	case med < suggestDist:
		return winner, med, "suggest"
	default:
		return "", 0, "none"
	}
}
