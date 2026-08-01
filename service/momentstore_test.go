// Tests for MomentStore: three tables (moment_recipes/moments/moment_assets)
// + repo-layer semantics.
// Covers the design brief's Step 1 checklist: seed is idempotent and doesn't
// overwrite pushed recipes, UpsertRecipes hot-updates, SyncRecipeMoments'
// four semantics of upsert/member replacement/deleting disappeared
// moments/preserving the LLM title, id stability (same week -> same id),
// ParseParams defaults, plus this round's "editable moments" storage layer:
// moment_edits migration idempotency, pin/exclude replay survival, hidden
// tombstone, derived-field (asset_count/time window/cover) refresh, and
// TopFeaturedByMoment's shape.
package service

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	"github.com/stretchr/testify/require"
)

// insertMomentAsset inserts an asset row that moment_assets will reference
// via foreign key (same approach as captionpull_test.go's
// insertCaptionAsset — the id just needs to exist).
func insertMomentAsset(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO assets(id, file_path, status) VALUES(?,?,'indexed')`, id, "/g/"+id+".jpg")
	require.NoError(t, err)
}

// insertMomentAssetAt inserts an asset row with taken_at, for derived
// time-window refresh tests to use.
func insertMomentAssetAt(t *testing.T, db *sql.DB, id string, takenAt time.Time) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO assets(id, file_path, status, taken_at) VALUES(?,?,'indexed',?)`,
		id, "/g/"+id+".jpg", takenAt.UTC().Format("2006-01-02 15:04:05"))
	require.NoError(t, err)
}

// ── ParseParams defaults ─────────────────────────────────────────────────

func TestMomentStore_ParseParamsDefaults(t *testing.T) {
	// Empty params (no config pushed) should all fall back to defaults.
	p, err := ParseParams(MomentRecipe{ParamsJSON: ""})
	require.NoError(t, err)
	require.Equal(t, 10, p.MinAssets)
	require.Equal(t, 12, p.MaxFeatured)
	require.Equal(t, 14, p.GapDays)
	require.Equal(t, 200, p.TopK)
	require.Equal(t, 0.2, p.MinScore)

	p2, err := ParseParams(MomentRecipe{ParamsJSON: "{}"})
	require.NoError(t, err)
	require.Equal(t, 10, p2.MinAssets)

	// When some fields are explicitly specified, only those take effect; the
	// rest still fall back to defaults.
	p3, err := ParseParams(MomentRecipe{ParamsJSON: `{"min_assets":5,"clip_prompts":["a cat"]}`})
	require.NoError(t, err)
	require.Equal(t, 5, p3.MinAssets)
	require.Equal(t, 12, p3.MaxFeatured, "an unspecified field should fall back to its default")
	require.Equal(t, []string{"a cat"}, p3.ClipPrompts)

	// Invalid JSON should return an err.
	_, err = ParseParams(MomentRecipe{ParamsJSON: `{bad json`})
	require.Error(t, err)
}

// ── ParseParams: profile new-field defaults (old fields/defaults unaffected) ──

func TestMomentStore_ParseParamsProfileDefaults(t *testing.T) {
	p, err := ParseParams(MomentRecipe{ParamsJSON: ""})
	require.NoError(t, err)
	require.Nil(t, p.Lexicon, "unspecified lexicon should be empty, shouldn't fall back to a guessed default word list")
	require.Equal(t, 8, p.MinPhotos)
	require.Equal(t, 2, p.MinMonths)
	require.Equal(t, 0.45, p.ClipMinScore)
	require.Equal(t, 100, p.ClipTopK)
	require.Equal(t, 5, p.TopPersons)
	require.Equal(t, 30, p.MinPersonPhotos)
	require.Equal(t, 2, p.MinTogetherPersons)
	// Old field defaults are unaffected.
	require.Equal(t, 10, p.MinAssets)
	require.Equal(t, 12, p.MaxFeatured)
	require.Equal(t, 14, p.GapDays)
	require.Equal(t, 200, p.TopK)
	require.Equal(t, 0.2, p.MinScore)

	// When some fields are explicitly specified, only those take effect; the
	// rest still fall back to defaults.
	p2, err := ParseParams(MomentRecipe{ParamsJSON: `{"min_photos":20,"lexicon":["beagle"]}`})
	require.NoError(t, err)
	require.Equal(t, 20, p2.MinPhotos)
	require.Equal(t, []string{"beagle"}, p2.Lexicon)
	require.Equal(t, 2, p2.MinMonths, "an unspecified field should fall back to its default")
}

// ── SeedDefaultRecipes: idempotent + doesn't overwrite pushed recipes ──────

func TestMomentStore_SeedIdempotentAndDoesNotOverwritePushed(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)

	require.NoError(t, store.SeedDefaultRecipes())
	recipes, err := store.ListRecipes(false)
	require.NoError(t, err)
	require.Len(t, recipes, 8, "built-in set: trip + 5 themes + 2 profiles")

	keys := map[string]MomentRecipe{}
	for _, r := range recipes {
		keys[r.Key] = r
	}
	require.Contains(t, keys, "trip")
	require.Contains(t, keys, "theme:pets")
	require.Contains(t, keys, "theme:food")
	require.Contains(t, keys, "theme:snow")
	require.Contains(t, keys, "theme:beach")
	require.Contains(t, keys, "theme:sunset")
	require.Contains(t, keys, "profile:pets")
	require.Contains(t, keys, "profile:family")
	require.Equal(t, "theme", keys["theme:pets"].Kind)
	require.Equal(t, "trip", keys["trip"].Kind)
	require.Equal(t, "pet_entities", keys["profile:pets"].Kind)
	require.Equal(t, "family", keys["profile:family"].Kind)

	// profile:pets' lexicon should have a substantive word list (~60-100
	// English species/breed words).
	petParams, err := ParseParams(keys["profile:pets"])
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(petParams.Lexicon), 60, "lexicon should cover enough species/breed words")
	require.LessOrEqual(t, len(petParams.Lexicon), 100)
	require.Contains(t, petParams.Lexicon, "beagle")
	require.Contains(t, petParams.Lexicon, "labrador")
	require.Contains(t, petParams.Lexicon, "tabby cat")
	require.Contains(t, petParams.Lexicon, "parrot")
	require.Equal(t, 8, petParams.MinPhotos)
	require.Equal(t, 2, petParams.MinMonths)
	require.Equal(t, 0.45, petParams.ClipMinScore)
	require.Equal(t, 100, petParams.ClipTopK)

	familyParams, err := ParseParams(keys["profile:family"])
	require.NoError(t, err)
	require.Equal(t, 5, familyParams.TopPersons)
	require.Equal(t, 30, familyParams.MinPersonPhotos)
	require.Equal(t, 2, familyParams.MinTogetherPersons)
	require.Equal(t, 10, familyParams.MinAssets, "family reuses the min_assets field (group-photo threshold)")

	// Seeding again should stay idempotent, no error, no duplicates.
	require.NoError(t, store.SeedDefaultRecipes())
	recipes2, err := store.ListRecipes(false)
	require.NoError(t, err)
	require.Len(t, recipes2, 8)

	// Simulate ops/the app store having already pushed a hot update to
	// theme:pets.
	require.NoError(t, store.UpsertRecipes([]MomentRecipe{
		{Key: "theme:pets", Kind: "theme", Title: "Custom Pets", ParamsJSON: `{"min_assets":5}`, Enabled: true},
	}))

	// Seeding again shouldn't overwrite the already-pushed recipe back to
	// default copy.
	require.NoError(t, store.SeedDefaultRecipes())
	recipes3, err := store.ListRecipes(false)
	require.NoError(t, err)
	for _, r := range recipes3 {
		if r.Key == "theme:pets" {
			require.Equal(t, "Custom Pets", r.Title, "seed shouldn't overwrite an already-pushed recipe")
		}
	}
}

// ── UpsertRecipes: hot-update entry point ──────────────────────────────────

func TestMomentStore_UpsertRecipesHotUpdate(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)

	before := time.Now().UnixMilli()
	require.NoError(t, store.UpsertRecipes([]MomentRecipe{
		{Key: "theme:art", Kind: "theme", Title: "Art Moments", ParamsJSON: `{"min_assets":8}`, Enabled: true},
	}))
	after := time.Now().UnixMilli()

	recipes, err := store.ListRecipes(false)
	require.NoError(t, err)
	require.Len(t, recipes, 1)
	r := recipes[0]
	require.Equal(t, "theme:art", r.Key)
	require.Equal(t, "Art Moments", r.Title)
	require.True(t, r.Enabled)
	require.GreaterOrEqual(t, r.UpdatedAt, before)
	require.LessOrEqual(t, r.UpdatedAt, after)

	// Upserting the same key again should overwrite all fields (hot update).
	require.NoError(t, store.UpsertRecipes([]MomentRecipe{
		{Key: "theme:art", Kind: "theme", Title: "Art & Design", ParamsJSON: `{"min_assets":3}`, Enabled: false},
	}))
	recipes2, err := store.ListRecipes(false)
	require.NoError(t, err)
	require.Len(t, recipes2, 1)
	require.Equal(t, "Art & Design", recipes2[0].Title)
	require.False(t, recipes2[0].Enabled)

	// enabledOnly filtering.
	onlyEnabled, err := store.ListRecipes(true)
	require.NoError(t, err)
	require.Len(t, onlyEnabled, 0, "the recipe has been disabled, enabledOnly should filter it out")
}

