// The pet entity profile engine: upgrades the "library-wide search for pet
// elements" concept version (theme:pets) into an entity inference of "the
// user's own dog/cat" — the distinguishing signal is recurrence: the user's
// own pet reappears across months/years, while a dog encountered on the
// street shows up only once (see the design spec's product motivation).
//
// Two responsibilities:
//   - MinePetEntities: pure mining. For each lexicon word (possibly a phrase)
//     does a caption word-boundary match, tallies photo count/distinct
//     months, and only infers a ProfileEntity once it qualifies; doesn't
//     persist and doesn't touch the moments table. Mining also consumes the
//     user's existing pin/exclude feedback for that entity (see below).
//   - BuildPetEntityMoments: mine → profileStore.ReplaceEntities persists the
//     profile table → for each qualifying entity, assembles a MomentDraft
//     (union of word-hit members ∪ CLIP-searched members), fed into
//     MomentsService.recomputeRecipe's shared curation/persist pipeline — the
//     same assembly approach as the trip/theme engines.
//
// pin/exclude feedback consumption (Task 3): the entity's moment id is
// pre-derived by the existing derivation function (ProfileEntityID) →
// MomentStore.MomentEditsFor reads that entity's currently effective edits →
// assets hit by exclude are removed from the matched set, assets hit by pin
// (which must genuinely exist in the candidate pool, i.e. have a valid
// taken_at) are merged into the matched set, and both participate in the
// min_photos/min_months qualification check and first/last-seen stats
// together. This is necessary: the final correction of the moment_assets
// membership table is already replayed uniformly by the storage layer's
// applyMomentEdits on every SyncRecipeMoments round (family/theme/trip also
// rely on this generic replay and don't need their own mining-level
// consumption), but the determination of "does this entity still qualify as
// the user's own pet" and the first/last seen used for the profile/card
// subtitle are produced only in this mining-stage tally — the storage
// layer's generic replay can't reach that far. Once exclude has stripped it
// below the threshold, the entity should disappear from this output (which
// in turn makes BuildPetEntityMoments stop producing a draft for it, and
// SyncRecipeMoments's stale-delete cascades the stale moment away along with
// its moment_assets/moment_edits).
package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
)

// petEvidence is the JSON snapshot structure for the pet entity mining
// evidence, persisted to ProfileEntity.EvidenceJSON for troubleshooting and
// future upgrades to read; it doesn't participate in query filtering.
type petEvidence struct {
	PhotoCount int    `json:"photo_count"`
	Months     int    `json:"months"`
	First      string `json:"first"`
	Last       string `json:"last"`
}

