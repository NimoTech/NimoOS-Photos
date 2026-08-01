// The trip engine: cuts GPS-tagged photos in the candidate pool into travel
// segments by taken time "globally" (not pre-grouped by city), with each
// segment landing as one trip-type MomentDraft.
//
// Why "globally segment first, name by city afterward" rather than grouping
// by city_id first and segmenting within each group like places.go's Visits
// does: a real trip often spans multiple cities (e.g. "Tokyo → Osaka"), and
// grouping by city first would split the same trip into several "separate
// visits". A trip moment should present the trip the way the user
// experienced it — as a single journey — so segments are first clustered
// globally by time, and then the segment's city frequency is used to pick
// one (or two) representative place names.
package service

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

// tripCandidate is the minimal information for one to-be-segmented asset in
// the candidate pool.
type tripCandidate struct {
	id      string
	takenAt time.Time
	city    string
	country string
}

// tripSegment is a segmentation result's index range within the ordered
// candidate slice (closed interval [start, end]).
type tripSegment struct {
	start, end int
}

// secondCityRatio is the minimum within-segment share a "second city" needs
// to be recognized for dual-city naming: only above this ratio is the city
// considered more than an incidental pass-through, worth pairing in the
// title.
const secondCityRatio = 0.3

// BuildTripMoments is the trip engine's pure-function entry point: takes
// GPS-tagged assets from the candidate pool sorted by taken time, segments
// them globally by the recipe's GapDays, and produces a MomentDraft for each
// segment whose size reaches MinAssets (not persisted; the caller merges
// idempotently via MomentStore.SyncRecipeMoments).
func BuildTripMoments(ctx context.Context, db *sql.DB, recipe MomentRecipe) ([]MomentDraft, error) {
	params, err := ParseParams(recipe)
	if err != nil {
		return nil, err
	}

	items, err := loadTripCandidates(ctx, db)
	if err != nil {
		return nil, err
	}

	times := make([]time.Time, len(items))
	for i, it := range items {
		times[i] = it.takenAt
	}
	segs := splitByGap(times, params.GapDays)

	drafts := make([]MomentDraft, 0, len(segs))
	for _, seg := range segs {
		n := seg.end - seg.start + 1
		if n < params.MinAssets {
			continue // A small cluster (e.g. a few shots taken while passing through somewhere over a weekend) isn't enough to become a trip moment.
		}
		segItems := items[seg.start : seg.end+1]
		from := segItems[0].takenAt
		to := segItems[len(segItems)-1].takenAt

		place := dominantPlace(segItems)
		title := "Trip"
		if place != "" {
			title = place + " Trip"
		}

		assets := make([]MomentAsset, len(segItems))
		for i, it := range segItems {
			// Score/Featured start as zero values; featured is filled in afterward
			// by Task 3's shared curation function.
			assets[i] = MomentAsset{AssetID: it.id}
		}

		drafts = append(drafts, MomentDraft{
			Moment: Moment{
				ID:         TripMomentID(recipe.Key, from),
				RecipeKey:  recipe.Key,
				Title:      title,
				Subtitle:   tripSubtitle(from, to),
				Place:      place,
				TimeFrom:   from,
				TimeTo:     to,
				AssetCount: n,
			},
			Assets: assets,
		})
	}
	return drafts, nil
}