// ── SyncRecipeMoments: upsert + full member replacement ────────────────────

func TestMomentStore_SyncUpsertsAndReplacesMembers(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)
	insertMomentAsset(t, db, "a1")
	insertMomentAsset(t, db, "a2")
	insertMomentAsset(t, db, "a3")

	draft := MomentDraft{
		Moment: Moment{
			ID:         "m1",
			RecipeKey:  "trip",
			Title:      "Yosemite Trip",
			Subtitle:   "May 2011 · Yosemite",
			Place:      "Yosemite",
			AssetCount: 2,
		},
		Assets: []MomentAsset{
			{AssetID: "a1", Featured: true, Score: 0.9},
			{AssetID: "a2", Featured: false, Score: 0.5},
		},
	}
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{draft}))

	moments, err := store.ListMoments()
	require.NoError(t, err)
	require.Len(t, moments, 1)
	require.Equal(t, "m1", moments[0].ID)
	require.Equal(t, "Yosemite Trip", moments[0].Title)
	require.Equal(t, 2, moments[0].AssetCount)
	require.False(t, moments[0].NamedByLLM)

	members, err := store.GetMomentAssets("m1", false)
	require.NoError(t, err)
	require.Len(t, members, 2)

	// Recalculation: the member set changes (drop a2, add a3) — should be a
	// full replacement, not a merge.
	draft2 := draft
	draft2.AssetCount = 2
	draft2.Assets = []MomentAsset{
		{AssetID: "a1", Featured: true, Score: 0.9},
		{AssetID: "a3", Featured: false, Score: 0.4},
	}
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{draft2}))

	members2, err := store.GetMomentAssets("m1", false)
	require.NoError(t, err)
	require.Len(t, members2, 2)
	ids := map[string]bool{}
	for _, m := range members2 {
		ids[m.AssetID] = true
	}
	require.True(t, ids["a1"])
	require.True(t, ids["a3"])
	require.False(t, ids["a2"], "old members should be cleared by the full replacement")
}

// ── SyncRecipeMoments: delete disappeared moments (cascading member cleanup) ──

