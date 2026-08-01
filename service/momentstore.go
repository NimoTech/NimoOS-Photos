// Smart Moments data layer: repo for three tables (moment_recipes/moments/moment_assets).
//
// moment_recipes is the hot-update carrier for "moment kind = data" — the engine
// dispatches algorithms by kind, and pure-data params like clip_prompts/
// caption_keywords/thresholds take effect via PUT recipes, no code change needed
// (except for kinds that genuinely need a new algorithm).
//
// moments/moment_assets are live entities with stable derived ids
// (TripMomentID/ThemeMomentID): each recalculation round upserts by id + does a
// diff-style member upsert (existing members keep added_at, absent ones are
// deleted but pinned members are exempt), so the moments the user sees don't
// flicker across recalculations; a title the LLM has already named
// (named_by_llm=1) is kept as-is on recalculation, and only a title still in
// the template-seeded stage (named_by_llm=0) gets overwritten by the next
// round's template result.
package service

import (
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// MomentRecipe corresponds to one row of the moment_recipes table. ParamsJSON
// is the raw JSON string; its concrete fields are parsed into RecipeParams
// (with defaults filled in) via ParseParams.
type MomentRecipe struct {
	Key        string
	Kind       string // "trip" | "theme"
	Title      string
	ParamsJSON string
	Enabled    bool
	UpdatedAt  int64 // Unix ms
}

// RecipeParams is the parsed result of the moment_recipes.params column. The
// json tags use snake_case, matching the JSON format pushed via
// PUT /v1/photos/moments/recipes.
type RecipeParams struct {
	ClipPrompts     []string `json:"clip_prompts"`
	CaptionKeywords []string `json:"caption_keywords"`
	MinAssets       int      `json:"min_assets"`
	MaxFeatured     int      `json:"max_featured"`
	GapDays         int      `json:"gap_days"`
	TopK            int      `json:"top_k"`
	MinScore        float64  `json:"min_score"`

	// ── Moments M2 profile-layer fields (profile:pets / profile:family only) ──
	// Lexicon has no default fallback: unspecified means an empty word list —
	// falling back to a guessed word list out of thin air is riskier than an
	// empty one. The remaining fields (including MinAssets, reused by family's
	// "group-photo threshold") still fall back to defaults under the
	// "zero value means unspecified" rule; see the ParseParams comment.
	Lexicon            []string `json:"lexicon"`              // profile:pets: English species/breed word list
	MinPhotos          int      `json:"min_photos"`           // profile:pets: min photo count to qualify
	MinMonths          int      `json:"min_months"`           // profile:pets: min month span to qualify
	ClipMinScore       float64  `json:"clip_min_score"`       // profile:pets: CLIP retrieval min score
	ClipTopK           int      `json:"clip_top_k"`           // profile:pets: CLIP retrieval top-K
	TopPersons         int      `json:"top_persons"`          // profile:family: top-K most frequent named persons for named-person moments
	MinPersonPhotos    int      `json:"min_person_photos"`    // profile:family: min photo count for a person to qualify
	MinTogetherPersons int      `json:"min_together_persons"` // profile:family: min number of people appearing together for a group-photo moment
}

// Defaults: fallback when a recipe doesn't explicitly specify a field (or the
// field is zero-valued); see the design brief for details.
const (
	defaultMinAssets   = 10
	defaultMaxFeatured = 12
	defaultGapDays     = 14
	defaultTopK        = 200
	defaultMinScore    = 0.2

	// profile:pets defaults (see section 1 of the design spec).
	defaultMinPhotos    = 8
	defaultMinMonths    = 2
	defaultClipMinScore = 0.45
	defaultClipTopK     = 100

	// profile:family defaults (see section 1 of the design spec).
	defaultTopPersons         = 5
	defaultMinPersonPhotos    = 30
	defaultMinTogetherPersons = 2
)

// ParseParams parses recipe.ParamsJSON into RecipeParams, filling in defaults
// for missing (zero-valued) fields. Note: this uses "zero value means
// unspecified" to decide whether to fall back to a default — there's no
// legitimate recipe-param scenario for "deliberately set to 0"
// (min_assets=0/gap_days=0 etc. are meaningless), so falling back on zero is
// a safe simplification; there's no need to distinguish "key absent" from
// "explicitly written as 0".
func ParseParams(r MomentRecipe) (RecipeParams, error) {
	var p RecipeParams
	if s := strings.TrimSpace(r.ParamsJSON); s != "" {
		if err := json.Unmarshal([]byte(s), &p); err != nil {
			return RecipeParams{}, fmt.Errorf("moments: parse recipe params %q: %w", r.Key, err)
		}
	}
	if p.MinAssets == 0 {
		p.MinAssets = defaultMinAssets
	}
	if p.MaxFeatured == 0 {
		p.MaxFeatured = defaultMaxFeatured
	}
	if p.GapDays == 0 {
		p.GapDays = defaultGapDays
	}
	if p.TopK == 0 {
		p.TopK = defaultTopK
	}
	if p.MinScore == 0 {
		p.MinScore = defaultMinScore
	}
	if p.MinPhotos == 0 {
		p.MinPhotos = defaultMinPhotos
	}
	if p.MinMonths == 0 {
		p.MinMonths = defaultMinMonths
	}
	if p.ClipMinScore == 0 {
		p.ClipMinScore = defaultClipMinScore
	}
	if p.ClipTopK == 0 {
		p.ClipTopK = defaultClipTopK
	}
	if p.TopPersons == 0 {
		p.TopPersons = defaultTopPersons
	}
	if p.MinPersonPhotos == 0 {
		p.MinPersonPhotos = defaultMinPersonPhotos
	}
	if p.MinTogetherPersons == 0 {
		p.MinTogetherPersons = defaultMinTogetherPersons
	}
	// Lexicon deliberately does not fall back to a default: unspecified means
	// an empty word list (see the field comment).
	return p, nil
}

// Moment corresponds to one row of the moments table (a live entity).
// TimeFrom/TimeTo being a zero-value time.Time means the column is NULL in
// the DB (theme-kind moments have no fixed time window).
type Moment struct {
	ID           string
	RecipeKey    string
	Title        string
	Subtitle     string
	CoverAssetID string
	Place        string
	TimeFrom     time.Time
	TimeTo       time.Time
	AssetCount   int
	NamedByLLM   bool
	CreatedAt    int64 // Unix ms
	UpdatedAt    int64 // Unix ms
	// SortOrder corresponds to the sort_order column: nil means "not manually
	// ordered" (preserving NULL semantics faithfully, distinct from "manually
	// ordered to 0"); when non-nil it's the (i+1)*10 sequence number written
	// by ReorderMoments.
	SortOrder *int
	// Hidden corresponds to the hidden column: a tombstone for the user
	// "hiding this moment". ListMoments filters on hidden=0, and
	// SyncRecipeMoments recalculation never clears it (it's not in the upsert
	// column list, same treatment as named_by_llm/sort_order).
	Hidden bool
}

// MomentAsset corresponds to one row of the moment_assets table.
type MomentAsset struct {
	AssetID  string
	Featured bool
	Score    float64
	// Manual corresponds to the manual column: 1 means this member was
	// inserted by replaying a user pin edit (not produced by the engine this
	// round); used only for display/debugging to distinguish the source.
	Manual bool
	// AddedAt corresponds to the added_at column: the Unix ms timestamp of
	// when the member was added, 0=NULL (legacy data / join time unknown,
	// excluded from the "added this week" count). For internal/test use
	// only; the asset endpoint does not expose this field directly (see the
	// design brief).
	AddedAt int64
}

// MomentPlace is one aggregated result for the About multi-location display:
// how many times a city appears among a moment's members, returned by
// PlacesByMoment in descending order of count.
type MomentPlace struct {
	Name  string
	Count int
}

// MomentDraft is the candidate moment produced by the engine each
// recalculation round (a draft not yet persisted): it embeds Moment's
// display fields plus this round's full member set. SyncRecipeMoments
// idempotently merges it into the DB by ID.
type MomentDraft struct {
	Moment
	Assets []MomentAsset
}

// MomentStore is the repo layer for the three Smart Moments tables, plain
// SQL with no ORM (following the style of existing stores in this repo like
// captionpull.go).
type MomentStore struct {
	db *sql.DB
}

// NewMomentStore constructs a MomentStore.
func NewMomentStore(db *sql.DB) *MomentStore {
	return &MomentStore{db: db}
}

// nowMs returns the current Unix millisecond timestamp (the convention for
// the *_at columns of moments/moment_recipes).
func nowMs() int64 {
	return time.Now().UnixMilli()
}

// ── recipe seeds ─────────────────────────────────────────────────────────

// seedRecipe is a declarative description of a built-in recipe, assembled
// into a MomentRecipe for persistence.
type seedRecipe struct {
	key    string
	kind   string
	title  string
	params RecipeParams
}

// defaultSeedRecipes is the built-in set seeded at startup: trip (time
// window × location) + the first batch of themes (caption keywords + CLIP
// prompt union matching). clip_prompts are natural descriptive sentences for
// CLIP (not keyword stuffing), caption_keywords are lowercase words matched
// against asset_caption via instr(lower(text),...). Copy targets
// English-speaking users — the product's default display language is
// English, and Chinese is handled by the frontend i18n layer.
func defaultSeedRecipes() []seedRecipe {
	return []seedRecipe{
		{
			key: "trip", kind: "trip", title: "Trip",
			// trip's display name is generated dynamically by the engine from
			// the "{main city} Trip" template (see section 2 of the design
			// spec); the title here is just a generic label in the recipe
			// management list.
			params: RecipeParams{},
		},
		{
			key: "theme:pets", kind: "theme", title: "Pet Moments",
			params: RecipeParams{
				ClipPrompts:     []string{"a photo of a pet dog or cat", "a cute animal companion"},
				CaptionKeywords: []string{"dog", "cat", "puppy", "kitten", "pet"},
			},
		},
		{
			key: "theme:food", kind: "theme", title: "Food Moments",
			params: RecipeParams{
				ClipPrompts:     []string{"a plate of food on a table", "a close-up photo of a delicious meal"},
				CaptionKeywords: []string{"food", "meal", "dish", "restaurant", "cooking", "dinner"},
			},
		},
		{
			key: "theme:snow", kind: "theme", title: "Snow Days",
			params: RecipeParams{
				ClipPrompts:     []string{"a landscape covered in fresh snow", "people playing in the snow"},
				CaptionKeywords: []string{"snow", "snowy", "snowman", "skiing", "snowboard", "winter"},
			},
		},
		{
			key: "theme:beach", kind: "theme", title: "Beach Days",
			params: RecipeParams{
				ClipPrompts:     []string{"a sandy beach with ocean waves", "people relaxing on a beach by the sea"},
				CaptionKeywords: []string{"beach", "ocean", "sand", "seaside", "surf", "shore"},
			},
		},
		{
			key: "theme:sunset", kind: "theme", title: "Sunset Views",
			params: RecipeParams{
				ClipPrompts:     []string{"a beautiful sunset with an orange sky", "the sun setting over the horizon"},
				CaptionKeywords: []string{"sunset", "dusk", "golden hour", "horizon"},
			},
		},
		{
			// profile:pets is the profile-layer mining config (kind=pet_entities,
			// distinct from theme:pets above, which is the "whole-library
			// search for pet-related content" concept version): it does
			// word-boundary caption matching for each lexicon entry, tallies
			// photo count + month span, and only when it qualifies
			// (>=min_photos and >=min_months) does it get distilled into a
			// "the user's own dog/cat" entity — see section 1 of the design
			// spec for details. lexicon covers common dog/cat breeds + birds
			// + small pets, in English, for word-boundary matching;
			// multi-word phrases (like "maine coon") are matched as a
			// whole-phrase boundary.
			key: "profile:pets", kind: "pet_entities", title: "Pet Entities",
			params: RecipeParams{
				Lexicon:   petEntityLexicon(),
				MinPhotos: defaultMinPhotos, MinMonths: defaultMinMonths,
				ClipMinScore: defaultClipMinScore, ClipTopK: defaultClipTopK,
			},
		},
		{
			// profile:family is the profile-layer family mining config
			// (kind=family): named persons whose appearance frequency
			// qualifies for the top-K are distilled into named-person
			// moments, and frequent persons appearing together are distilled
			// into group-photo moments — see section 1 of the design spec
			// for details.
			key: "profile:family", kind: "family", title: "Family Entities",
			params: RecipeParams{
				TopPersons: defaultTopPersons, MinPersonPhotos: defaultMinPersonPhotos,
				MinTogetherPersons: defaultMinTogetherPersons, MinAssets: defaultMinAssets,
			},
		},
	}
}

// petEntityLexicon is the species/breed English word list used by profile:pets
// mining: covers common dog/cat breeds + birds + small pets, roughly 60-100
// words, for caption word-boundary matching. Deliberately excludes overly
// broad words (like a bare "dog"/"cat" — that's the job of the theme:pets
// concept version; what the profile layer needs is specific breed/species
// words that converge on "the user's particular one", since only a
// recurrence signal has discriminative power).
//
// Fixed after review (high-frequency false-match words, US market scenario):
//   - Removed newfoundland (homograph with the place name Newfoundland),
//     finch (a common surname, e.g. Atticus Finch), canary (homograph with
//     the Canary Islands), goldfish (homonym with the Goldfish cracker
//     brand, and real-goldfish captions usually write phrases like
//     "goldfish in a bowl" anyway, so the bare-word false-recall risk
//     outweighs the benefit).
//   - Disambiguated breeds whose single word is ambiguous into phrases:
//     akita->"akita dog", boxer->"boxer dog", greyhound->"greyhound dog"
//     (avoiding confusion with the boxer sport / greyhound bus etc.); bare
//     "shepherd" replaced with the specific breeds "german shepherd"/
//     "australian shepherd" (the bare word isn't specific enough on its
//     own, so might as well use the two high-frequency specific breeds
//     directly).
//   - Cat coat-pattern words (tabby/calico/tuxedo/ginger etc.) are
//     themselves extremely common in everyday speech but ambiguous as a
//     single word (e.g. "tuxedo" can mean the suit), yet VLM-generated
//     captions describing cats almost always say pattern+"cat" (e.g. "a
//     tabby cat") and rarely report the specific breed — dropping these
//     words would make most users' cats un-minable as entities. So the
//     trade-off here is: keep all the pattern words, but always anchor them
//     as "<pattern> cat" two-word phrases (rather than the bare pattern
//     word), leaving the "cat breed words are limited" exposure risk to be
//     backstopped by the mining threshold (the recurrence criteria of
//     min_photos/min_months) instead of throwing out the baby with the
//     bathwater at the word-list level.
func petEntityLexicon() []string {
	return []string{
		// ── Dogs (breeds, excludes generic "dog"/"puppy"; ambiguous words disambiguated per comment above) ──
		"beagle", "labrador", "corgi", "husky", "poodle", "terrier", "retriever",
		"bulldog", "dachshund", "chihuahua", "pug", "german shepherd",
		"australian shepherd", "collie", "spaniel", "dalmatian", "boxer dog",
		"rottweiler", "doberman", "schnauzer", "mastiff", "greyhound dog",
		"whippet", "pomeranian", "shih tzu", "maltese", "chow chow", "akita dog",
		"samoyed", "malamute", "bernese mountain dog", "labradoodle",
		"goldendoodle", "basset hound", "bloodhound",
		// ── Cats (breeds + coat-pattern words, pattern words all anchored as "<pattern> cat" phrases, see comment above) ──
		"tabby cat", "siamese", "persian cat", "maine coon", "ragdoll", "sphynx",
		"bengal cat", "calico cat", "tuxedo cat", "ginger cat", "orange cat",
		"tortoiseshell cat", "british shorthair", "scottish fold", "abyssinian",
		"burmese cat", "russian blue", "himalayan cat",
		// ── Birds (canary/finch removed due to place-name/surname homograph false recall, see comment above) ──
		"parrot", "parakeet", "cockatiel", "budgie", "macaw", "cockatoo",
		"lovebird",
		// ── Small pets (goldfish removed due to brand-name homonym, see comment above) ──
		"hamster", "rabbit", "bunny", "guinea pig", "turtle", "tortoise",
		"gecko", "ferret", "chinchilla", "hedgehog", "iguana",
	}
}

// SeedDefaultRecipes writes the built-in recipe set with INSERT OR IGNORE;
// keys that already exist (including ones already hot-updated by ops/the app
// store) are skipped as-is, never overwritten. Safe to call repeatedly on
// every startup (idempotent).
func (s *MomentStore) SeedDefaultRecipes() error {
	now := nowMs()
	for _, sr := range defaultSeedRecipes() {
		b, err := json.Marshal(sr.params)
		if err != nil {
			return fmt.Errorf("moments: marshal seed params %q: %w", sr.key, err)
		}
		if _, err := s.db.Exec(`
			INSERT OR IGNORE INTO moment_recipes(key, kind, title, params, enabled, updated_at)
			VALUES (?, ?, ?, ?, 1, ?)`,
			sr.key, sr.kind, sr.title, string(b), now,
		); err != nil {
			return fmt.Errorf("moments: seed recipe %q: %w", sr.key, err)
		}
	}
	return nil
}

// ── recipe read/write ────────────────────────────────────────────────────

// ListRecipes lists all recipes; when enabledOnly=true, only returns those
// with enabled=1.
func (s *MomentStore) ListRecipes(enabledOnly bool) ([]MomentRecipe, error) {
	q := `SELECT key, kind, title, params, enabled, updated_at FROM moment_recipes`
	if enabledOnly {
		q += ` WHERE enabled=1`
	}
	q += ` ORDER BY key`
	rows, err := s.db.Query(q)
	if err != nil {
		return nil, fmt.Errorf("moments: list recipes: %w", err)
	}
	defer rows.Close()

	var out []MomentRecipe
	for rows.Next() {
		var r MomentRecipe
		var enabled int
		if err := rows.Scan(&r.Key, &r.Kind, &r.Title, &r.ParamsJSON, &enabled, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("moments: scan recipe: %w", err)
		}
		r.Enabled = enabled != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpsertRecipes is the hot-update entry point for recipes: upserts all
// fields by Key, always writing the server's current time for updated_at
// (ignoring the caller-supplied UpdatedAt), used by `PUT
// /v1/photos/moments/recipes` to push new/changed type definitions.
func (s *MomentStore) UpsertRecipes(recipes []MomentRecipe) error {
	if len(recipes) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("moments: upsert recipes begin: %w", err)
	}
	now := nowMs()
	for _, r := range recipes {
		enabled := 0
		if r.Enabled {
			enabled = 1
		}
		if _, err := tx.Exec(`
			INSERT INTO moment_recipes(key, kind, title, params, enabled, updated_at)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET
				kind       = excluded.kind,
				title      = excluded.title,
				params     = excluded.params,
				enabled    = excluded.enabled,
				updated_at = excluded.updated_at`,
			r.Key, r.Kind, r.Title, r.ParamsJSON, enabled, now,
		); err != nil {
			tx.Rollback()
			return fmt.Errorf("moments: upsert recipe %q: %w", r.Key, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("moments: upsert recipes commit: %w", err)
	}
	return nil
}

// ── moments read/write ───────────────────────────────────────────────────

// nullTimeArg converts a zero-value time.Time into a SQL NULL parameter, and
// formats non-zero values as a string following this repo's existing
// DATETIME convention (same shape as assets.taken_at, see places.go).
func nullTimeArg(t time.Time) interface{} {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format("2006-01-02 15:04:05")
}

// SyncRecipeMoments is the persistence entry point for idempotent
// recalculation: inside a transaction, for each draft it upserts moments
// (existing rows with named_by_llm=1 keep their title/named_by_llm, other
// fields are updated) + does a diff-style member upsert (ON CONFLICT
// refreshes featured/score/manual but doesn't touch added_at; absent
// members are deleted but pinned editors are exempt, preventing "deleted
// then re-inserted by edit replay" from cycling pinned members' added_at
// into false freshness); it then deletes old moments under this recipeKey
// that aren't in this round's draft id set (cascading member cleanup),
// making disappeared moments (e.g. an old trip that no longer clusters
// together after the gap is re-split) exit the DB.
//
// Moments visible to the user don't flicker across recalculation: an upsert
// for the same id never deletes-then-inserts, it just refreshes fields in
// place.
func (s *MomentStore) SyncRecipeMoments(recipeKey string, drafts []MomentDraft) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("moments: sync begin: %w", err)
	}
	now := nowMs()
	keepIDs := make([]string, 0, len(drafts))

	for _, d := range drafts {
		namedByLLM := 0 // A draft (template-seeded) is never LLM-named; keep-or-override is handled by the CASE below.
		if _, err := tx.Exec(`
			INSERT INTO moments(id, recipe_key, title, subtitle, cover_asset_id, time_from, time_to, place, asset_count, named_by_llm, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				recipe_key     = excluded.recipe_key,
				title          = CASE WHEN named_by_llm=1 THEN title ELSE excluded.title END,
				subtitle       = excluded.subtitle,
				cover_asset_id = excluded.cover_asset_id,
				time_from      = excluded.time_from,
				time_to        = excluded.time_to,
				place          = excluded.place,
				asset_count    = excluded.asset_count,
				updated_at     = excluded.updated_at`,
			d.ID, d.RecipeKey, d.Title, d.Subtitle, nullableStr(d.CoverAssetID),
			nullTimeArg(d.TimeFrom), nullTimeArg(d.TimeTo), d.Place, d.AssetCount,
			namedByLLM, now, now,
		); err != nil {
			tx.Rollback()
			return fmt.Errorf("moments: upsert moment %q: %w", d.ID, err)
		}

		// Diff-style member upsert (spec 1.2's four-step semantics, preserving
		// added_at, semantically equivalent to the old "wholesale
		// delete+insert replacement"):
		//  1. Upsert each member of this round's draft: the conflict branch
		//     only refreshes featured/score/manual, never touching added_at
		//     (existing members keep their original join time; NULL stays
		//     NULL); truly new rows get stamped with the current timestamp.
		//  2. Delete old members "not produced this round", but pinned
		//     members are exempt — otherwise "deleted then re-inserted by
		//     applyMomentEdits replay below" would cycle pinned members'
		//     added_at to now every round (the false-freshness trap, see the
		//     spec).
		for _, a := range d.Assets {
			featured := 0
			if a.Featured {
				featured = 1
			}
			manual := 0
			if a.Manual {
				manual = 1
			}
			if _, err := tx.Exec(`
				INSERT INTO moment_assets(moment_id, asset_id, featured, score, manual, added_at)
				VALUES (?, ?, ?, ?, ?, ?)
				ON CONFLICT(moment_id, asset_id) DO UPDATE SET
					featured = excluded.featured,
					score    = excluded.score,
					manual   = excluded.manual`,
				d.ID, a.AssetID, featured, a.Score, manual, now,
			); err != nil {
				tx.Rollback()
				return fmt.Errorf("moments: upsert member %q/%q: %w", d.ID, a.AssetID, err)
			}
		}

		deleteMembersQ := `
			DELETE FROM moment_assets
			WHERE moment_id = ?`
		deleteMembersArgs := []interface{}{d.ID}
		if len(d.Assets) > 0 {
			placeholders := make([]string, len(d.Assets))
			for i, a := range d.Assets {
				placeholders[i] = "?"
				deleteMembersArgs = append(deleteMembersArgs, a.AssetID)
			}
			deleteMembersQ += ` AND asset_id NOT IN (` + strings.Join(placeholders, ",") + `)`
		}
		// The deletion exemption only recognizes live assets (aliveAssetExpr):
		// a pinned asset that has since gone to trash/offline is no longer
		// exempt, and gets deleted along with this diff upsert (the edits row
		// itself isn't deleted, so applyMomentEdits can automatically re-add
		// it once the asset comes back alive).
		deleteMembersQ += `
			AND asset_id NOT IN (
				SELECT me.asset_id FROM moment_edits me
				JOIN assets a ON a.id = me.asset_id
				WHERE me.moment_id=? AND me.op='pin' AND ` + aliveAssetExpr + `
			)`
		deleteMembersArgs = append(deleteMembersArgs, d.ID)
		if _, err := tx.Exec(deleteMembersQ, deleteMembersArgs...); err != nil {
			tx.Rollback()
			return fmt.Errorf("moments: delete stale members %q: %w", d.ID, err)
		}

		// edits replay: the engine's recalculation doesn't know about the
		// user's previous pin/exclude edits, so right after the member diff
		// upsert we layer the edits back on, preventing this round's
		// recalculation from silently wiping them out. Derived-field refresh
		// (count/time window/cover re-pick) is only triggered when this
		// moment has edits — a moment with no edit records keeps the derived
		// values the engine computed this round, no need for a redundant
		// recalculation.
		hasEdits, err := applyMomentEdits(tx, d.ID, now)
		if err != nil {
			tx.Rollback()
			return err
		}
		if hasEdits {
			// hadTimeWindow: only trip-kind moments (whose draft carries a
			// concrete TimeFrom) recalculate the time window from members'
			// taken_at; theme-kind moments (TimeFrom is a zero value) always
			// have a NULL time window and shouldn't get accidentally assigned
			// one by this recalculation.
			if err := refreshMomentDerived(tx, d.ID, !d.TimeFrom.IsZero()); err != nil {
				tx.Rollback()
				return fmt.Errorf("moments: sync refresh derived %q: %w", d.ID, err)
			}
		}
		keepIDs = append(keepIDs, d.ID)
	}

	// Delete old moments under this recipe that weren't produced this round
	// (moment_assets is cleaned up via FK ON DELETE CASCADE, no separate
	// DELETE needed). When drafts is empty, this clears all moments under
	// this recipe.
	deleteQ := `DELETE FROM moments WHERE recipe_key=?`
	args := []interface{}{recipeKey}
	if len(keepIDs) > 0 {
		placeholders := make([]string, len(keepIDs))
		for i, id := range keepIDs {
			placeholders[i] = "?"
			args = append(args, id)
		}
		deleteQ += ` AND id NOT IN (` + strings.Join(placeholders, ",") + `)`
	}
	if _, err := tx.Exec(deleteQ, args...); err != nil {
		tx.Rollback()
		return fmt.Errorf("moments: delete stale moments for %q: %w", recipeKey, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("moments: sync commit: %w", err)
	}
	return nil
}

// aliveAssetExpr is the SQL fragment for the "live asset" criterion, matching
// the same bar as the "live asset" clause in moments_theme.go's
// loadThemeCandidatePool (around L192): fully indexed (status='indexed'),
// not in trash (deleted_at IS NULL), not offline (offline=0). Note this is
// only a subset of the pool's full criteria — the pool also excludes the
// live-photo video side, documents (hasOcrExpr), and assets with no
// taken_at; pin deliberately doesn't apply those conditions (a photo the
// user manually added should have its intent respected even if it's a
// document), it only requires being "alive". Relies on the outer query
// aliasing the assets table as a. All three pin-related spots (diff upsert
// deletion exemption / replay re-insertion / immediate insertion) uniformly
// use this criterion to judge whether an asset is "alive" — a debt cleanup:
// previously these three spots only checked that the row existed in the
// assets table, without recognizing live-asset status, so a pinned photo
// that went to trash/offline would still cling to the moment and never
// leave; now the pin edit record (moment_edits) for a dead asset is still
// kept, it's just no longer exempted/re-inserted as a member — once the
// asset is restored from trash/offline, the next replay round automatically
// rejoins it.
const aliveAssetExpr = `a.status='indexed' AND a.deleted_at IS NULL AND a.offline=0`

// nullableStr converts an empty string into SQL NULL (cover_asset_id allows
// NULL, meaning "no cover yet"); non-empty strings pass through unchanged.
func nullableStr(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

// ListMoments lists all moments. Ordering semantics (see section 1 of the
// design spec): manually-ordered ones (sort_order non-NULL) come first, in
// ascending sort_order (the order the user gave via drag); manually
// unordered ones (sort_order NULL) come after the manually-ordered ones, in
// descending updated_at (most recently recalculated/named first). When
// nothing in the whole DB is manually ordered, this equals the previous
// behavior unchanged.
func (s *MomentStore) ListMoments() ([]Moment, error) {
	rows, err := s.db.Query(`
		SELECT id, recipe_key, title, subtitle, cover_asset_id, time_from, time_to,
		       place, asset_count, named_by_llm, created_at, updated_at, sort_order, hidden
		FROM moments
		WHERE hidden=0
		ORDER BY (sort_order IS NULL) ASC, sort_order ASC, updated_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("moments: list: %w", err)
	}
	defer rows.Close()

	var out []Moment
	for rows.Next() {
		m, err := scanMoment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// momentScanner is the minimal Scan interface shared by *sql.Row / *sql.Rows.
type momentScanner interface {
	Scan(dest ...interface{}) error
}

// scanMoment scans one moments row in ListMoments' column order, handling
// the cases where cover_asset_id/time_from/time_to/sort_order may be NULL.
func scanMoment(row momentScanner) (Moment, error) {
	var m Moment
	var cover sql.NullString
	var from, to sql.NullString
	var namedByLLM int
	var sortOrder sql.NullInt64
	var hidden int
	if err := row.Scan(&m.ID, &m.RecipeKey, &m.Title, &m.Subtitle, &cover, &from, &to,
		&m.Place, &m.AssetCount, &namedByLLM, &m.CreatedAt, &m.UpdatedAt, &sortOrder, &hidden); err != nil {
		return Moment{}, fmt.Errorf("moments: scan moment: %w", err)
	}
	if cover.Valid {
		m.CoverAssetID = cover.String
	}
	if t := parseSQLiteTime(from); t != nil {
		m.TimeFrom = *t
	}
	if t := parseSQLiteTime(to); t != nil {
		m.TimeTo = *t
	}
	m.NamedByLLM = namedByLLM != 0
	if sortOrder.Valid {
		v := int(sortOrder.Int64)
		m.SortOrder = &v
	}
	m.Hidden = hidden != 0
	return m, nil
}

// GetMomentAssets returns a moment's members, in descending score order;
// when featuredOnly=true, only returns featured (featured=1) members.
func (s *MomentStore) GetMomentAssets(id string, featuredOnly bool) ([]MomentAsset, error) {
	q := `SELECT asset_id, featured, score, manual, added_at FROM moment_assets WHERE moment_id=?`
	if featuredOnly {
		q += ` AND featured=1`
	}
	q += ` ORDER BY score DESC`
	rows, err := s.db.Query(q, id)
	if err != nil {
		return nil, fmt.Errorf("moments: get assets %q: %w", id, err)
	}
	defer rows.Close()

	var out []MomentAsset
	for rows.Next() {
		var a MomentAsset
		var featured, manual int
		var addedAt sql.NullInt64
		if err := rows.Scan(&a.AssetID, &featured, &a.Score, &manual, &addedAt); err != nil {
			return nil, fmt.Errorf("moments: scan member: %w", err)
		}
		a.Featured = featured != 0
		a.Manual = manual != 0
		if addedAt.Valid {
			a.AddedAt = addedAt.Int64
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ── Editable moments: pin/exclude/hidden ───────────────────────────────────

// PinMomentAssets force-adds a number of assets into a moment: writes an
// op='pin' moment_edits record (overwriting any prior edit on that asset)
// and immediately updates membership — a member the engine already included
// this round (with featured/score already set) is kept as-is via
// INSERT OR IGNORE without being downgraded, while an absent member gets a
// new manual=1/featured=0/score=0 row added. Ids that don't exist in the
// assets table are silently ignored (neither edits nor membership is
// touched). Triggers a derived-field refresh afterward (count/time
// window/cover re-pick), returning the refreshed asset_count.
func (s *MomentStore) PinMomentAssets(momentID string, assetIDs []string) (int, error) {
	return s.applyMomentEditOp(momentID, assetIDs, "pin")
}

// ExcludeMomentAssets force-removes a number of assets from a moment: writes
// an op='exclude' moment_edits record (overwriting any prior edit on that
// asset) and immediately deletes it from the member table. Ids that don't
// exist in the assets table are silently ignored. Triggers a derived-field
// refresh afterward, returning the refreshed asset_count (which is allowed
// to drop to 0).
func (s *MomentStore) ExcludeMomentAssets(momentID string, assetIDs []string) (int, error) {
	return s.applyMomentEditOp(momentID, assetIDs, "exclude")
}

// applyMomentEditOp is the shared implementation for Pin/ExcludeMomentAssets:
// inside a transaction, check each asset's existence one by one (skip if
// absent) -> upsert moment_edits (later write overwrites earlier) ->
// immediately update membership (pin only takes immediate effect for live
// assets — aliveAssetExpr — a dead asset only has its edits intent recorded,
// see the aliveAssetExpr comment) -> tally the member rows actually changed
// this call; when it's 0 (a no-op such as all-unknown ids / a pin target
// that's a dead asset / an exclude target that isn't even a member), skip
// the derived-field refresh and updated_at (a debt cleanup: previously this
// refreshed regardless of whether any row changed, so even a no-op would
// bump the moment to the front of ListMoments' ordering) -> read back
// asset_count.
func (s *MomentStore) applyMomentEditOp(momentID string, assetIDs []string, op string) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("moments: %s begin: %w", op, err)
	}
	now := nowMs()
	var affected int64 // member rows actually changed this call (sum of pin INSERT / exclude DELETE rows affected)
	for _, assetID := range assetIDs {
		// Unknown ids (not in the assets table) are silently ignored: no
		// edits written, no membership changed — moment_edits has a foreign
		// key constraint on assets (this repo's DSN has _foreign_keys=on), so
		// a blind write would error; checking existence explicitly here also
		// more clearly matches the "unknown id ignored" semantics.
		var exists int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM assets WHERE id=?`, assetID).Scan(&exists); err != nil {
			tx.Rollback()
			return 0, fmt.Errorf("moments: %s check asset %q: %w", op, assetID, err)
		}
		if exists == 0 {
			continue
		}
		if _, err := tx.Exec(`
			INSERT INTO moment_edits(moment_id, asset_id, op, created_at)
			VALUES (?, ?, ?, ?)
			ON CONFLICT(moment_id, asset_id) DO UPDATE SET
				op         = excluded.op,
				created_at = excluded.created_at`,
			momentID, assetID, op, now,
		); err != nil {
			tx.Rollback()
			return 0, fmt.Errorf("moments: %s upsert edit %q/%q: %w", op, momentID, assetID, err)
		}
		if op == "pin" {
			// Immediate insertion only recognizes live assets (aliveAssetExpr):
			// a dead asset's (trash/offline) pin intent has been written to
			// edits, but isn't immediately counted into membership/count —
			// consistent with the criterion used by SyncRecipeMoments replay;
			// once the asset comes back alive, the next replay round
			// automatically rejoins it.
			res, err := tx.Exec(`
				INSERT OR IGNORE INTO moment_assets(moment_id, asset_id, featured, score, manual, added_at)
				SELECT ?, a.id, 0, 0, 1, ?
				FROM assets a
				WHERE a.id=? AND `+aliveAssetExpr,
				momentID, now, assetID,
			)
			if err != nil {
				tx.Rollback()
				return 0, fmt.Errorf("moments: pin insert member %q/%q: %w", momentID, assetID, err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				tx.Rollback()
				return 0, fmt.Errorf("moments: pin rows affected %q/%q: %w", momentID, assetID, err)
			}
			affected += n
		} else {
			res, err := tx.Exec(`DELETE FROM moment_assets WHERE moment_id=? AND asset_id=?`,
				momentID, assetID,
			)
			if err != nil {
				tx.Rollback()
				return 0, fmt.Errorf("moments: exclude delete member %q/%q: %w", momentID, assetID, err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				tx.Rollback()
				return 0, fmt.Errorf("moments: exclude rows affected %q/%q: %w", momentID, assetID, err)
			}
			affected += n
		}
	}

	if affected == 0 {
		// This call caused no member row changes, so skip the derived-field
		// refresh and updated_at, and just read back the current asset_count
		// (which shouldn't change from a no-op).
		var count int
		if err := tx.QueryRow(`SELECT asset_count FROM moments WHERE id=?`, momentID).Scan(&count); err != nil {
			tx.Rollback()
			return 0, fmt.Errorf("moments: %s read count %q: %w", op, momentID, err)
		}
		if err := tx.Commit(); err != nil {
			return 0, fmt.Errorf("moments: %s commit %q: %w", op, momentID, err)
		}
		return count, nil
	}

	hadTimeWindow, err := momentHasTimeWindow(tx, momentID)
	if err != nil {
		tx.Rollback()
		return 0, err
	}
	if err := refreshMomentDerived(tx, momentID, hadTimeWindow); err != nil {
		tx.Rollback()
		return 0, fmt.Errorf("moments: %s refresh derived %q: %w", op, momentID, err)
	}

	var count int
	if err := tx.QueryRow(`SELECT asset_count FROM moments WHERE id=?`, momentID).Scan(&count); err != nil {
		tx.Rollback()
		return 0, fmt.Errorf("moments: %s read count %q: %w", op, momentID, err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("moments: %s commit %q: %w", op, momentID, err)
	}
	return count, nil
}

// HideMoment marks a moment as hidden (a tombstone): ListMoments will no
// longer return it afterward, but the row itself is kept (SyncRecipeMoments
// recalculation never clears this flag, since hidden isn't in the upsert
// column list).
func (s *MomentStore) HideMoment(momentID string) error {
	_, err := s.db.Exec(`UPDATE moments SET hidden=1, updated_at=? WHERE id=?`, nowMs(), momentID)
	if err != nil {
		return fmt.Errorf("moments: hide %q: %w", momentID, err)
	}
	return nil
}

// MomentEditsFor returns the edit records currently in effect for a moment,
// split into two slices by op (for the Task 3 mining engine to read, to
// tell whether an asset has been manually excluded/pinned by the user, so
// recalculation doesn't silently swallow the edit intent). Returns two empty
// slices, not an error, when there are no edit records.
func (s *MomentStore) MomentEditsFor(momentID string) (pins []string, excludes []string, err error) {
	rows, err := s.db.Query(`SELECT asset_id, op FROM moment_edits WHERE moment_id=? ORDER BY asset_id`, momentID)
	if err != nil {
		return nil, nil, fmt.Errorf("moments: edits for %q: %w", momentID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var assetID, op string
		if err := rows.Scan(&assetID, &op); err != nil {
			return nil, nil, fmt.Errorf("moments: scan edit: %w", err)
		}
		switch op {
		case "pin":
			pins = append(pins, assetID)
		case "exclude":
			excludes = append(excludes, assetID)
		}
	}
	return pins, excludes, rows.Err()
}

// TopFeaturedByMoment fetches featured members across the whole DB in one
// query (in descending score order), joining moments to exclude each one's
// own cover (the cover is already shown separately, no need to repeat it in
// the "featured" list), then groups by moment on the Go side and truncates
// each group to the first perMoment entries. Since SQL already returns rows
// in descending score order, rows for the same moment naturally keep their
// relative order in the result stream, so truncating per group is exactly
// "the top N highest-scoring within that moment". perMoment<=0 means no
// truncation.
func (s *MomentStore) TopFeaturedByMoment(perMoment int) (map[string][]string, error) {
	rows, err := s.db.Query(`
		SELECT ma.moment_id, ma.asset_id, ma.score
		FROM moment_assets ma
		JOIN moments m ON m.id = ma.moment_id
		WHERE ma.featured=1 AND (m.cover_asset_id IS NULL OR ma.asset_id <> m.cover_asset_id)
		ORDER BY ma.score DESC, ma.asset_id ASC`)
	if err != nil {
		return nil, fmt.Errorf("moments: top featured: %w", err)
	}
	defer rows.Close()

	out := map[string][]string{}
	for rows.Next() {
		var momentID, assetID string
		var score float64
		if err := rows.Scan(&momentID, &assetID, &score); err != nil {
			return nil, fmt.Errorf("moments: scan top featured: %w", err)
		}
		if perMoment > 0 && len(out[momentID]) >= perMoment {
			continue
		}
		out[momentID] = append(out[momentID], assetID)
	}
	return out, rows.Err()
}

// CoverRatioByMoment fetches the width/height ratio of every cover
// (cover_asset_id) across the whole DB in one query, joining asset_exif to
// get the cover asset's EXIF dimensions — the INNER JOIN naturally
// implements "a row missing exif doesn't enter the map" semantics (whether
// width/height is 0 or the cover has no asset_exif row at all, the caller
// treats any id that doesn't appear as 0=unknown, which the route layer
// renders as JSON cover_ratio=0). Same approach as
// TopFeaturedByMoment/AddedThisWeekByMoment: one whole-table query, not a
// per-moment query (no N+1).
func (s *MomentStore) CoverRatioByMoment() (map[string]float64, error) {
	rows, err := s.db.Query(`
		SELECT m.id, e.width, e.height
		FROM moments m
		JOIN asset_exif e ON e.asset_id = m.cover_asset_id`)
	if err != nil {
		return nil, fmt.Errorf("moments: cover ratio: %w", err)
	}
	defer rows.Close()

	out := map[string]float64{}
	for rows.Next() {
		var momentID string
		var width, height sql.NullInt64
		if err := rows.Scan(&momentID, &width, &height); err != nil {
			return nil, fmt.Errorf("moments: scan cover ratio: %w", err)
		}
		// If either width/height is missing (NULL) or <=0, treat it as an
		// invalid dimension and don't produce a ratio.
		if !width.Valid || !height.Valid || width.Int64 <= 0 || height.Int64 <= 0 {
			continue
		}
		out[momentID] = float64(width.Int64) / float64(height.Int64)
	}
	return out, rows.Err()
}

// sevenDaysMs is the AddedThisWeekByMoment stats window (7 days, in milliseconds).
const sevenDaysMs = int64(7 * 24 * 60 * 60 * 1000)

// AddedThisWeekByMoment counts the "added this week" member count for every
// moment across the whole DB in one query: only counted when added_at is
// non-NULL and >= nowMs-7d (NULL=legacy data / join time unknown, not
// counted, to avoid every photo in the whole DB showing +N during launch
// week). Same approach as TopFeaturedByMoment: one whole-table query grouped
// by moment_id on the Go side, not a per-moment query (no N+1). The returned
// map only contains moment ids with count>0; the caller should treat any id
// that doesn't appear as 0.
func (s *MomentStore) AddedThisWeekByMoment(nowMs int64) (map[string]int, error) {
	cutoff := nowMs - sevenDaysMs
	rows, err := s.db.Query(`
		SELECT moment_id, COUNT(*)
		FROM moment_assets
		WHERE added_at IS NOT NULL AND added_at >= ?
		GROUP BY moment_id`, cutoff)
	if err != nil {
		return nil, fmt.Errorf("moments: added this week: %w", err)
	}
	defer rows.Close()

	out := map[string]int{}
	for rows.Next() {
		var momentID string
		var count int
		if err := rows.Scan(&momentID, &count); err != nil {
			return nil, fmt.Errorf("moments: scan added this week: %w", err)
		}
		out[momentID] = count
	}
	return out, rows.Err()
}

// PlacesByMoment returns the occurrence count of a moment's members
// aggregated by city, for the About multi-location display (spec section
// 3): joins asset_geo, and a member with an empty city or no geo row is not
// counted; sorted by count DESC, city ASC (tie-break, to guarantee
// deterministic results), truncated to the first limit rows (limit<=0 means
// no truncation).
func (s *MomentStore) PlacesByMoment(momentID string, limit int) ([]MomentPlace, error) {
	q := `
		SELECT g.city, COUNT(*) AS c
		FROM moment_assets ma
		JOIN asset_geo g ON g.asset_id = ma.asset_id
		WHERE ma.moment_id = ? AND g.city IS NOT NULL AND g.city <> ''
		GROUP BY g.city
		ORDER BY c DESC, g.city ASC`
	args := []interface{}{momentID}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("moments: places by moment %q: %w", momentID, err)
	}
	defer rows.Close()

	out := make([]MomentPlace, 0)
	for rows.Next() {
		var p MomentPlace
		if err := rows.Scan(&p.Name, &p.Count); err != nil {
			return nil, fmt.Errorf("moments: scan place %q: %w", momentID, err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// momentHasTimeWindow determines whether a moment currently has a concrete
// time window (time_from non-NULL, i.e. a trip-kind moment); theme-kind
// moments (time_from is always NULL) shouldn't get accidentally assigned a
// time window by a derived-field refresh.
func momentHasTimeWindow(tx *sql.Tx, momentID string) (bool, error) {
	var from sql.NullString
	if err := tx.QueryRow(`SELECT time_from FROM moments WHERE id=?`, momentID).Scan(&from); err != nil {
		return false, fmt.Errorf("moments: check time window %q: %w", momentID, err)
	}
	return from.Valid, nil
}

// applyMomentEdits is SyncRecipeMoments' replay hook: after the member diff
// upsert, it layers the user's previous pin/exclude edits on this moment
// back on top. exclude is removed first, pin merged in after
// (INSERT OR IGNORE won't downgrade a member the engine already included
// this round — a row that already has featured/score set is kept as-is,
// only adding a manual=1/featured=0/score=0/added_at=now row when absent;
// since SyncRecipeMoments step 2 already exempts pinned members from
// deletion, a pinned member is normally already in the table, so this
// INSERT OR IGNORE won't fire and added_at won't be refreshed). The
// returned hasEdits indicates whether this moment has any edit records, for
// the caller to decide whether a derived-field refresh is needed (a moment
// with no edits keeps the derived values the engine computed this round, no
// need for a redundant recalculation). now is this round's SyncRecipeMoments
// timestamp, used only to stamp added_at on a truly newly-inserted pin row.
func applyMomentEdits(tx *sql.Tx, momentID string, now int64) (bool, error) {
	if _, err := tx.Exec(`
		DELETE FROM moment_assets
		WHERE moment_id = ?
		  AND asset_id IN (SELECT asset_id FROM moment_edits WHERE moment_id=? AND op='exclude')`,
		momentID, momentID,
	); err != nil {
		return false, fmt.Errorf("moments: replay exclude %q: %w", momentID, err)
	}
	// Replay re-insertion only recognizes live assets (aliveAssetExpr): pin
	// edits for a dead asset (trash/offline) won't be re-added as a member —
	// it's already been cleared from the member table by SyncRecipeMoments
	// step 2 above (no longer exempt) or by this function's exclude replay.
	if _, err := tx.Exec(`
		INSERT OR IGNORE INTO moment_assets(moment_id, asset_id, featured, score, manual, added_at)
		SELECT me.moment_id, me.asset_id, 0, 0, 1, ?
		FROM moment_edits me
		JOIN assets a ON a.id = me.asset_id
		WHERE me.moment_id=? AND me.op='pin' AND `+aliveAssetExpr,
		now, momentID,
	); err != nil {
		return false, fmt.Errorf("moments: replay pin %q: %w", momentID, err)
	}
	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM moment_edits WHERE moment_id=?`, momentID).Scan(&count); err != nil {
		return false, fmt.Errorf("moments: replay count edits %q: %w", momentID, err)
	}
	return count > 0, nil
}

// refreshMomentDerived is the shared implementation that recalculates a
// moment's derived fields after a pin/exclude membership change:
//   - asset_count: COUNT(*) on the member table.
//   - Time window (only when hadTimeWindow=true): take MIN/MAX(taken_at) by
//     joining assets on current members; when hadTimeWindow=false
//     (theme-kind moment), time_from/time_to are left untouched, preserving
//     their NULL semantics.
//   - Cover: left unchanged if the current cover is still a member;
//     otherwise re-picked by falling back in order through "highest-scoring
//     featured -> any member (take the first by score DESC, asset_id ASC, to
//     avoid test flakiness) -> NULL if no members".
func refreshMomentDerived(tx *sql.Tx, momentID string, hadTimeWindow bool) error {
	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM moment_assets WHERE moment_id=?`, momentID).Scan(&count); err != nil {
		return fmt.Errorf("moments: refresh count %q: %w", momentID, err)
	}

	setClauses := []string{"asset_count=?"}
	args := []interface{}{count}

	if hadTimeWindow {
		var from, to sql.NullString
		if err := tx.QueryRow(`
			SELECT MIN(a.taken_at), MAX(a.taken_at)
			FROM moment_assets ma JOIN assets a ON a.id = ma.asset_id
			WHERE ma.moment_id=?`, momentID).Scan(&from, &to); err != nil {
			return fmt.Errorf("moments: refresh time window %q: %w", momentID, err)
		}
		var fromTime, toTime time.Time
		if t := parseSQLiteTime(from); t != nil {
			fromTime = *t
		}
		if t := parseSQLiteTime(to); t != nil {
			toTime = *t
		}
		setClauses = append(setClauses, "time_from=?", "time_to=?")
		args = append(args, nullTimeArg(fromTime), nullTimeArg(toTime))
	}

	var currentCover sql.NullString
	if err := tx.QueryRow(`SELECT cover_asset_id FROM moments WHERE id=?`, momentID).Scan(&currentCover); err != nil {
		return fmt.Errorf("moments: refresh read cover %q: %w", momentID, err)
	}
	coverStillMember := false
	if currentCover.Valid {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM moment_assets WHERE moment_id=? AND asset_id=?`,
			momentID, currentCover.String).Scan(&n); err != nil {
			return fmt.Errorf("moments: refresh check cover member %q: %w", momentID, err)
		}
		coverStillMember = n > 0
	}

	newCover := currentCover
	if !currentCover.Valid || !coverStillMember {
		var pick sql.NullString
		err := tx.QueryRow(`
			SELECT asset_id FROM moment_assets WHERE moment_id=? AND featured=1
			ORDER BY score DESC, asset_id ASC LIMIT 1`, momentID).Scan(&pick)
		if err == sql.ErrNoRows {
			err = tx.QueryRow(`
				SELECT asset_id FROM moment_assets WHERE moment_id=?
				ORDER BY score DESC, asset_id ASC LIMIT 1`, momentID).Scan(&pick)
		}
		if err != nil && err != sql.ErrNoRows {
			return fmt.Errorf("moments: refresh pick cover %q: %w", momentID, err)
		}
		newCover = pick // on sql.ErrNoRows, pick stays zero-valued (Valid=false) -> falls back to NULL
	}
	setClauses = append(setClauses, "cover_asset_id=?", "updated_at=?")
	var coverArg interface{}
	if newCover.Valid {
		coverArg = newCover.String
	}
	args = append(args, coverArg, nowMs())
	args = append(args, momentID)

	q := fmt.Sprintf(`UPDATE moments SET %s WHERE id=?`, strings.Join(setClauses, ", "))
	if _, err := tx.Exec(q, args...); err != nil {
		return fmt.Errorf("moments: refresh update %q: %w", momentID, err)
	}
	return nil
}

// SetMomentTitle sets a moment's display name to the LLM-polished result and
// marks named_by_llm=1 — subsequent SyncRecipeMoments recalculations will
// keep this title, no longer overwritten by the template result.
func (s *MomentStore) SetMomentTitle(id, title string) error {
	_, err := s.db.Exec(`UPDATE moments SET title=?, named_by_llm=1, updated_at=? WHERE id=?`,
		title, nowMs(), id)
	if err != nil {
		return fmt.Errorf("moments: set title %q: %w", id, err)
	}
	return nil
}

// ReorderMoments is the persistence entry point for drag-and-drop ordering:
// inside a transaction, it assigns sort_order as (i+1)*10 in the order of
// ids (leaving gaps, so a future "insert between two" doesn't require a
// full re-sort). A moment id in ids that doesn't exist (e.g. the frontend's
// list is slightly stale, or the moment was already deleted by
// recalculation) results in an UPDATE affecting 0 rows, which is ignored
// without an error — a single stale id shouldn't fail the whole batch
// operation. An empty ids is intercepted upstream by the caller (the
// handler) as a 400; this function doesn't perform that check.
func (s *MomentStore) ReorderMoments(ids []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("moments: reorder begin: %w", err)
	}
	for i, id := range ids {
		sortOrder := (i + 1) * 10
		if _, err := tx.Exec(`UPDATE moments SET sort_order=? WHERE id=?`, sortOrder, id); err != nil {
			tx.Rollback()
			return fmt.Errorf("moments: reorder update %q: %w", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("moments: reorder commit: %w", err)
	}
	return nil
}

// ── Stable id derivation ─────────────────────────────────────────────────

// hashID16 returns the first 16 hex characters of the input string's sha1
// digest (64 bits, sufficient to avoid collisions at this repo's
// single-machine sqlite scale, and better suited as a display-layer id than
// the full 40-hex digest).
func hashID16(s string) string {
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}

// TripMomentID derives a trip-kind moment's stable id: hash(recipeKey + "|"
// + ISO week). ISO week is used (rather than the calendar week or a
// specific date) because when the same trip is recalculated, the split-out
// start date may shift by a few days as boundary photos are added or
// removed, but as long as it still falls within the same ISO week, the id
// stays the same — preventing the moment the user sees from "changing
// identity" between recalculations.
func TripMomentID(recipeKey string, from time.Time) string {
	year, week := from.ISOWeek()
	weekKey := fmt.Sprintf("%d-W%02d", year, week)
	return hashID16(recipeKey + "|" + weekKey)
}

// ThemeMomentID derives a theme-kind moment's stable id: hash(recipeKey).
// Theme kind is "one continuously-updated live set per recipe" (unlike trip,
// which is segmented by time window), so the id only depends on the recipe
// key — the same theme always maps to the same moment row, and
// recalculation just refreshes its members in place.
func ThemeMomentID(recipeKey string) string {
	return hashID16(recipeKey)
}
