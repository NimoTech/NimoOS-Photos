// The family profile engine: mines "the user's own family" out of face
// clustering results — a frequently-appearing person (the persons table) is
// inferred as a profile entity, then produces two kinds of moments: a
// "together" collection of photos where frequent people co-occur, and a
// "Through the Years" for each named person. The distinguishing signal
// follows the profile layer's usual logic — recurrence (the user's own family
// keeps reappearing; unlike pets there's no lexicon to match against, family
// is judged by face-clustering frequency); see the design spec §1/§2 for
// details.
//
// Three responsibilities:
//   - MinePersonEntities: pure mining. Tallies each person's appearance
//     frequency (distinct assets, excluding excluded faces and hidden
//     persons, join style mirrors persons.go's ListPersons); those that
//     qualify (>= MinPersonPhotos) take the top TopPersons; doesn't persist,
//     doesn't touch the moments table.
//   - BuildFamilyMoments: mine → ReplaceEntities persists the profile table →
//     a "together" draft (≥ MinTogetherPersons of the top entities co-occur)
//     and named-person drafts (one per entity with a non-empty label), fed
//     into MomentsService.recomputeRecipe's shared curation/persist
//     pipeline — the same assembly approach as the pet/trip/theme engines.
package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// personEvidence is the JSON snapshot structure for the person entity mining
// evidence, persisted to ProfileEntity.EvidenceJSON for troubleshooting and
// future upgrades to read; it doesn't participate in query filtering.
type personEvidence struct {
	PhotoCount int    `json:"photo_count"`
	First      string `json:"first"`
	Last       string `json:"last"`
}

// personFreq is one row of intermediate result from person-frequency mining
// (before being persisted as a ProfileEntity).
type personFreq struct {
	personID string
	name     string
	count    int
	first    time.Time
	last     time.Time
}