func TestMomentStore_SyncDeletesDisappearedMoments(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)
	insertMomentAsset(t, db, "a1")
	insertMomentAsset(t, db, "a2")

	// First produce two moments under the same recipe.
	d1 := MomentDraft{Moment: Moment{ID: "m1", RecipeKey: "trip", Title: "Trip One", AssetCount: 1}, Assets: []MomentAsset{{AssetID: "a1"}}}
	d2 := MomentDraft{Moment: Moment{ID: "m2", RecipeKey: "trip", Title: "Trip Two", AssetCount: 1}, Assets: []MomentAsset{{AssetID: "a2"}}}
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{d1, d2}))

	moments, err := store.ListMoments()
	require.NoError(t, err)
	require.Len(t, moments, 2)

	// A moment under a different recipe shouldn't be affected.
	dOther := MomentDraft{Moment: Moment{ID: "m3", RecipeKey: "theme:pets", Title: "Pet Moments", AssetCount: 1}, Assets: []MomentAsset{{AssetID: "a1"}}}
	require.NoError(t, store.SyncRecipeMoments("theme:pets", []MomentDraft{dOther}))

	// Next recalculation round: trip only produces m1, m2 disappears.
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{d1}))

	moments2, err := store.ListMoments()
	require.NoError(t, err)
	require.Len(t, moments2, 2, "m2 should be deleted, m1 and the other recipe's m3 should be kept")
	idSet := map[string]bool{}
	for _, m := range moments2 {
		idSet[m.ID] = true
	}
	require.True(t, idSet["m1"])
	require.True(t, idSet["m3"])
	require.False(t, idSet["m2"], "a disappeared moment should be deleted")

	// m2's members should be cascade-cleaned along with it (won't become
	// orphaned moment_assets).
	var orphanCount int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM moment_assets WHERE moment_id='m2'`).Scan(&orphanCount))
	require.Equal(t, 0, orphanCount)
}

// ── SyncRecipeMoments: preserve an LLM-named title ─────────────────────────

func TestMomentStore_SyncPreservesLLMNamedTitle(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)
	insertMomentAsset(t, db, "a1")

	draft := MomentDraft{
		Moment: Moment{ID: "m1", RecipeKey: "trip", Title: "Yosemite Trip", Subtitle: "May 2011", AssetCount: 1},
		Assets: []MomentAsset{{AssetID: "a1"}},
	}
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{draft}))
	require.NoError(t, store.SetMomentTitle("m1", "An Amazing Yosemite Getaway"))

	moments, err := store.ListMoments()
	require.NoError(t, err)
	require.Equal(t, "An Amazing Yosemite Getaway", moments[0].Title)
	require.True(t, moments[0].NamedByLLM)

	// Next recalculation round: the template recomputes a different
	// title/subtitle, but since the LLM already named it, the title should
	// be preserved.
	draft2 := draft
	draft2.Title = "Yosemite Trip (Recomputed)"
	draft2.Subtitle = "May-June 2011"
	draft2.AssetCount = 5
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{draft2}))

	moments2, err := store.ListMoments()
	require.NoError(t, err)
	require.Equal(t, "An Amazing Yosemite Getaway", moments2[0].Title, "an LLM-named title shouldn't be overwritten by recalculation")
	require.True(t, moments2[0].NamedByLLM)
	require.Equal(t, "May-June 2011", moments2[0].Subtitle, "non-title fields should still update normally")
	require.Equal(t, 5, moments2[0].AssetCount)
}

// ── id stability ─────────────────────────────────────────────────────────

func TestMomentStore_IDStability(t *testing.T) {
	// Different dates within the same ISO week should get the same trip
	// moment id (a slight date shift on recalculation doesn't change the id).
	t1 := time.Date(2011, 5, 9, 0, 0, 0, 0, time.UTC)  // 2011-W19
	t2 := time.Date(2011, 5, 12, 0, 0, 0, 0, time.UTC) // same week
	require.Equal(t, TripMomentID("trip", t1), TripMomentID("trip", t2))

	t3 := time.Date(2011, 5, 20, 0, 0, 0, 0, time.UTC) // next week
	require.NotEqual(t, TripMomentID("trip", t1), TripMomentID("trip", t3), "crossing a week boundary should change the id")

	require.NotEqual(t, TripMomentID("trip", t1), TripMomentID("trip2", t1), "a different recipe should change the id")

	// A theme moment's id depends only on the recipe key, staying constant
	// across rolling updates.
	require.Equal(t, ThemeMomentID("theme:pets"), ThemeMomentID("theme:pets"))
	require.NotEqual(t, ThemeMomentID("theme:pets"), ThemeMomentID("theme:food"))

	// The id should be a 16-character hex string (the first 16 hex chars of sha1).
	require.Len(t, TripMomentID("trip", t1), 16)
	require.Len(t, ThemeMomentID("theme:pets"), 16)
}

// ── ListMoments ordered by updated_at desc; GetMomentAssets featured filter ──

func TestMomentStore_ListMomentsOrderAndFeaturedFilter(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)
	insertMomentAsset(t, db, "a1")
	insertMomentAsset(t, db, "a2")
	insertMomentAsset(t, db, "a3")

	d1 := MomentDraft{Moment: Moment{ID: "m1", RecipeKey: "trip", Title: "First", AssetCount: 1}, Assets: []MomentAsset{{AssetID: "a1"}}}
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{d1}))
	time.Sleep(2 * time.Millisecond)

	d2 := MomentDraft{
		Moment: Moment{ID: "m2", RecipeKey: "theme:pets", Title: "Second", AssetCount: 2},
		Assets: []MomentAsset{
			{AssetID: "a2", Featured: true, Score: 0.9},
			{AssetID: "a3", Featured: false, Score: 0.3},
		},
	}
	require.NoError(t, store.SyncRecipeMoments("theme:pets", []MomentDraft{d2}))

	moments, err := store.ListMoments()
	require.NoError(t, err)
	require.Len(t, moments, 2)
	require.Equal(t, "m2", moments[0].ID, "the most recently updated should come first")
	require.Equal(t, "m1", moments[1].ID)

	all, err := store.GetMomentAssets("m2", false)
	require.NoError(t, err)
	require.Len(t, all, 2)

	featured, err := store.GetMomentAssets("m2", true)
	require.NoError(t, err)
	require.Len(t, featured, 1)
	require.Equal(t, "a2", featured[0].AssetID)
}

// ── ListMoments: manual-ordering semantics (sort_order column) ─────────────
//
// Three-part semantics (see section 1 of the design spec):
//  1. Manually-ordered ones (sort_order non-NULL) come first, ascending by
//     sort_order;
//  2. Unordered ones (sort_order NULL) come after the manually-ordered ones,
//     descending by updated_at;
//  3. When nothing in the whole DB is manually ordered = unchanged from
//     current behavior (pure updated_at descending, backward-compatible
//     with existing assertions).
func TestMomentStore_ListMomentsSortOrderSemantics(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)
	insertMomentAsset(t, db, "a1")
	insertMomentAsset(t, db, "a2")
	insertMomentAsset(t, db, "a3")

	// Produce m1 (oldest) -> m2 -> m3 (newest) in sequence, updated_at increasing.
	d1 := MomentDraft{Moment: Moment{ID: "m1", RecipeKey: "trip", Title: "First", AssetCount: 1}, Assets: []MomentAsset{{AssetID: "a1"}}}
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{d1}))
	time.Sleep(2 * time.Millisecond)
	d2 := MomentDraft{Moment: Moment{ID: "m2", RecipeKey: "theme:pets", Title: "Second", AssetCount: 1}, Assets: []MomentAsset{{AssetID: "a2"}}}
	require.NoError(t, store.SyncRecipeMoments("theme:pets", []MomentDraft{d2}))
	time.Sleep(2 * time.Millisecond)
	d3 := MomentDraft{Moment: Moment{ID: "m3", RecipeKey: "theme:food", Title: "Third", AssetCount: 1}, Assets: []MomentAsset{{AssetID: "a3"}}}
	require.NoError(t, store.SyncRecipeMoments("theme:food", []MomentDraft{d3}))

	// Part 3: nothing in the whole DB manually ordered = unchanged, sorted by
	// updated_at DESC (the newest, m3, comes first).
	all, err := store.ListMoments()
	require.NoError(t, err)
	require.Len(t, all, 3)
	require.Equal(t, []string{"m3", "m2", "m1"}, []string{all[0].ID, all[1].ID, all[2].ID})
	require.Nil(t, all[0].SortOrder, "SortOrder should be nil when not manually ordered (faithful NULL semantics)")

	// Parts 1+2: manually order m1, m2 (m1 before m2, even though m1's
	// updated_at is older), m3 remains unordered. The manually-ordered ones
	// should come before the unordered one (m3) as a whole, in the order the
	// user gave internally.
	require.NoError(t, store.ReorderMoments([]string{"m1", "m2"}))

	mixed, err := store.ListMoments()
	require.NoError(t, err)
	require.Len(t, mixed, 3)
	require.Equal(t, []string{"m1", "m2", "m3"}, []string{mixed[0].ID, mixed[1].ID, mixed[2].ID},
		"manually-ordered m1/m2 should come before unordered m3, and in manual order internally")
	require.NotNil(t, mixed[0].SortOrder)
	require.Equal(t, 10, *mixed[0].SortOrder)
	require.NotNil(t, mixed[1].SortOrder)
	require.Equal(t, 20, *mixed[1].SortOrder)
	require.Nil(t, mixed[2].SortOrder, "m3 isn't manually ordered, SortOrder should still be nil")
}

// ── ReorderMoments: assignment and gaps + unknown id ignored ───────────────

func TestMomentStore_ReorderAssignsGapsAndIgnoresUnknown(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)
	insertMomentAsset(t, db, "a1")
	insertMomentAsset(t, db, "a2")
	insertMomentAsset(t, db, "a3")

	d1 := MomentDraft{Moment: Moment{ID: "m1", RecipeKey: "trip", Title: "One", AssetCount: 1}, Assets: []MomentAsset{{AssetID: "a1"}}}
	d2 := MomentDraft{Moment: Moment{ID: "m2", RecipeKey: "theme:pets", Title: "Two", AssetCount: 1}, Assets: []MomentAsset{{AssetID: "a2"}}}
	d3 := MomentDraft{Moment: Moment{ID: "m3", RecipeKey: "theme:food", Title: "Three", AssetCount: 1}, Assets: []MomentAsset{{AssetID: "a3"}}}
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{d1}))
	require.NoError(t, store.SyncRecipeMoments("theme:pets", []MomentDraft{d2}))
	require.NoError(t, store.SyncRecipeMoments("theme:food", []MomentDraft{d3}))

	// Mix in an unknown id in the middle ("ghost" doesn't exist in the
	// moments table), should affect 0 rows, no error.
	require.NoError(t, store.ReorderMoments([]string{"m1", "ghost", "m2", "m3"}))

	moments, err := store.ListMoments()
	require.NoError(t, err)
	byID := map[string]Moment{}
	for _, m := range moments {
		byID[m.ID] = m
	}
	require.NotNil(t, byID["m1"].SortOrder)
	require.Equal(t, 10, *byID["m1"].SortOrder, "m1 is ids[0], assigned (0+1)*10=10")
	require.NotNil(t, byID["m2"].SortOrder)
	require.Equal(t, 30, *byID["m2"].SortOrder, "m2 is ids[2] (ghost took index 1), assigned (2+1)*10=30")
	require.NotNil(t, byID["m3"].SortOrder)
	require.Equal(t, 40, *byID["m3"].SortOrder, "m3 is ids[3], assigned (3+1)*10=40")
}

// ── SyncRecipeMoments: recalculation doesn't touch an already manually-ordered sort_order (survives) ──

func TestMomentStore_SyncPreservesSortOrder(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)
	insertMomentAsset(t, db, "a1")

	draft := MomentDraft{
		Moment: Moment{ID: "m1", RecipeKey: "trip", Title: "Yosemite Trip", AssetCount: 1},
		Assets: []MomentAsset{{AssetID: "a1"}},
	}
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{draft}))
	require.NoError(t, store.ReorderMoments([]string{"m1"}))

	moments, err := store.ListMoments()
	require.NoError(t, err)
	require.Len(t, moments, 1)
	require.NotNil(t, moments[0].SortOrder)
	require.Equal(t, 10, *moments[0].SortOrder)

	// Next recalculation round (same-id upsert), sort_order shouldn't be touched.
	draft2 := draft
	draft2.Title = "Yosemite Trip (Recomputed)"
	draft2.AssetCount = 5
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{draft2}))

	moments2, err := store.ListMoments()
	require.NoError(t, err)
	require.Len(t, moments2, 1)
	require.NotNil(t, moments2[0].SortOrder, "Sync upsert shouldn't clear an already manually-ordered sort_order")
	require.Equal(t, 10, *moments2[0].SortOrder)
	require.Equal(t, "Yosemite Trip (Recomputed)", moments2[0].Title)
}

// ── Editable moments: moment_edits migration idempotency ───────────────────

// TestMomentStore_MigrationIdempotent opens (= migrates) the same DB file
// three times in a row, confirming that neither the idempotent column-add
// for moments.hidden / moment_assets.manual nor the moment_edits table
// creation errors out on repeated migration (e.g. "duplicate column"/"table
// already exists").
func TestMomentStore_MigrationIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "migrate.db")

	for i := 0; i < 3; i++ {
		db, err := sqlite.Open(path)
		require.NoError(t, err, "migration #%d shouldn't error", i+1)

		// Verify the new column/table actually exists, not just that "no error occurred".
		var hiddenCol, manualCol bool
		hRows, err := db.Query(`PRAGMA table_info(moments)`)
		require.NoError(t, err)
		for hRows.Next() {
			var cid int
			var name, ctype string
			var notnull, pk int
			var dflt sql.NullString
			require.NoError(t, hRows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk))
			if name == "hidden" {
				hiddenCol = true
			}
		}
		hRows.Close()
		require.True(t, hiddenCol, "moments.hidden column should exist")

		mRows, err := db.Query(`PRAGMA table_info(moment_assets)`)
		require.NoError(t, err)
		for mRows.Next() {
			var cid int
			var name, ctype string
			var notnull, pk int
			var dflt sql.NullString
			require.NoError(t, mRows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk))
			if name == "manual" {
				manualCol = true
			}
		}
		mRows.Close()
		require.True(t, manualCol, "moment_assets.manual column should exist")

		var addedAtCol bool
		aaRows, err := db.Query(`PRAGMA table_info(moment_assets)`)
		require.NoError(t, err)
		for aaRows.Next() {
			var cid int
			var name, ctype string
			var notnull, pk int
			var dflt sql.NullString
			require.NoError(t, aaRows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk))
			if name == "added_at" {
				addedAtCol = true
			}
		}
		aaRows.Close()
		require.True(t, addedAtCol, "moment_assets.added_at column should exist")

		var tblCount int
		require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='moment_edits'`).Scan(&tblCount))
		require.Equal(t, 1, tblCount, "moment_edits table should exist")

		require.NoError(t, db.Close())
	}
}

// ── Editable moments: pin survives recalculation ────────────────────────────

