// Tests for pet entity mining + the entity moment engine: covers the Step 1
// brief checklist — beagle with 14 photos across 2 months qualifies and
// produces an entity, labrador with 1 photo doesn't qualify, the phrase word
// "boxer dog" matches while bare "boxer" text doesn't; draft title/subtitle
// formatting (same-year/cross-year)/member union (word hits ∪ CLIP fake); no
// qualifying entity clears the profile. Test cases for the replacement rule
// (theme:pets, both directions) are added in moments_test.go (which needs
// the full RecomputeAll assembly).
//
// Pet entity mining consumes pin/exclude feedback (Task 3): exclude narrows
// first/last seen, exclude dropping below the min_photos threshold makes the
// entity disappear from the mining output, pin merged into the matched set
// increases the count/extends the span, and an asset hit by pin but not in
// the candidate pool (e.g. already trashed/offline) doesn't count toward the
// stats. See test cases under "── MinePetEntities: consuming pin/exclude
// feedback".
package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// petTestRecipe assembles a kind=pet_entities test recipe, reusing the
// insertThemeAsset/insertCaption helper functions and fakeThemeSearcher
// defined in moments_theme_test.go.
func petTestRecipe(lexicon []string, minPhotos, minMonths int) MomentRecipe {
	params := RecipeParams{
		Lexicon:      lexicon,
		MinPhotos:    minPhotos,
		MinMonths:    minMonths,
		ClipMinScore: 0.45,
		ClipTopK:     100,
	}
	b, _ := json.Marshal(params)
	return MomentRecipe{Key: "profile:pets", Kind: "pet_entities", Title: "Pet Entities", ParamsJSON: string(b)}
}

// ── MinePetEntities: qualification threshold + phrase word boundary ────────

func TestMinePetEntities_ThresholdsAndPhraseBoundary(t *testing.T) {
	db := makeTestDB(t)

	month1 := time.Date(2011, time.August, 1, 12, 0, 0, 0, time.UTC)
	month2 := time.Date(2026, time.July, 1, 12, 0, 0, 0, time.UTC)

	// beagle: 14 photos across 2 months, should qualify (default threshold 8
	// photos/2 months, explicitly passed as 8/2 here).
	for i := 0; i < 7; i++ {
		id := "beagle-a" + string(rune('0'+i))
		insertThemeAsset(t, db, id, month1.AddDate(0, 0, i))
		insertCaption(t, db, id, "our beagle running in the yard")
	}
	for i := 0; i < 7; i++ {
		id := "beagle-b" + string(rune('0'+i))
		insertThemeAsset(t, db, id, month2.AddDate(0, 0, i))
		insertCaption(t, db, id, "the beagle sleeping on the couch")
	}

	// labrador: only 1 photo, doesn't qualify.
	insertThemeAsset(t, db, "lab1", month1)
	insertCaption(t, db, "lab1", "a labrador retrieving a ball")

	// "boxer dog": 8 photos across 2 months matched by the phrase (qualifies),
	// plus 1 photo with bare "boxer" (no "dog" in the caption) that should not
	// count toward this entity's photo count — word-boundary matching is
	// against the whole phrase, not a single word inside it.
	for i := 0; i < 4; i++ {
		id := "boxer-a" + string(rune('0'+i))
		insertThemeAsset(t, db, id, month1.AddDate(0, 0, i))
		insertCaption(t, db, id, "a boxer dog resting on the porch")
	}
	for i := 0; i < 4; i++ {
		id := "boxer-b" + string(rune('0'+i))
		insertThemeAsset(t, db, id, month2.AddDate(0, 0, i))
		insertCaption(t, db, id, "a boxer dog playing fetch")
	}
	insertThemeAsset(t, db, "boxer-bare", month1)
	insertCaption(t, db, "boxer-bare", "the boxer stared at me from across the ring")

	recipe := petTestRecipe([]string{"beagle", "labrador", "boxer dog"}, 8, 2)

	entities, err := MinePetEntities(context.Background(), db, NewMomentStore(db), recipe)
	require.NoError(t, err)

	byKey := map[string]ProfileEntity{}
	for _, e := range entities {
		byKey[e.Key] = e
	}

	require.Contains(t, byKey, "beagle")
	require.Equal(t, "pet", byKey["beagle"].Kind)
	require.Equal(t, 14, byKey["beagle"].PhotoCount)
	require.Equal(t, "Beagle", byKey["beagle"].Label)
	require.True(t, byKey["beagle"].FirstSeen.Equal(month1))
	require.True(t, byKey["beagle"].LastSeen.Equal(month2.AddDate(0, 0, 6)))

	require.NotContains(t, byKey, "labrador", "1 photo isn't enough to qualify (min_photos=8)")

	require.Contains(t, byKey, "boxer dog")
	require.Equal(t, 8, byKey["boxer dog"].PhotoCount, "bare 'boxer' (no 'dog') should not count toward the phrase entity's photo count")
	require.Equal(t, "Boxer Dog", byKey["boxer dog"].Label)

	var ev petEvidence
	require.NoError(t, json.Unmarshal([]byte(byKey["beagle"].EvidenceJSON), &ev))
	require.Equal(t, 14, ev.PhotoCount)
	require.Equal(t, 2, ev.Months)
	require.NotEmpty(t, ev.First)
	require.NotEmpty(t, ev.Last)
}