// MinePersonEntities is the pure-function entry point for family profile
// mining: tallies each person's appearance frequency (distinct assets, join
// style mirrors persons.go's ListPersons — face_person joins face_detections
// with excluded=0, joins persons with hidden=0); the assets-side criterion is
// aligned character-for-character with loadThemeCandidatePool's
// (moments_theme.go) candidate pool criteria — status='indexed', not trashed
// (deleted_at IS NULL AND offline=0), excludes documents (the negation of
// hasOcrExpr), excludes is_live_photo_video, requires non-null taken_at —
// otherwise the frequency-qualification count could inflate above the
// candidate pool actually available on the member side (both
// buildTogetherDraft/buildNamedPersonDraft on the member side intersect with
// the candidate pool, and a looser mining criterion could produce a
// discrepancy of "it qualified, but the member count never reaches
// MinAssets"). photo_count >= MinPersonPhotos is required to qualify;
// qualifying entities are sorted by frequency descending (ties broken
// stably by person_id lexical order, guaranteeing deterministic,
// reproducible mining results across rounds), taking the top TopPersons.
// Unnamed persons (persons.name empty) participate in mining and can be
// selected as entities too — the "named" bar only takes effect when
// BuildFamilyMoments produces the individual draft; the profile table itself
// faithfully records the mining result, not excluding an entity for being
// unnamed (decoupled from the timing of the user later adding a name on the
// People page).
func MinePersonEntities(ctx context.Context, db *sql.DB, recipe MomentRecipe) ([]ProfileEntity, error) {
	params, err := ParseParams(recipe)
	if err != nil {
		return nil, err
	}

	rows, err := db.QueryContext(ctx, `
		SELECT fp.person_id, COALESCE(p.name,''), COUNT(DISTINCT fd.asset_id) AS cnt,
		       MIN(a.taken_at), MAX(a.taken_at)
		FROM face_person fp
		JOIN face_detections fd ON fd.id=fp.face_id AND fd.excluded=0
		JOIN persons p ON p.id=fp.person_id AND p.hidden=0
		JOIN assets a ON a.id=fd.asset_id AND a.status='indexed' AND a.deleted_at IS NULL AND a.offline=0 AND a.is_live_photo_video=0
		WHERE a.taken_at IS NOT NULL AND NOT (`+hasOcrExpr+`)
		GROUP BY fp.person_id
		HAVING cnt >= ?
		ORDER BY cnt DESC, fp.person_id ASC`, params.MinPersonPhotos)
	if err != nil {
		return nil, fmt.Errorf("moments: person frequency query: %w", err)
	}
	defer rows.Close()

	var freqs []personFreq
	for rows.Next() {
		var f personFreq
		var first, last sql.NullString
		if err := rows.Scan(&f.personID, &f.name, &f.count, &first, &last); err != nil {
			return nil, fmt.Errorf("moments: scan person frequency: %w", err)
		}
		if t := parseSQLiteTime(first); t != nil {
			f.first = *t
		}
		if t := parseSQLiteTime(last); t != nil {
			f.last = *t
		}
		freqs = append(freqs, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("moments: iterate person frequency: %w", err)
	}

	if len(freqs) > params.TopPersons {
		freqs = freqs[:params.TopPersons]
	}

	out := make([]ProfileEntity, 0, len(freqs))
	for _, f := range freqs {
		ev, _ := json.Marshal(personEvidence{
			PhotoCount: f.count,
			First:      f.first.UTC().Format("2006-01-02"),
			Last:       f.last.UTC().Format("2006-01-02"),
		})
		out = append(out, ProfileEntity{
			ID:           ProfileEntityID("person", f.personID),
			Kind:         "person",
			Key:          f.personID,
			Label:        f.name,
			EvidenceJSON: string(ev),
			PhotoCount:   f.count,
			FirstSeen:    f.first,
			LastSeen:     f.last,
		})
	}
	return out, nil
}

// BuildFamilyMoments is the family engine's entry point: first
// MinePersonEntities mines the library-wide qualifying frequent persons →
// profileStore.ReplaceEntities("person", ...) persists the profile table
// idempotently (must be called with an empty set even when there are no
// qualifying entities, to clear the previous round's profile) → produces two
// kinds of draft:
//   - A "together" collection (photos where ≥ MinTogetherPersons of the top
//     entities co-occur, at most one, with the id fixed as
//     ProfileEntityID("family","together"));
//   - Named-person moments (one per entity with a non-empty label, id=the
//     entity's id, members=all of that person's photos).
//
// Both kinds of draft's members must intersect with the candidate pool
// (loadThemeCandidatePool, which excludes trash/offline/documents/live photo
// companion videos — the same criteria as the theme/trip engines), and are
// only produced once >= MinAssets; member Score is uniformly set to 0 —
// unlike the pet/theme engines which have a CLIP score available, curation is
// left to PickFeaturedAndCover to pick by aesthetic score afterward (the same
// approach as the trip engine, see BuildTripMoments). Returns an empty slice
// (not an error) when there are no qualifying entities.
func BuildFamilyMoments(ctx context.Context, db *sql.DB, profileStore *ProfileStore, recipe MomentRecipe) ([]MomentDraft, error) {
	entities, err := MinePersonEntities(ctx, db, recipe)
	if err != nil {
		return nil, err
	}
	if err := profileStore.ReplaceEntities("person", entities); err != nil {
		return nil, fmt.Errorf("moments: replace person entities: %w", err)
	}
	if len(entities) == 0 {
		return nil, nil
	}

	params, err := ParseParams(recipe)
	if err != nil {
		return nil, err
	}
	pool, err := loadThemeCandidatePool(ctx, db)
	if err != nil {
		return nil, err
	}

	var drafts []MomentDraft

	together, err := buildTogetherDraft(ctx, db, entities, pool, recipe, params)
	if err != nil {
		return nil, err
	}
	if together != nil {
		drafts = append(drafts, *together)
	}

	for _, e := range entities {
		if e.Label == "" {
			// An unnamed person doesn't produce an individual draft: avoids
			// showing a meaningless placeholder name like "Person 1", naturally
			// nudging the user toward naming them on the People page (see design
			// spec §2).
			continue
		}
		draft, err := buildNamedPersonDraft(ctx, db, e, pool, recipe, params)
		if err != nil {
			return nil, err
		}
		if draft != nil {
			drafts = append(drafts, *draft)
		}
	}

	return drafts, nil
}

// buildTogetherDraft queries photos where ≥ MinTogetherPersons of the top
// entities co-occur (a face_person/face_detections join, GROUP BY asset
// HAVING COUNT(DISTINCT person_id) >= N, the excluded-exclusion convention
// same as MinePersonEntities), intersects with the candidate pool, and only
// returns a draft once qualifying (>= MinAssets); otherwise returns nil (not
// an error).
func buildTogetherDraft(ctx context.Context, db *sql.DB, entities []ProfileEntity, pool map[string]time.Time, recipe MomentRecipe, params RecipeParams) (*MomentDraft, error) {
	personIDs := make([]string, len(entities))
	for i, e := range entities {
		personIDs[i] = e.Key
	}
	if len(personIDs) == 0 {
		// Defensive: the caller BuildFamilyMoments already returns early when
		// entities is empty, so this is theoretically unreachable; but
		// strings.Repeat(..., -1) would panic, and adding this guard is safer
		// than relying on "the caller always checks first".
		return nil, nil
	}
	placeholders := strings.Repeat("?,", len(personIDs)-1) + "?"
	args := make([]interface{}, 0, len(personIDs)+1)
	for _, id := range personIDs {
		args = append(args, id)
	}
	args = append(args, params.MinTogetherPersons)

	q := fmt.Sprintf(`
		SELECT fd.asset_id
		FROM face_person fp
		JOIN face_detections fd ON fd.id=fp.face_id AND fd.excluded=0
		WHERE fp.person_id IN (%s)
		GROUP BY fd.asset_id
		HAVING COUNT(DISTINCT fp.person_id) >= ?`, placeholders)
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("moments: family together query: %w", err)
	}
	defer rows.Close()

	var assets []MomentAsset
	var from, to time.Time
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("moments: scan together asset: %w", err)
		}
		takenAt, ok := pool[id]
		if !ok {
			continue // Not in the candidate pool (trash/offline/document/live photo companion video/no taken_at).
		}
		assets = append(assets, MomentAsset{AssetID: id})
		if from.IsZero() || takenAt.Before(from) {
			from = takenAt
		}
		if to.IsZero() || takenAt.After(to) {
			to = takenAt
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("moments: iterate together asset: %w", err)
	}
	if len(assets) < params.MinAssets {
		return nil, nil
	}
	sort.Slice(assets, func(i, j int) bool { return assets[i].AssetID < assets[j].AssetID })

	return &MomentDraft{
		Moment: Moment{
			ID:         ProfileEntityID("family", "together"),
			RecipeKey:  recipe.Key,
			Title:      "Family Moments",
			Subtitle:   petEntitySubtitle(from, to),
			TimeFrom:   from,
			TimeTo:     to,
			AssetCount: len(assets),
		},
		Assets: assets,
	}, nil
}