func TestMomentStore_PinSurvivesRecompute(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)
	insertMomentAsset(t, db, "a1")
	insertMomentAsset(t, db, "a2")

	draft := MomentDraft{
		Moment: Moment{ID: "m1", RecipeKey: "trip", Title: "Trip", AssetCount: 1},
		Assets: []MomentAsset{{AssetID: "a1", Score: 0.5}},
	}
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{draft}))

	// The user force-pins a2 (not included by the engine this round).
	count, err := store.PinMomentAssets("m1", []string{"a2"})
	require.NoError(t, err)
	require.Equal(t, 2, count)

	// Next recalculation round: the engine still only produces a1, but a2
	// should survive via edits replay.
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{draft}))

	members, err := store.GetMomentAssets("m1", false)
	require.NoError(t, err)
	ids := map[string]MomentAsset{}
	for _, m := range members {
		ids[m.AssetID] = m
	}
	require.Contains(t, ids, "a1")
	require.Contains(t, ids, "a2", "pin should survive after recalculation")
	require.True(t, ids["a2"].Manual, "a member inserted by replay should be marked manual=1")
}

// ── Editable moments: pin doesn't downgrade a member the engine already included (INSERT OR IGNORE semantics) ──

func TestMomentStore_PinDoesNotDowngradeExistingEngineMember(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)
	insertMomentAsset(t, db, "a1")

	// a1 has already been included by the engine this round as a featured
	// member (featured=1, score>0).
	draft := MomentDraft{
		Moment: Moment{ID: "m1", RecipeKey: "trip", Title: "Trip", AssetCount: 1},
		Assets: []MomentAsset{{AssetID: "a1", Featured: true, Score: 0.9}},
	}
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{draft}))

	// Calling PinMomentAssets on the same asset: INSERT OR IGNORE shouldn't
	// downgrade the existing row by overwriting it with a manual
	// featured=0/score=0 insert.
	count, err := store.PinMomentAssets("m1", []string{"a1"})
	require.NoError(t, err)
	require.Equal(t, 1, count)

	members, err := store.GetMomentAssets("m1", false)
	require.NoError(t, err)
	require.Len(t, members, 1)
	require.True(t, members[0].Featured, "pinning an asset that's already an engine member shouldn't downgrade featured to 0")
	require.Equal(t, 0.9, members[0].Score, "pinning an asset that's already an engine member shouldn't zero out score")

	// moment_edits should retain the pin record.
	pins, excludes, err := store.MomentEditsFor("m1")
	require.NoError(t, err)
	require.Equal(t, []string{"a1"}, pins)
	require.Empty(t, excludes)

	// Next recalculation round (the engine still includes a1), still
	// shouldn't be downgraded after replay.
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{draft}))
	members2, err := store.GetMomentAssets("m1", false)
	require.NoError(t, err)
	require.Len(t, members2, 1)
	require.True(t, members2[0].Featured, "featured still shouldn't be downgraded after recalculation replay")
	require.Equal(t, 0.9, members2[0].Score, "score still shouldn't be downgraded after recalculation replay")
}

// ── Editable moments: exclude survives recalculation ────────────────────────

func TestMomentStore_ExcludeSurvivesRecompute(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)
	insertMomentAsset(t, db, "a1")
	insertMomentAsset(t, db, "a2")

	draft := MomentDraft{
		Moment: Moment{ID: "m1", RecipeKey: "trip", Title: "Trip", AssetCount: 2},
		Assets: []MomentAsset{{AssetID: "a1", Score: 0.5}, {AssetID: "a2", Score: 0.4}},
	}
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{draft}))

	count, err := store.ExcludeMomentAssets("m1", []string{"a2"})
	require.NoError(t, err)
	require.Equal(t, 1, count)

	// Next recalculation round: the engine still produces a1+a2, but a2
	// should be removed via exclude replay.
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{draft}))

	members, err := store.GetMomentAssets("m1", false)
	require.NoError(t, err)
	require.Len(t, members, 1)
	require.Equal(t, "a1", members[0].AssetID)
}

// ── Editable moments: pin overrides exclude (the later-written edit on the same asset wins) ──

func TestMomentStore_PinOverridesExclude(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)
	insertMomentAsset(t, db, "a1")
	insertMomentAsset(t, db, "a2")

	draft := MomentDraft{
		Moment: Moment{ID: "m1", RecipeKey: "trip", Title: "Trip", AssetCount: 1},
		Assets: []MomentAsset{{AssetID: "a1", Score: 0.5}},
	}
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{draft}))

	// First exclude a2, then change one's mind and pin it instead — the
	// later-written edit (pin) should override the earlier one (exclude).
	_, err := store.ExcludeMomentAssets("m1", []string{"a2"})
	require.NoError(t, err)
	count, err := store.PinMomentAssets("m1", []string{"a2"})
	require.NoError(t, err)
	require.Equal(t, 2, count)

	pins, excludes, err := store.MomentEditsFor("m1")
	require.NoError(t, err)
	require.Equal(t, []string{"a2"}, pins)
	require.Empty(t, excludes, "pin should override the prior exclude record, not coexist with it")

	// After recalculation, a2 should remain as a member (pin takes effect,
	// rather than being removed by exclude).
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{draft}))
	members, err := store.GetMomentAssets("m1", false)
	require.NoError(t, err)
	ids := map[string]bool{}
	for _, m := range members {
		ids[m.AssetID] = true
	}
	require.True(t, ids["a2"])
}

// ── Editable moments: theme-kind moments are immune to time-window changes (TimeFrom/TimeTo always NULL) ──

func TestMomentStore_ThemeMomentTimeWindowImmuneToEditRecompute(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)
	insertMomentAssetAt(t, db, "a1", time.Date(2011, 5, 10, 0, 0, 0, 0, time.UTC))
	insertMomentAssetAt(t, db, "a2", time.Date(2011, 6, 1, 0, 0, 0, 0, time.UTC))

	// Theme-kind draft: TimeFrom/TimeTo stay zero-valued (unset), should
	// persist as NULL.
	draft := MomentDraft{
		Moment: Moment{ID: "m1", RecipeKey: "theme:pets", Title: "Pets", AssetCount: 1},
		Assets: []MomentAsset{{AssetID: "a1", Featured: true, Score: 0.9}},
	}
	require.NoError(t, store.SyncRecipeMoments("theme:pets", []MomentDraft{draft}))

	moments, err := store.ListMoments()
	require.NoError(t, err)
	require.Len(t, moments, 1)
	require.True(t, moments[0].TimeFrom.IsZero(), "a theme moment's initial time window should be zero-valued (NULL)")
	require.True(t, moments[0].TimeTo.IsZero())

	// Pin an asset with taken_at — if the time window were mistakenly
	// refreshed, it would be stretched to a non-zero value.
	_, err = store.PinMomentAssets("m1", []string{"a2"})
	require.NoError(t, err)
	moments, err = store.ListMoments()
	require.NoError(t, err)
	require.True(t, moments[0].TimeFrom.IsZero(), "after pin, a theme moment's time window should still stay NULL")
	require.True(t, moments[0].TimeTo.IsZero())

	// Exclude an existing member, should likewise be immune.
	_, err = store.ExcludeMomentAssets("m1", []string{"a1"})
	require.NoError(t, err)
	moments, err = store.ListMoments()
	require.NoError(t, err)
	require.True(t, moments[0].TimeFrom.IsZero(), "after exclude, a theme moment's time window should still stay NULL")
	require.True(t, moments[0].TimeTo.IsZero())

	// Trigger one more round of SyncRecipeMoments with edits replay
	// (hasEdits=true will enter refreshMomentDerived, need to confirm the
	// hadTimeWindow determination is still false).
	require.NoError(t, store.SyncRecipeMoments("theme:pets", []MomentDraft{draft}))
	moments, err = store.ListMoments()
	require.NoError(t, err)
	require.True(t, moments[0].TimeFrom.IsZero(), "after a recalculation with edits replay, a theme moment's time window should still stay NULL")
	require.True(t, moments[0].TimeTo.IsZero())
}

// ── Editable moments: derived refresh (count + time window + cover re-pick) ──

func TestMomentStore_DerivedRefreshOnEdit(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)
	tFrom := time.Date(2011, 5, 10, 0, 0, 0, 0, time.UTC)
	tMid := time.Date(2011, 5, 12, 0, 0, 0, 0, time.UTC)
	tLate := time.Date(2011, 5, 20, 0, 0, 0, 0, time.UTC) // a time point outside the window, pinning should widen the time window
	insertMomentAssetAt(t, db, "a1", tFrom)
	insertMomentAssetAt(t, db, "a2", tMid)
	insertMomentAssetAt(t, db, "a3", tLate)

	draft := MomentDraft{
		Moment: Moment{
			ID: "m1", RecipeKey: "trip", Title: "Trip", AssetCount: 2,
			TimeFrom: tFrom, TimeTo: tMid, CoverAssetID: "a1",
		},
		Assets: []MomentAsset{
			{AssetID: "a1", Featured: true, Score: 0.9},
			{AssetID: "a2", Featured: false, Score: 0.5},
		},
	}
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{draft}))

	// pin a3 (its time is outside the window) — count should become 3, the
	// time window's right end should extend to a3's taken_at.
	count, err := store.PinMomentAssets("m1", []string{"a3"})
	require.NoError(t, err)
	require.Equal(t, 3, count)

	moments, err := store.ListMoments()
	require.NoError(t, err)
	require.Len(t, moments, 1)
	require.Equal(t, 3, moments[0].AssetCount)
	require.True(t, moments[0].TimeTo.Equal(tLate), "pin should trigger the time window to be recalculated against the new member set")
	require.Equal(t, "a1", moments[0].CoverAssetID, "the cover is still a member, shouldn't be re-picked")

	// Exclude the current cover a1 — the cover should be re-picked as the
	// highest-scoring remaining featured member (there are no other featured
	// members here, so it should fall back to the "any member" tier: order
	// determined by score DESC, asset_id.
	count2, err := store.ExcludeMomentAssets("m1", []string{"a1"})
	require.NoError(t, err)
	require.Equal(t, 2, count2)

	moments2, err := store.ListMoments()
	require.NoError(t, err)
	require.Equal(t, 2, moments2[0].AssetCount)
	require.NotEqual(t, "a1", moments2[0].CoverAssetID, "the old cover has been removed, it shouldn't keep hanging on")
	require.Contains(t, []string{"a2", "a3"}, moments2[0].CoverAssetID)
	require.Equal(t, "a2", moments2[0].CoverAssetID, "with no featured candidate, falls back to any member, taking the first by score DESC (a2=0.5>a3=0)")
}