func TestMinePetEntities_EmptyLexiconReturnsEmpty(t *testing.T) {
	db := makeTestDB(t)
	recipe := petTestRecipe(nil, 8, 2)
	entities, err := MinePetEntities(context.Background(), db, NewMomentStore(db), recipe)
	require.NoError(t, err)
	require.Empty(t, entities)
}

// ── MinePetEntities: consuming pin/exclude feedback ─────────────────────────

// seedStubMoment inserts a bare-minimum moments row directly, purely to
// satisfy moment_edits's foreign key constraint on moments(id) — the test
// calls store.Pin/ExcludeMomentAssets directly to write edit records, without
// going through the full BuildPetEntityMoments/SyncRecipeMoments assembly
// flow.
func seedStubMoment(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	now := time.Now().UnixMilli()
	_, err := db.Exec(`
		INSERT INTO moments(id, recipe_key, title, asset_count, created_at, updated_at)
		VALUES (?, 'profile:pets', 'stub', 0, ?, ?)`, id, now, now)
	require.NoError(t, err)
}

func TestMinePetEntities_ExcludeNarrowsFirstLastSeenAndCount(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)

	month1 := time.Date(2011, time.August, 1, 12, 0, 0, 0, time.UTC)
	month2 := time.Date(2026, time.July, 1, 12, 0, 0, 0, time.UTC)

	for i := 0; i < 7; i++ {
		id := "beagle-a" + string(rune('0'+i))
		insertThemeAsset(t, db, id, month1.AddDate(0, 0, i))
		insertCaption(t, db, id, "our beagle running in the yard")
	}
	for i := 0; i < 7; i++ {
		id := "beagle-b" + string(rune('0'+i))
		insertThemeAsset(t, db, id, month2.AddDate(0, 0, i))
		insertCaption(t, db, id, "the beagle sleeping on the couch")
	}

	recipe := petTestRecipe([]string{"beagle"}, 8, 2)
	momentID := ProfileEntityID("pet", "beagle")
	seedStubMoment(t, db, momentID)

	// The user decides the earliest (beagle-a0) and latest (beagle-b6) photos
	// aren't their own dog, and excludes them.
	_, err := store.ExcludeMomentAssets(momentID, []string{"beagle-a0", "beagle-b6"})
	require.NoError(t, err)

	entities, err := MinePetEntities(context.Background(), db, store, recipe)
	require.NoError(t, err)
	require.Len(t, entities, 1)

	e := entities[0]
	require.Equal(t, 12, e.PhotoCount, "14 photos minus 2 excluded should be 12")
	require.True(t, e.FirstSeen.Equal(month1.AddDate(0, 0, 1)), "first-seen should narrow to beagle-a1")
	require.True(t, e.LastSeen.Equal(month2.AddDate(0, 0, 5)), "last-seen should narrow to beagle-b5")
}