// buildNamedPersonDraft queries all photos of a named person (non-empty
// label, excluding excluded faces), intersects with the candidate pool, and
// only returns a draft once qualifying (>= MinAssets); otherwise returns nil
// (not an error).
func buildNamedPersonDraft(ctx context.Context, db *sql.DB, e ProfileEntity, pool map[string]time.Time, recipe MomentRecipe, params RecipeParams) (*MomentDraft, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT DISTINCT fd.asset_id
		FROM face_person fp
		JOIN face_detections fd ON fd.id=fp.face_id AND fd.excluded=0
		WHERE fp.person_id=?`, e.Key)
	if err != nil {
		return nil, fmt.Errorf("moments: named person asset query %q: %w", e.Key, err)
	}
	defer rows.Close()

	var assets []MomentAsset
	var from, to time.Time
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("moments: scan named person asset: %w", err)
		}
		takenAt, ok := pool[id]
		if !ok {
			continue // Not in the candidate pool (trash/offline/document/live photo companion video/no taken_at).
		}
		assets = append(assets, MomentAsset{AssetID: id})
		if from.IsZero() || takenAt.Before(from) {
			from = takenAt
		}
		if to.IsZero() || takenAt.After(to) {
			to = takenAt
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("moments: iterate named person asset: %w", err)
	}
	if len(assets) < params.MinAssets {
		return nil, nil
	}
	sort.Slice(assets, func(i, j int) bool { return assets[i].AssetID < assets[j].AssetID })

	return &MomentDraft{
		Moment: Moment{
			ID:         e.ID,
			RecipeKey:  recipe.Key,
			Title:      e.Label + " Through the Years",
			Subtitle:   petEntitySubtitle(from, to),
			TimeFrom:   from,
			TimeTo:     to,
			AssetCount: len(assets),
		},
		Assets: assets,
	}, nil
}