// ── Editable moments: clearing members allows count=0 (no error after excluding everyone, cover falls back to NULL) ──

func TestMomentStore_ExcludeAllMembersAllowsZeroCount(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)
	insertMomentAsset(t, db, "a1")

	draft := MomentDraft{
		Moment: Moment{ID: "m1", RecipeKey: "trip", Title: "Trip", AssetCount: 1, CoverAssetID: "a1"},
		Assets: []MomentAsset{{AssetID: "a1", Featured: true, Score: 0.9}},
	}
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{draft}))

	count, err := store.ExcludeMomentAssets("m1", []string{"a1"})
	require.NoError(t, err)
	require.Equal(t, 0, count, "clearing members should allow count=0, no error")

	moments, err := store.ListMoments()
	require.NoError(t, err)
	require.Equal(t, 0, moments[0].AssetCount)
	require.Equal(t, "", moments[0].CoverAssetID, "the cover should fall back to NULL/empty when there are no members")
}

// ── Editable moments: hidden tombstone — preserved across upsert + filtered by ListMoments ──

func TestMomentStore_HideMomentPersistsAndFiltersListMoments(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)
	insertMomentAsset(t, db, "a1")

	draft := MomentDraft{
		Moment: Moment{ID: "m1", RecipeKey: "trip", Title: "Trip", AssetCount: 1},
		Assets: []MomentAsset{{AssetID: "a1"}},
	}
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{draft}))
	require.NoError(t, store.HideMoment("m1"))

	// After hiding, ListMoments should filter out this moment.
	moments, err := store.ListMoments()
	require.NoError(t, err)
	require.Len(t, moments, 0)

	// Recalculation (same-id upsert) shouldn't reset hidden to 0 — hidden
	// isn't in the upsert column list, naturally preserved the same way as
	// named_by_llm.
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{draft}))
	var hidden int
	require.NoError(t, db.QueryRow(`SELECT hidden FROM moments WHERE id=?`, "m1").Scan(&hidden))
	require.Equal(t, 1, hidden, "recalculation shouldn't clear the hidden tombstone")

	moments2, err := store.ListMoments()
	require.NoError(t, err)
	require.Len(t, moments2, 0, "should still be filtered by ListMoments after recalculation")
}

// ── Editable moments: PinMomentAssets takes immediate effect, and unknown ids are ignored ──

func TestMomentStore_PinTakesEffectImmediatelyAndIgnoresUnknownID(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)
	insertMomentAsset(t, db, "a1")
	insertMomentAsset(t, db, "a2")

	draft := MomentDraft{
		Moment: Moment{ID: "m1", RecipeKey: "trip", Title: "Trip", AssetCount: 1},
		Assets: []MomentAsset{{AssetID: "a1", Score: 0.5}},
	}
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{draft}))

	// Mix in an id that doesn't exist in the assets table, should be
	// silently ignored, no error, no effect on known ids.
	count, err := store.PinMomentAssets("m1", []string{"a2", "ghost-asset"})
	require.NoError(t, err)
	require.Equal(t, 2, count, "unknown ids should be ignored, only a2 takes effect")

	// Takes immediate effect: without waiting for the next Sync round,
	// GetMomentAssets should see a2 right away.
	members, err := store.GetMomentAssets("m1", false)
	require.NoError(t, err)
	ids := map[string]bool{}
	for _, m := range members {
		ids[m.AssetID] = true
	}
	require.True(t, ids["a1"])
	require.True(t, ids["a2"])
	require.False(t, ids["ghost-asset"])

	// An unknown id also shouldn't leave a moment_edits record.
	pins, _, err := store.MomentEditsFor("m1")
	require.NoError(t, err)
	require.Equal(t, []string{"a2"}, pins)
}

// ── Editable moments: MomentEditsFor shape ──────────────────────────────────

func TestMomentStore_MomentEditsFor(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)
	insertMomentAsset(t, db, "a1")
	insertMomentAsset(t, db, "a2")
	insertMomentAsset(t, db, "a3")

	draft := MomentDraft{
		Moment: Moment{ID: "m1", RecipeKey: "trip", Title: "Trip", AssetCount: 1},
		Assets: []MomentAsset{{AssetID: "a1"}},
	}
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{draft}))

	_, err := store.PinMomentAssets("m1", []string{"a2"})
	require.NoError(t, err)
	_, err = store.ExcludeMomentAssets("m1", []string{"a3"})
	require.NoError(t, err)

	pins, excludes, err := store.MomentEditsFor("m1")
	require.NoError(t, err)
	require.Equal(t, []string{"a2"}, pins)
	require.Equal(t, []string{"a3"}, excludes)

	// A moment with no edit records at all should return empty slices, no error.
	pins2, excludes2, err := store.MomentEditsFor("no-such-moment")
	require.NoError(t, err)
	require.Empty(t, pins2)
	require.Empty(t, excludes2)
}

// ── Editable moments: TopFeaturedByMoment shape (non-cover, score order, <=N) ──

func TestMomentStore_TopFeaturedByMoment(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)
	for _, id := range []string{"a1", "a2", "a3", "a4", "b1", "b2"} {
		insertMomentAsset(t, db, id)
	}

	d1 := MomentDraft{
		Moment: Moment{ID: "m1", RecipeKey: "trip", Title: "Trip One", AssetCount: 4, CoverAssetID: "a1"},
		Assets: []MomentAsset{
			{AssetID: "a1", Featured: true, Score: 0.95}, // the cover, should be excluded
			{AssetID: "a2", Featured: true, Score: 0.9},
			{AssetID: "a3", Featured: true, Score: 0.8},
			{AssetID: "a4", Featured: false, Score: 0.99}, // not featured, shouldn't appear
		},
	}
	d2 := MomentDraft{
		Moment: Moment{ID: "m2", RecipeKey: "theme:pets", Title: "Pets", AssetCount: 2},
		Assets: []MomentAsset{
			{AssetID: "b1", Featured: true, Score: 0.7},
			{AssetID: "b2", Featured: true, Score: 0.6},
		},
	}
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{d1}))
	require.NoError(t, store.SyncRecipeMoments("theme:pets", []MomentDraft{d2}))

	top, err := store.TopFeaturedByMoment(1)
	require.NoError(t, err)
	require.Equal(t, []string{"a2"}, top["m1"], "cover a1 should be excluded, taking the highest-scoring one among the remaining featured")
	require.Equal(t, []string{"b1"}, top["m2"])

	top2, err := store.TopFeaturedByMoment(2)
	require.NoError(t, err)
	require.Equal(t, []string{"a2", "a3"}, top2["m1"], "by score DESC, the top 2 excluding the cover")
	require.Equal(t, []string{"b1", "b2"}, top2["m2"])
}

// ── CoverRatioByMoment: cover width/height ratio (normal / missing exif row / zero value -> 0) ──

// insertMomentAssetWithExif inserts an asset row with asset_exif
// (width/height), for CoverRatioByMoment tests to use.
func insertMomentAssetWithExif(t *testing.T, db *sql.DB, id string, width, height int) {
	t.Helper()
	insertMomentAsset(t, db, id)
	_, err := db.Exec(`INSERT INTO asset_exif(asset_id, width, height) VALUES (?, ?, ?)`, id, width, height)
	require.NoError(t, err)
}

func TestMomentStore_CoverRatioByMoment_Normal(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)
	insertMomentAssetWithExif(t, db, "a1", 1600, 2000) // portrait cover, ratio=0.8

	draft := MomentDraft{
		Moment: Moment{ID: "m1", RecipeKey: "trip", Title: "Trip", AssetCount: 1, CoverAssetID: "a1"},
		Assets: []MomentAsset{{AssetID: "a1", Score: 0.5}},
	}
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{draft}))

	ratios, err := store.CoverRatioByMoment()
	require.NoError(t, err)
	require.InDelta(t, 0.8, ratios["m1"], 1e-9)
}