func TestMinePetEntities_ExcludeBelowMinPhotosEntityDisappears(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)

	base := time.Date(2020, time.March, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 8; i++ {
		id := "lab-" + string(rune('a'+i))
		insertThemeAsset(t, db, id, base.AddDate(0, 0, i))
		insertCaption(t, db, id, "a labrador at the beach")
	}

	recipe := petTestRecipe([]string{"labrador"}, 8, 1)
	momentID := ProfileEntityID("pet", "labrador")
	seedStubMoment(t, db, momentID)

	// Exactly at the qualification threshold (8 photos); excluding 1 drops
	// below min_photos=8, and the entity should disappear from the mining
	// output — this is deliberate semantics: insufficient recurrence evidence
	// doesn't support the judgment "this is my own pet".
	_, err := store.ExcludeMomentAssets(momentID, []string{"lab-a"})
	require.NoError(t, err)

	entities, err := MinePetEntities(context.Background(), db, store, recipe)
	require.NoError(t, err)
	require.Empty(t, entities, "once below the min_photos threshold, the entity should no longer be mined out")
}

func TestMinePetEntities_PinMergesAdditionalAssetIntoCountAndSpan(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)

	month1 := time.Date(2011, time.August, 1, 12, 0, 0, 0, time.UTC)
	month2 := time.Date(2026, time.July, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 7; i++ {
		id := "beagle-a" + string(rune('0'+i))
		insertThemeAsset(t, db, id, month1.AddDate(0, 0, i))
		insertCaption(t, db, id, "our beagle running in the yard")
	}
	for i := 0; i < 7; i++ {
		id := "beagle-b" + string(rune('0'+i))
		insertThemeAsset(t, db, id, month2.AddDate(0, 0, i))
		insertCaption(t, db, id, "the beagle sleeping on the couch")
	}

	// A photo whose caption doesn't mention "beagle" at all, taken earlier than
	// the current first-seen, to verify that merging in a pin increases the
	// count and extends the span backward.
	earlier := month1.AddDate(0, 0, -30)
	insertThemeAsset(t, db, "beagle-pinned", earlier)
	insertCaption(t, db, "beagle-pinned", "a lazy afternoon nap")

	recipe := petTestRecipe([]string{"beagle"}, 8, 2)
	momentID := ProfileEntityID("pet", "beagle")
	seedStubMoment(t, db, momentID)

	_, err := store.PinMomentAssets(momentID, []string{"beagle-pinned"})
	require.NoError(t, err)

	entities, err := MinePetEntities(context.Background(), db, store, recipe)
	require.NoError(t, err)
	require.Len(t, entities, 1)

	e := entities[0]
	require.Equal(t, 15, e.PhotoCount, "14 word-hit photos + 1 pin-confirmed sample")
	require.True(t, e.FirstSeen.Equal(earlier), "the earlier pinned sample should extend first-seen")
	require.True(t, e.LastSeen.Equal(month2.AddDate(0, 0, 6)), "last-seen is unaffected")
}

func TestMinePetEntities_PinOutsideCandidatePoolNotCounted(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)

	base := time.Date(2020, time.March, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 8; i++ {
		id := "beagle-" + string(rune('a'+i))
		insertThemeAsset(t, db, id, base.AddDate(0, 0, i))
		insertCaption(t, db, id, "our beagle at the park")
	}

	// An asset already in the trash: still present in the assets table
	// (satisfying moment_edits's foreign key), but not in the
	// loadThemeCandidatePool candidate pool — pinning it should not count it
	// toward the stats.
	_, err := db.Exec(`INSERT INTO assets(id, file_path, status, taken_at, deleted_at) VALUES (?,?,'indexed',?,?)`,
		"beagle-trashed", "/g/beagle-trashed.jpg",
		base.AddDate(0, 0, -10).UTC().Format("2006-01-02 15:04:05"),
		time.Now().UTC().Format("2006-01-02 15:04:05"))
	require.NoError(t, err)

	recipe := petTestRecipe([]string{"beagle"}, 8, 1)
	momentID := ProfileEntityID("pet", "beagle")
	seedStubMoment(t, db, momentID)

	_, err = store.PinMomentAssets(momentID, []string{"beagle-trashed"})
	require.NoError(t, err)

	entities, err := MinePetEntities(context.Background(), db, store, recipe)
	require.NoError(t, err)
	require.Len(t, entities, 1)
	require.Equal(t, 8, entities[0].PhotoCount, "pinning a trashed asset should not count toward the stats")
}