// loadTripCandidates queries the candidate pool: status='indexed', not
// trashed (deleted_at IS NULL AND offline=0, the same criterion as existing
// queries elsewhere in this codebase — see embedder.go/faces.go etc.),
// excludes documents (the negation of hasOcrExpr, see docscore.go:202),
// excludes is_live_photo_video (a live photo's MOV side lands its own
// asset_geo row for the same instant as its still photo; not excluding it
// would double-count it within the same segment, inflating the count/
// skewing the dominant-city share; consistent with this codebase's other
// 15+ geo JOIN queries — places.go/persons.go/search.go/smartview.go etc. —
// rather than following non-geo pipelines like embedder/faces), and must
// have an asset_geo row (the JOIN naturally filters out GPS-less assets),
// returned sorted by taken time ascending. Ties on taken_at fall back to
// sorting by id, guaranteeing a deterministic, reproducible segmentation
// result across calls.
func loadTripCandidates(ctx context.Context, db *sql.DB) ([]tripCandidate, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT a.id, a.taken_at, COALESCE(g.city,''), COALESCE(g.country,'')
		FROM assets a
		JOIN asset_geo g ON g.asset_id = a.id
		WHERE a.status='indexed' AND a.deleted_at IS NULL AND a.offline=0
		  AND a.is_live_photo_video=0
		  AND a.taken_at IS NOT NULL
		  AND NOT (`+hasOcrExpr+`)
		ORDER BY a.taken_at ASC, a.id ASC`)
	if err != nil {
		return nil, fmt.Errorf("moments: trip candidate query: %w", err)
	}
	defer rows.Close()

	var out []tripCandidate
	for rows.Next() {
		var it tripCandidate
		var ts sql.NullString
		if err := rows.Scan(&it.id, &ts, &it.city, &it.country); err != nil {
			return nil, fmt.Errorf("moments: scan trip candidate: %w", err)
		}
		t := parseSQLiteTime(ts)
		if t == nil {
			continue // taken_at is already constrained non-null in the WHERE clause; this is just a belt-and-suspenders check.
		}
		it.takenAt = *t
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("moments: iterate trip candidates: %w", err)
	}
	return out, nil
}

// splitByGap cuts an already time-ascending-sorted sequence into segments
// wherever adjacent points are > gapDays apart; the algorithm matches
// places.go:194-207's splitTrips, the difference being that the input here
// is a globally merged cross-city sequence, not a sequence within a single
// city.
func splitByGap(times []time.Time, gapDays int) []tripSegment {
	if len(times) == 0 {
		return nil
	}
	gap := time.Duration(gapDays) * 24 * time.Hour
	segs := []tripSegment{{0, 0}}
	for i := 1; i < len(times); i++ {
		if times[i].Sub(times[i-1]) > gap {
			segs = append(segs, tripSegment{i, i})
		} else {
			segs[len(segs)-1].end = i
		}
	}
	return segs
}

// dominantPlace returns the segment's dominant-city naming: the
// highest-frequency city is the dominant one; if there's a second-most-
// frequent city whose appearance count exceeds secondCityRatio of the
// segment's total assets, returns "CityA & CityB" (A being the dominant
// one). Frequency ties are broken stably by city name lexical order, to
// avoid getting a different naming order across multiple recomputes of the
// same data. An empty city (reverse geocoding produced no place name)
// doesn't count; if the whole segment has empty cities, returns an empty
// string (the caller falls back to the "Trip" title).
func dominantPlace(items []tripCandidate) string {
	counts := map[string]int{}
	var order []string
	for _, it := range items {
		city := strings.TrimSpace(it.city)
		if city == "" {
			continue
		}
		if _, ok := counts[city]; !ok {
			order = append(order, city)
		}
		counts[city]++
	}
	if len(order) == 0 {
		return ""
	}
	sort.SliceStable(order, func(i, j int) bool {
		if counts[order[i]] != counts[order[j]] {
			return counts[order[i]] > counts[order[j]]
		}
		return order[i] < order[j]
	})
	top := order[0]
	if len(order) < 2 {
		return top
	}
	second := order[1]
	if float64(counts[second])/float64(len(items)) > secondCityRatio {
		return top + " & " + second
	}
	return top
}

// tripSubtitle generates the moment card subtitle: same month and year "May
// 2011"; cross-month same year "May – Jun 2011" (en dash, a space on each
// side); cross-year carries the year on both sides, e.g. "Dec 2011 – Jan
// 2012".
func tripSubtitle(from, to time.Time) string {
	if from.Year() == to.Year() && from.Month() == to.Month() {
		return from.Format("Jan 2006")
	}
	if from.Year() == to.Year() {
		return from.Format("Jan") + " – " + to.Format("Jan 2006")
	}
	return from.Format("Jan 2006") + " – " + to.Format("Jan 2006")
}