func TestMomentStore_CoverRatioByMoment_MissingExifRowExcluded(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)
	insertMomentAsset(t, db, "a1") // no asset_exif row (dimensions weren't indexed)

	draft := MomentDraft{
		Moment: Moment{ID: "m1", RecipeKey: "trip", Title: "Trip", AssetCount: 1, CoverAssetID: "a1"},
		Assets: []MomentAsset{{AssetID: "a1", Score: 0.5}},
	}
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{draft}))

	ratios, err := store.CoverRatioByMoment()
	require.NoError(t, err)
	_, ok := ratios["m1"]
	require.False(t, ok, "a moment missing an asset_exif row shouldn't appear in the map, the caller should treat it as 0")
}

func TestMomentStore_CoverRatioByMoment_ZeroWidthOrHeightExcluded(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)
	insertMomentAssetWithExif(t, db, "a1", 0, 2000)    // width=0
	insertMomentAssetWithExif(t, db, "a2", 1600, 0)    // height=0
	insertMomentAssetWithExif(t, db, "a3", 1600, 1200) // normal, ratio=1.333...

	draft1 := MomentDraft{
		Moment: Moment{ID: "m1", RecipeKey: "trip", Title: "Trip1", AssetCount: 1, CoverAssetID: "a1"},
		Assets: []MomentAsset{{AssetID: "a1", Score: 0.5}},
	}
	draft2 := MomentDraft{
		Moment: Moment{ID: "m2", RecipeKey: "trip", Title: "Trip2", AssetCount: 1, CoverAssetID: "a2"},
		Assets: []MomentAsset{{AssetID: "a2", Score: 0.5}},
	}
	draft3 := MomentDraft{
		Moment: Moment{ID: "m3", RecipeKey: "trip", Title: "Trip3", AssetCount: 1, CoverAssetID: "a3"},
		Assets: []MomentAsset{{AssetID: "a3", Score: 0.5}},
	}
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{draft1, draft2, draft3}))

	ratios, err := store.CoverRatioByMoment()
	require.NoError(t, err)
	_, ok1 := ratios["m1"]
	require.False(t, ok1, "width=0 shouldn't appear in the map")
	_, ok2 := ratios["m2"]
	require.False(t, ok2, "height=0 shouldn't appear in the map")
	require.InDelta(t, 1600.0/1200.0, ratios["m3"], 1e-9)
}

// ── diff-style upsert: added_at semantics (spec 1.2) ────────────────────────

// momentAssetAddedAt reads back moment_assets.added_at (returns 0 when
// NULL, consistent with MomentAsset.AddedAt's 0=NULL convention), for this
// group of tests' assertions to use.
func momentAssetAddedAt(t *testing.T, db *sql.DB, momentID, assetID string) int64 {
	t.Helper()
	var v sql.NullInt64
	require.NoError(t, db.QueryRow(`SELECT added_at FROM moment_assets WHERE moment_id=? AND asset_id=?`,
		momentID, assetID).Scan(&v))
	if !v.Valid {
		return 0
	}
	return v.Int64
}

// TestMomentStore_SyncDiffUpsertNewMemberGetsAddedAtNow: a new member
// produced by the first Sync should be stamped with the current timestamp
// (non-NULL).
func TestMomentStore_SyncDiffUpsertNewMemberGetsAddedAtNow(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)
	insertMomentAsset(t, db, "a1")

	before := nowMs()
	draft := MomentDraft{
		Moment: Moment{ID: "m1", RecipeKey: "trip", Title: "Trip", AssetCount: 1},
		Assets: []MomentAsset{{AssetID: "a1", Score: 0.5}},
	}
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{draft}))
	after := nowMs()

	addedAt := momentAssetAddedAt(t, db, "m1", "a1")
	require.GreaterOrEqual(t, addedAt, before, "a new member's added_at should be stamped with the current timestamp")
	require.LessOrEqual(t, addedAt, after)
}

// TestMomentStore_SyncDiffUpsertExistingMemberAddedAtUnchanged: the same
// member across Sync rounds (the conflict branch) shouldn't touch the
// existing added_at, even if featured/score change.
func TestMomentStore_SyncDiffUpsertExistingMemberAddedAtUnchanged(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)
	insertMomentAsset(t, db, "a1")

	draft := MomentDraft{
		Moment: Moment{ID: "m1", RecipeKey: "trip", Title: "Trip", AssetCount: 1},
		Assets: []MomentAsset{{AssetID: "a1", Featured: false, Score: 0.5}},
	}
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{draft}))
	firstAddedAt := momentAssetAddedAt(t, db, "m1", "a1")
	require.NotZero(t, firstAddedAt)

	time.Sleep(2 * time.Millisecond)
	draft2 := draft
	draft2.Assets = []MomentAsset{{AssetID: "a1", Featured: true, Score: 0.9}}
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{draft2}))

	secondAddedAt := momentAssetAddedAt(t, db, "m1", "a1")
	require.Equal(t, firstAddedAt, secondAddedAt, "the conflict branch shouldn't touch an existing member's added_at")

	// featured/score should still update normally (diff upsert semantics are
	// equivalent to the original full replacement).
	members, err := store.GetMomentAssets("m1", false)
	require.NoError(t, err)
	require.Len(t, members, 1)
	require.True(t, members[0].Featured)
	require.Equal(t, 0.9, members[0].Score)
}