// ── petEntitySubtitle: year-span formatting ─────────────────────────────────

func TestPetEntitySubtitle(t *testing.T) {
	sameYear := petEntitySubtitle(
		time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2020, 12, 31, 0, 0, 0, 0, time.UTC))
	require.Equal(t, "2020", sameYear, "same year should write just one year")

	crossYear := petEntitySubtitle(
		time.Date(2011, 8, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC))
	require.Equal(t, "2011 – 2026", crossYear, "cross-year uses an en dash with a space on each side")
}

// ── BuildPetEntityMoments: draft assembly + member union + persisting the profile table ──

func TestBuildPetEntityMoments_DraftsAndMemberUnion(t *testing.T) {
	db := makeTestDB(t)
	profileStore := NewProfileStore(db)

	base := time.Date(2020, time.March, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 8; i++ {
		id := "beagle-" + string(rune('a'+i))
		insertThemeAsset(t, db, id, base.AddDate(0, 0, i))
		insertCaption(t, db, id, "our beagle at the park")
	}
	// Matched by CLIP only, no "beagle" in the caption, to verify the union
	// (not the intersection).
	insertThemeAsset(t, db, "beagle-clip-only", base.AddDate(0, 0, 30))
	insertCaption(t, db, "beagle-clip-only", "a lazy afternoon nap")

	recipe := petTestRecipe([]string{"beagle"}, 8, 1)

	searcher := fakeThemeSearcher{hits: map[string][]AssetScore{
		"a photo of a beagle": {
			{AssetID: "beagle-clip-only", Score: 0.9},
			{AssetID: "beagle-a", Score: 0.5}, // A word-hit asset also matched by CLIP: take the higher score.
		},
	}}

	drafts, err := BuildPetEntityMoments(context.Background(), db, searcher, profileStore, NewMomentStore(db), recipe)
	require.NoError(t, err)
	require.Len(t, drafts, 1)

	d := drafts[0]
	require.Equal(t, ProfileEntityID("pet", "beagle"), d.ID)
	require.Equal(t, "profile:pets", d.RecipeKey)
	require.Equal(t, "Your Beagle", d.Title)
	require.Equal(t, "2020", d.Subtitle, "same year should write just one year")
	require.Len(t, d.Assets, 9, "8 word-hit photos ∪ 1 CLIP-only photo")

	byID := map[string]MomentAsset{}
	for _, a := range d.Assets {
		byID[a.AssetID] = a
	}
	require.Contains(t, byID, "beagle-clip-only")
	require.InDelta(t, 0.9, byID["beagle-clip-only"].Score, 1e-9)
	require.InDelta(t, 0.5, byID["beagle-a"].Score, 1e-9, "word hit + CLIP hit takes the higher score")
	require.InDelta(t, 0.45, byID["beagle-b"].Score, 1e-9, "word-hit only should get the ClipMinScore floor score")

	saved, err := profileStore.ListEntities("pet")
	require.NoError(t, err)
	require.Len(t, saved, 1)
	require.Equal(t, "beagle", saved[0].Key)
}

func TestBuildPetEntityMoments_NoQualifyingClearsProfile(t *testing.T) {
	db := makeTestDB(t)
	profileStore := NewProfileStore(db)

	// Stale profile: simulates a previous round that qualified (now the lexicon
	// has changed / the pet no longer recurs, so it doesn't qualify this round).
	require.NoError(t, profileStore.ReplaceEntities("pet", []ProfileEntity{
		{ID: ProfileEntityID("pet", "husky"), Kind: "pet", Key: "husky", Label: "Husky", PhotoCount: 20},
	}))

	insertThemeAsset(t, db, "lab1", time.Now())
	insertCaption(t, db, "lab1", "a labrador at the beach")

	recipe := petTestRecipe([]string{"labrador"}, 8, 2)

	drafts, err := BuildPetEntityMoments(context.Background(), db, fakeThemeSearcher{}, profileStore, NewMomentStore(db), recipe)
	require.NoError(t, err)
	require.Empty(t, drafts)

	saved, err := profileStore.ListEntities("pet")
	require.NoError(t, err)
	require.Empty(t, saved, "no qualifying entity should clear the previous round's profile, leaving no stale data")
}