// MinePetEntities is the pure-function entry point for pet profile mining:
// for each species/breed word in recipe.Lexicon (possibly a multi-word
// phrase, e.g. "maine coon"/"boxer dog"), does a caption word-boundary match
// (reusing matchCaptionKeywords's precise-filter approach — SQL instr for a
// coarse pass + a Go `\bkw\b` regex for the precise pass; \b naturally
// matches a multi-word phrase by its whole-phrase boundary, so it won't be
// misfired by a single word inside the phrase — e.g. bare "boxer" won't
// match "boxer dog"), intersects with the existing candidate pool
// (loadThemeCandidatePool, which excludes trash/offline/documents/live photo
// companion videos — the same criteria as the theme/trip engines), and
// tallies photo count and distinct year-month count; only once
// photo_count >= MinPhotos and months >= MinMonths does it infer a
// ProfileEntity (the recurrence criterion). Returns results sorted by Key
// lexically, guaranteeing deterministic, reproducible ordering across mining
// rounds. Returns empty when Lexicon is empty (no word list configured, not
// an error). store is used to read each entity's existing pin/exclude edit
// feedback (see the file header comment), which corrects the matched set
// before qualification and first/last-seen stats are computed.
func MinePetEntities(ctx context.Context, db *sql.DB, store *MomentStore, recipe MomentRecipe) ([]ProfileEntity, error) {
	params, err := ParseParams(recipe)
	if err != nil {
		return nil, err
	}
	if len(params.Lexicon) == 0 {
		return nil, nil
	}

	pool, err := loadThemeCandidatePool(ctx, db)
	if err != nil {
		return nil, err
	}

	var out []ProfileEntity
	for _, species := range params.Lexicon {
		hits, err := matchCaptionKeywords(ctx, db, []string{species})
		if err != nil {
			return nil, fmt.Errorf("moments: pet lexicon match %q: %w", species, err)
		}

		// Consume this entity's existing pin/exclude feedback: the moment id
		// uses the same derivation as e.ID used when BuildPetEntityMoments
		// persists, so it can be pre-computed and used to query edit records
		// directly. Assets hit by exclude are removed from the matched set,
		// assets hit by pin (treated as user-confirmed samples) are merged in,
		// and both participate in the qualification check and first/last-seen
		// stats below — otherwise the user's "this isn't my dog" feedback would
		// only show up in the membership table, and the next round's mining
		// stats would quietly pull it back in.
		momentID := ProfileEntityID("pet", species)
		pins, excludes, err := store.MomentEditsFor(momentID)
		if err != nil {
			return nil, fmt.Errorf("moments: pet entity edits %q: %w", momentID, err)
		}
		excludeSet := make(map[string]bool, len(excludes))
		for _, id := range excludes {
			excludeSet[id] = true
		}
		matched := make(map[string]bool, len(hits)+len(pins))
		for _, id := range hits {
			if !excludeSet[id] {
				matched[id] = true
			}
		}
		for _, id := range pins {
			if !excludeSet[id] { // pin and exclude coexisting shouldn't happen in theory (later write overwrites earlier), but we conservatively prioritize exclude here.
				matched[id] = true
			}
		}

		var photoCount int
		var first, last time.Time
		months := map[string]bool{}
		for id := range matched {
			takenAt, ok := pool[id]
			if !ok {
				continue // Not in the candidate pool (trash/offline/document/live photo companion video/no taken_at) — a pinned asset must genuinely exist in the candidate pool to count toward the stats.
			}
			photoCount++
			months[takenAt.Format("2006-01")] = true
			if first.IsZero() || takenAt.Before(first) {
				first = takenAt
			}
			if last.IsZero() || takenAt.After(last) {
				last = takenAt
			}
		}

		if photoCount < params.MinPhotos || len(months) < params.MinMonths {
			continue // Insufficient recurrence: a stranger's dog/a one-off encounter, not inferred as the user's own pet.
		}

		ev, _ := json.Marshal(petEvidence{
			PhotoCount: photoCount,
			Months:     len(months),
			First:      first.UTC().Format("2006-01-02"),
			Last:       last.UTC().Format("2006-01-02"),
		})

		out = append(out, ProfileEntity{
			ID:           momentID,
			Kind:         "pet",
			Key:          species,
			Label:        titleCasePhrase(species),
			EvidenceJSON: string(ev),
			PhotoCount:   photoCount,
			FirstSeen:    first,
			LastSeen:     last,
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// BuildPetEntityMoments is the pet entity moment engine's entry point: first
// MinePetEntities mines the library-wide qualifying pet entities →
// profileStore.ReplaceEntities("pet", ...) persists the profile table
// idempotently (must be called with an empty set even when there are no
// qualifying entities, to clear the previous round's profile — e.g. if the
// user's dog is gone/a lexicon change means it no longer matches, the profile
// shouldn't retain stale data) → for each qualifying entity, produces one
// MomentDraft: members = the intersection with the candidate pool of (that
// word's caption word-boundary hits ∪ CLIP("a photo of a "+species,
// filtered by ClipMinScore/ClipTopK)), Score takes the CLIP score, and an
// asset hit by the word boundary but not by CLIP gets the ClipMinScore floor
// score (the same two-path union approach as the theme engine, see
// BuildThemeMoments). TimeFrom/TimeTo come from the first/last computed
// during mining (the word-hit criterion, consistent with the entity's
// qualification criterion, not further extended by the CLIP union).
// Featured/cover are filled in afterward by the caller
// MomentsService.recomputeRecipe via PickFeaturedAndCover; this function only
// produces members and initial scores. Returns an empty slice (not an
// error) when there are no qualifying entities. store is passed through to
// MinePetEntities to consume pin/exclude feedback (TimeFrom/TimeTo come from
// the already-corrected first/last from mining, so the correction is carried
// through naturally; correction of the member list itself is backstopped by
// the storage layer's SyncRecipeMoments generic replay, not duplicated here —
// consistent with the trip/theme/family engines).
func BuildPetEntityMoments(ctx context.Context, db *sql.DB, searcher clipTextSearcher, profileStore *ProfileStore, store *MomentStore, recipe MomentRecipe) ([]MomentDraft, error) {
	entities, err := MinePetEntities(ctx, db, store, recipe)
	if err != nil {
		return nil, err
	}
	if err := profileStore.ReplaceEntities("pet", entities); err != nil {
		return nil, fmt.Errorf("moments: replace pet entities: %w", err)
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

	drafts := make([]MomentDraft, 0, len(entities))
	for _, e := range entities {
		species := e.Key

		score := map[string]float64{}

		wordHits, err := matchCaptionKeywords(ctx, db, []string{species})
		if err != nil {
			return nil, fmt.Errorf("moments: pet entity word hits %q: %w", species, err)
		}
		for _, id := range wordHits {
			score[id] = params.ClipMinScore // Floor score, may be overwritten by a higher CLIP score below.
		}

		clipHits, err := searcher.SearchAssetsByText(ctx, "a photo of a "+species, params.ClipTopK)
		if err != nil {
			return nil, fmt.Errorf("moments: pet entity clip search %q: %w", species, err)
		}
		for _, h := range clipHits {
			if h.Score < params.ClipMinScore {
				continue
			}
			if cur, ok := score[h.AssetID]; !ok || h.Score > cur {
				score[h.AssetID] = h.Score
			}
		}

		var assets []MomentAsset
		for id, s := range score {
			if _, ok := pool[id]; !ok {
				continue // Not in the candidate pool (trash/offline/document/live photo companion video).
			}
			assets = append(assets, MomentAsset{AssetID: id, Score: s})
		}
		sort.Slice(assets, func(i, j int) bool {
			if assets[i].Score != assets[j].Score {
				return assets[i].Score > assets[j].Score
			}
			return assets[i].AssetID < assets[j].AssetID
		})

		drafts = append(drafts, MomentDraft{
			Moment: Moment{
				ID:         e.ID,
				RecipeKey:  recipe.Key,
				Title:      "Your " + e.Label,
				Subtitle:   petEntitySubtitle(e.FirstSeen, e.LastSeen),
				TimeFrom:   e.FirstSeen,
				TimeTo:     e.LastSeen,
				AssetCount: len(assets),
			},
			Assets: assets,
		})
	}

	return drafts, nil
}

// petEntitySubtitle generates the pet entity moment card subtitle: a year
// span, writing just one year for the same year ("2020"), or an en dash with
// a space on each side across years ("2011 – 2026"). Different granularity
// from tripSubtitle's month-level — pet entities recur year after year, and
// the span between first and last photo is often several years, so year
// granularity fits better; month precision would be needlessly fussy.
func petEntitySubtitle(from, to time.Time) string {
	if from.Year() == to.Year() {
		return fmt.Sprintf("%d", from.Year())
	}
	return fmt.Sprintf("%d", from.Year()) + " – " + fmt.Sprintf("%d", to.Year())
}

// titleCasePhrase converts a (possibly multi-word) lowercase phrase to Title
// Case, capitalizing the first letter of each word: "beagle" -> "Beagle",
// "maine coon" -> "Maine Coon", "boxer dog" -> "Boxer Dog". Not using the
// standard library's strings.Title (deprecated, and overkill for the plain
// ASCII words this scenario needs) — a hand-rolled version is clearer.
func titleCasePhrase(s string) string {
	words := strings.Fields(s)
	for i, w := range words {
		if w == "" {
			continue
		}
		r := []rune(w)
		r[0] = unicode.ToUpper(r[0])
		words[i] = string(r)
	}
	return strings.Join(words, " ")
}