// TestMomentStore_SyncDiffUpsertDisappearedMemberDeleted: an old member
// (non-pinned) not produced by the engine this round should be deleted
// (diff upsert's step 2); if it reappears later, it's treated as a brand
// new member (added_at re-stamped with now).
func TestMomentStore_SyncDiffUpsertDisappearedMemberDeleted(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)
	insertMomentAsset(t, db, "a1")
	insertMomentAsset(t, db, "a2")

	draft := MomentDraft{
		Moment: Moment{ID: "m1", RecipeKey: "trip", Title: "Trip", AssetCount: 2},
		Assets: []MomentAsset{{AssetID: "a1", Score: 0.5}, {AssetID: "a2", Score: 0.4}},
	}
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{draft}))

	// Next round only produces a1, a2 disappears (not pinned) — should be
	// deleted, not merely "not updated".
	draft2 := draft
	draft2.Assets = []MomentAsset{{AssetID: "a1", Score: 0.5}}
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{draft2}))

	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM moment_assets WHERE moment_id='m1' AND asset_id='a2'`).Scan(&count))
	require.Equal(t, 0, count, "a disappeared non-pinned member should be deleted by diff upsert")
}

// TestMomentStore_SyncDiffUpsertPinExemptFromDeletionAddedAtStable: a pinned
// member shouldn't be deleted even when the engine doesn't produce it this
// round (the "false-freshness trap" called out in the spec); added_at
// should stay stable across two consecutive Sync rounds (not refreshed to
// now every round due to "deleted then re-inserted by replay").
func TestMomentStore_SyncDiffUpsertPinExemptFromDeletionAddedAtStable(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)
	insertMomentAsset(t, db, "a1")
	insertMomentAsset(t, db, "a2")

	draft := MomentDraft{
		Moment: Moment{ID: "m1", RecipeKey: "trip", Title: "Trip", AssetCount: 1},
		Assets: []MomentAsset{{AssetID: "a1", Score: 0.5}},
	}
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{draft}))

	// The user pins a2 (not included by the engine this round).
	_, err := store.PinMomentAssets("m1", []string{"a2"})
	require.NoError(t, err)
	pinnedAddedAt := momentAssetAddedAt(t, db, "m1", "a2")
	require.NotZero(t, pinnedAddedAt, "PinMomentAssets should stamp added_at=now")

	// Two consecutive recalculation rounds, the engine still only produces
	// a1, a2 should survive throughout with added_at unchanged.
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{draft}))
	afterRound1 := momentAssetAddedAt(t, db, "m1", "a2")
	require.Equal(t, pinnedAddedAt, afterRound1, "a pinned member is exempt from deletion, added_at shouldn't be refreshed after the first recalculation round")

	time.Sleep(2 * time.Millisecond)
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{draft}))
	afterRound2 := momentAssetAddedAt(t, db, "m1", "a2")
	require.Equal(t, pinnedAddedAt, afterRound2, "added_at still shouldn't be refreshed after the second recalculation round (false-freshness trap regression)")

	members, err := store.GetMomentAssets("m1", false)
	require.NoError(t, err)
	ids := map[string]bool{}
	for _, m := range members {
		ids[m.AssetID] = true
	}
	require.True(t, ids["a1"])
	require.True(t, ids["a2"], "a pinned member should survive after two recalculation rounds")
}

// ── PinMomentAssets' immediate-insertion path stamps added_at=now ──────────

func TestMomentStore_PinMomentAssetsSetsAddedAtNow(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)
	insertMomentAsset(t, db, "a1")
	insertMomentAsset(t, db, "a2")

	draft := MomentDraft{
		Moment: Moment{ID: "m1", RecipeKey: "trip", Title: "Trip", AssetCount: 1},
		Assets: []MomentAsset{{AssetID: "a1", Score: 0.5}},
	}
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{draft}))

	before := nowMs()
	_, err := store.PinMomentAssets("m1", []string{"a2"})
	require.NoError(t, err)
	after := nowMs()

	addedAt := momentAssetAddedAt(t, db, "m1", "a2")
	require.GreaterOrEqual(t, addedAt, before)
	require.LessOrEqual(t, addedAt, after)
}

// ── Debt cleanup: pin replay aligned to the "live asset" criterion ────────
//
// The live-asset criterion shares its source with moments_theme.go's
// loadThemeCandidatePool: status='indexed' AND deleted_at IS NULL AND
// offline=0. Previously, the three pin-related spots (immediate insertion /
// diff upsert deletion exemption / replay re-insertion) only checked that
// the row existed in the assets table, without recognizing the live-asset
// criterion, causing the divergence where "a pinned photo that went to
// trash still clings to the moment and won't leave".

// momentUpdatedAt reads back moments.updated_at, for the "fake refresh"
// regression test's assertions.
func momentUpdatedAt(t *testing.T, db *sql.DB, momentID string) int64 {
	t.Helper()
	var v int64
	require.NoError(t, db.QueryRow(`SELECT updated_at FROM moments WHERE id=?`, momentID).Scan(&v))
	return v
}

// TestMomentStore_PinMomentAssetsIgnoresDeadAssetImmediateInsert: calling
// PinMomentAssets directly on an asset already in trash (deleted_at
// non-NULL) — the id exists in the assets table, so the edits record
// should still be written (so it can automatically rejoin once restored
// from trash later), but the immediate member-insertion step should be
// blocked by the "live asset" criterion, and shouldn't immediately count
// the dead asset into membership/count.
func TestMomentStore_PinMomentAssetsIgnoresDeadAssetImmediateInsert(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)
	insertMomentAsset(t, db, "a1")
	insertMomentAsset(t, db, "a2")
	_, err := db.Exec(`UPDATE assets SET deleted_at=? WHERE id=?`, "2020-01-01 00:00:00", "a2")
	require.NoError(t, err)

	draft := MomentDraft{
		Moment: Moment{ID: "m1", RecipeKey: "trip", Title: "Trip", AssetCount: 1},
		Assets: []MomentAsset{{AssetID: "a1", Score: 0.5}},
	}
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{draft}))

	count, err := store.PinMomentAssets("m1", []string{"a2"})
	require.NoError(t, err)
	require.Equal(t, 1, count, "a trashed asset shouldn't be immediately counted into count")

	members, err := store.GetMomentAssets("m1", false)
	require.NoError(t, err)
	for _, m := range members {
		require.NotEqual(t, "a2", m.AssetID, "a trashed asset shouldn't be immediately inserted as a member")
	}

	pins, _, err := store.MomentEditsFor("m1")
	require.NoError(t, err)
	require.Contains(t, pins, "a2", "the edits record should be kept, so it rejoins automatically after restoration")
}

// TestMomentStore_PinReplayRemovesDeadAssetAndRejoinsOnRestore: after
// pinning a live asset survives a recalculation, the asset goes to trash
// (deleted_at set) — the next Sync round should no longer exempt its
// deletion, the member should be removed and count should drop
// correspondingly; after restoring from trash (deleted_at cleared) and
// Sync-ing again, it should automatically rejoin (the edits row is kept
// throughout, the pin intent isn't lost just because the asset died).
func TestMomentStore_PinReplayRemovesDeadAssetAndRejoinsOnRestore(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)
	insertMomentAsset(t, db, "a1")
	insertMomentAsset(t, db, "a2")

	draft := MomentDraft{
		Moment: Moment{ID: "m1", RecipeKey: "trip", Title: "Trip", AssetCount: 1},
		Assets: []MomentAsset{{AssetID: "a1", Score: 0.5}},
	}
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{draft}))

	// The user pins a2 (not included by the engine this round); a2 is still
	// a live asset at this point, so it takes effect immediately.
	count, err := store.PinMomentAssets("m1", []string{"a2"})
	require.NoError(t, err)
	require.Equal(t, 2, count)

	// a2 goes to trash.
	_, err = db.Exec(`UPDATE assets SET deleted_at=? WHERE id=?`, "2020-01-01 00:00:00", "a2")
	require.NoError(t, err)

	// Next recalculation round: a2 is no longer a live asset, pin no longer
	// exempts it from deletion, it should be removed by diff upsert.
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{draft}))
	members, err := store.GetMomentAssets("m1", false)
	require.NoError(t, err)
	ids := map[string]bool{}
	for _, m := range members {
		ids[m.AssetID] = true
	}
	require.True(t, ids["a1"])
	require.False(t, ids["a2"], "a pinned asset that went to trash should be removed on the next recalculation")

	moments, err := store.ListMoments()
	require.NoError(t, err)
	require.Equal(t, 1, moments[0].AssetCount, "count should drop correspondingly as the dead asset is removed")

	// The edits record should be kept throughout (not cleared), so it keeps
	// taking effect after restoration.
	pinsBeforeRestore, _, err := store.MomentEditsFor("m1")
	require.NoError(t, err)
	require.Contains(t, pinsBeforeRestore, "a2", "pin intent should be preserved while the asset is dead")

	// Restore from trash.
	_, err = db.Exec(`UPDATE assets SET deleted_at=NULL WHERE id=?`, "a2")
	require.NoError(t, err)

	// Next recalculation round: a2 is restored to a live asset, pin replay
	// should automatically re-insert it as a member.
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{draft}))
	members2, err := store.GetMomentAssets("m1", false)
	require.NoError(t, err)
	ids2 := map[string]bool{}
	for _, m := range members2 {
		ids2[m.AssetID] = true
	}
	require.True(t, ids2["a2"], "should automatically rejoin after being restored from trash")

	moments2, err := store.ListMoments()
	require.NoError(t, err)
	require.Equal(t, 2, moments2[0].AssetCount, "count should recover once it rejoins")
}

// ── Debt cleanup: fix for the all-unknown-ids fake refresh ────────────────
//
// Previously applyMomentEditOp always went through refreshMomentDerived +
// refreshed updated_at regardless of whether the call caused any member row
// changes, so a no-op like "passing all unknown ids" would still bump the
// moment to the front of ListMoments' ordering (called out in the round-8
// final review). Changed to tally the member rows actually affected this
// call (pin's INSERT-affected count / exclude's DELETE-affected count),
// skipping the derived-field refresh when it's 0, directly returning the
// current asset_count with updated_at left unchanged.

func TestMomentStore_EditOpAllUnknownIDsSkipsFakeRefresh(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)
	insertMomentAsset(t, db, "a1")
	insertMomentAsset(t, db, "b1")

	// Use two different recipe_keys to persist m1/m2 separately:
	// SyncRecipeMoments clears old moments not produced this round under the
	// same recipe_key, so reusing the same recipe_key would mistakenly
	// delete m1.
	draft1 := MomentDraft{
		Moment: Moment{ID: "m1", RecipeKey: "trip1", Title: "Trip1", AssetCount: 1},
		Assets: []MomentAsset{{AssetID: "a1", Score: 0.5}},
	}
	draft2 := MomentDraft{
		Moment: Moment{ID: "m2", RecipeKey: "trip2", Title: "Trip2", AssetCount: 1},
		Assets: []MomentAsset{{AssetID: "b1", Score: 0.5}},
	}
	require.NoError(t, store.SyncRecipeMoments("trip1", []MomentDraft{draft1}))
	time.Sleep(2 * time.Millisecond)
	require.NoError(t, store.SyncRecipeMoments("trip2", []MomentDraft{draft2}))

	// Neither is manually ordered (sort_order NULL), by updated_at DESC: m2
	// (more recently updated) comes first.
	moments, err := store.ListMoments()
	require.NoError(t, err)
	require.Equal(t, "m2", moments[0].ID)
	require.Equal(t, "m1", moments[1].ID)

	beforeUpdatedAt := momentUpdatedAt(t, db, "m1")

	// Call Pin on m1 with all-unknown ids: none exist in the assets table,
	// silently ignored, the member row count shouldn't change at all.
	count, err := store.PinMomentAssets("m1", []string{"ghost1", "ghost2"})
	require.NoError(t, err)
	require.Equal(t, 1, count, "all-unknown ids shouldn't change count")

	afterUpdatedAt := momentUpdatedAt(t, db, "m1")
	require.Equal(t, beforeUpdatedAt, afterUpdatedAt, "all-unknown ids shouldn't refresh updated_at (fake-refresh regression)")

	moments2, err := store.ListMoments()
	require.NoError(t, err)
	require.Equal(t, "m2", moments2[0].ID, "the sort position shouldn't change due to a fake refresh")
	require.Equal(t, "m1", moments2[1].ID)

	// ExcludeMomentAssets likewise: all-unknown ids shouldn't refresh
	// updated_at either.
	count2, err := store.ExcludeMomentAssets("m1", []string{"ghost3"})
	require.NoError(t, err)
	require.Equal(t, 1, count2)
	require.Equal(t, beforeUpdatedAt, momentUpdatedAt(t, db, "m1"), "exclude with all-unknown ids likewise shouldn't refresh updated_at")
}

// ── Debt cleanup: two cheap tests added ────────────────────────────────────

// TestMomentStore_SyncRecipeMomentsEmptyDraftAssetsDeletesAllNonPinMembers:
// when a draft's member slice is empty (the engine determines this moment
// now has no members at all this round), diff upsert should delete all
// non-pinned members (pinned members remain exempt) — covering the
// "empty-member draft" boundary called out in the round-9 final review,
// which previously had no explicit unit test.
func TestMomentStore_SyncRecipeMomentsEmptyDraftAssetsDeletesAllNonPinMembers(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)
	insertMomentAsset(t, db, "a1")
	insertMomentAsset(t, db, "a2")

	draft := MomentDraft{
		Moment: Moment{ID: "m1", RecipeKey: "trip", Title: "Trip", AssetCount: 2},
		Assets: []MomentAsset{{AssetID: "a1", Score: 0.5}, {AssetID: "a2", Score: 0.4}},
	}
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{draft}))

	// The user pins an asset outside this round's engine output.
	insertMomentAsset(t, db, "a3")
	_, err := store.PinMomentAssets("m1", []string{"a3"})
	require.NoError(t, err)

	// Next round: the draft's member slice is empty (the engine determines
	// this moment has no members).
	emptyDraft := draft
	emptyDraft.Assets = nil
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{emptyDraft}))

	members, err := store.GetMomentAssets("m1", false)
	require.NoError(t, err)
	ids := map[string]bool{}
	for _, m := range members {
		ids[m.AssetID] = true
	}
	require.False(t, ids["a1"], "an empty draft should delete old non-pinned members")
	require.False(t, ids["a2"], "an empty draft should delete old non-pinned members")
	require.True(t, ids["a3"], "a pinned member should be exempt from deletion even when the draft's members are empty")
	require.Len(t, members, 1)
}

// ── AddedThisWeekByMoment: criteria (NULL not counted / 7-day window / no-N+1 shape) ──

func TestMomentStore_AddedThisWeekByMoment_NullNotCountedAndSevenDayWindow(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)
	insertMomentAsset(t, db, "a1") // legacy: added_at manually set to NULL
	insertMomentAsset(t, db, "a2") // added this week
	insertMomentAsset(t, db, "a3") // added 8 days ago, outside the window
	insertMomentAsset(t, db, "a4") // exactly within the window boundary (right after now-7d)

	draft := MomentDraft{
		Moment: Moment{ID: "m1", RecipeKey: "trip", Title: "Trip", AssetCount: 4},
		Assets: []MomentAsset{
			{AssetID: "a1"}, {AssetID: "a2"}, {AssetID: "a3"}, {AssetID: "a4"},
		},
	}
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{draft}))

	now := nowMs()
	sevenDaysMs := int64(7 * 24 * 60 * 60 * 1000)

	// a1 simulates legacy data: backfilled to NULL (diff upsert's first
	// INSERT stamped it with now, here we manually overwrite it to simulate
	// the scenario of "existed before the upgrade, join time unknown").
	_, err := db.Exec(`UPDATE moment_assets SET added_at=NULL WHERE moment_id='m1' AND asset_id='a1'`)
	require.NoError(t, err)
	// a2: within the window (2 days ago).
	_, err = db.Exec(`UPDATE moment_assets SET added_at=? WHERE moment_id='m1' AND asset_id='a2'`,
		now-2*24*60*60*1000)
	require.NoError(t, err)
	// a3: outside the window (8 days ago).
	_, err = db.Exec(`UPDATE moment_assets SET added_at=? WHERE moment_id='m1' AND asset_id='a3'`,
		now-8*24*60*60*1000)
	require.NoError(t, err)
	// a4: exactly equal to the window boundary (now-7d), the boundary should be counted (>=).
	_, err = db.Exec(`UPDATE moment_assets SET added_at=? WHERE moment_id='m1' AND asset_id='a4'`,
		now-sevenDaysMs)
	require.NoError(t, err)

	counts, err := store.AddedThisWeekByMoment(now)
	require.NoError(t, err)
	require.Equal(t, 2, counts["m1"], "only a2/a4 fall within the 7-day window and are non-NULL; a1 (NULL) and a3 (outside the window) aren't counted")
}

// TestMomentStore_AddedThisWeekByMoment_MultiMomentGrouping covers multiple
// moments with a single query (verifying the no-N+1 shape: the same call
// should correctly group results into their respective moment_id).
func TestMomentStore_AddedThisWeekByMoment_MultiMomentGrouping(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)
	insertMomentAsset(t, db, "a1")
	insertMomentAsset(t, db, "a2")
	insertMomentAsset(t, db, "b1")

	d1 := MomentDraft{Moment: Moment{ID: "m1", RecipeKey: "trip", Title: "One", AssetCount: 2},
		Assets: []MomentAsset{{AssetID: "a1"}, {AssetID: "a2"}}}
	d2 := MomentDraft{Moment: Moment{ID: "m2", RecipeKey: "theme:pets", Title: "Two", AssetCount: 1},
		Assets: []MomentAsset{{AssetID: "b1"}}}
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{d1}))
	require.NoError(t, store.SyncRecipeMoments("theme:pets", []MomentDraft{d2}))

	now := nowMs()
	counts, err := store.AddedThisWeekByMoment(now)
	require.NoError(t, err)
	require.Equal(t, 2, counts["m1"], "m1 has two members added this week: a1/a2")
	require.Equal(t, 1, counts["m2"])
}

// ── PlacesByMoment: ordering/tie-break/limit/no-geo fallback ───────────────

// insertMomentAssetWithGeo inserts an asset row with asset_geo, for
// PlacesByMoment tests to use.
func insertMomentAssetWithGeo(t *testing.T, db *sql.DB, id, city string) {
	t.Helper()
	insertMomentAsset(t, db, id)
	_, err := db.Exec(`INSERT INTO asset_geo(asset_id, city) VALUES (?, ?)`, id, city)
	require.NoError(t, err)
}

func TestMomentStore_PlacesByMoment_OrderAndTieBreak(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)
	// Bozeman x3, Rexburg x1, Twin Falls x1 (ties broken by city ASC).
	insertMomentAssetWithGeo(t, db, "a1", "Bozeman")
	insertMomentAssetWithGeo(t, db, "a2", "Bozeman")
	insertMomentAssetWithGeo(t, db, "a3", "Bozeman")
	insertMomentAssetWithGeo(t, db, "a4", "Twin Falls")
	insertMomentAssetWithGeo(t, db, "a5", "Rexburg")

	draft := MomentDraft{
		Moment: Moment{ID: "m1", RecipeKey: "trip", Title: "Trip", AssetCount: 5},
		Assets: []MomentAsset{{AssetID: "a1"}, {AssetID: "a2"}, {AssetID: "a3"}, {AssetID: "a4"}, {AssetID: "a5"}},
	}
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{draft}))

	places, err := store.PlacesByMoment("m1", 8)
	require.NoError(t, err)
	require.Equal(t, []MomentPlace{
		{Name: "Bozeman", Count: 3},
		{Name: "Rexburg", Count: 1},
		{Name: "Twin Falls", Count: 1},
	}, places, "by count DESC, tie-broken by city ASC for equal counts")
}

func TestMomentStore_PlacesByMoment_LimitCaps(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)
	cities := []string{"A", "B", "C", "D", "E", "F", "G", "H", "I", "J"}
	assets := make([]MomentAsset, 0, len(cities))
	for i, city := range cities {
		id := fmt.Sprintf("a%d", i)
		insertMomentAssetWithGeo(t, db, id, city)
		assets = append(assets, MomentAsset{AssetID: id})
	}
	draft := MomentDraft{
		Moment: Moment{ID: "m1", RecipeKey: "trip", Title: "Trip", AssetCount: len(cities)},
		Assets: assets,
	}
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{draft}))

	places, err := store.PlacesByMoment("m1", 8)
	require.NoError(t, err)
	require.Len(t, places, 8, "10 different cities, the limit should cap it to 8 rows")
}

func TestMomentStore_PlacesByMoment_NoGeoExcludedAndEmptyReturnsEmpty(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)
	insertMomentAsset(t, db, "a1") // no asset_geo row
	_, err := db.Exec(`INSERT INTO asset_geo(asset_id, city) VALUES (?, ?)`, "a1", "")
	require.NoError(t, err)        // an empty city string likewise shouldn't be counted
	insertMomentAsset(t, db, "a2") // no asset_geo row at all (not geocoded)

	draft := MomentDraft{
		Moment: Moment{ID: "m1", RecipeKey: "trip", Title: "Trip", AssetCount: 2},
		Assets: []MomentAsset{{AssetID: "a1"}, {AssetID: "a2"}},
	}
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{draft}))

	places, err := store.PlacesByMoment("m1", 8)
	require.NoError(t, err)
	require.Empty(t, places, "members with an empty city or no geo row shouldn't be counted in places")
}
