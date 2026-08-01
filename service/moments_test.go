// Tests for MomentsService: covers the Step 1 brief checklist — recompute
// wires up both the trip/theme kinds, successful LLM naming overwrites title,
// namer failure doesn't stop RecomputeAll from returning nil, CAS
// re-entrancy returns immediately. Uses a real MomentStore (makeTestDB) +
// fakeThemeSearcher (already defined in moments_theme_test.go, reused in the
// same package) + a fake namer, no real ML/AI involved.
package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// noVecLoader is a test double for clipVecLoader: always returns "no
// vector", so PickFeaturedAndCover skips burst dedup and goes straight to the
// candidate pool by aesthetic_score (also unscored in these tests, so they
// all tie) — doesn't affect the "wiring works" semantics this file's tests
// care about.
func noVecLoader(_ string) ([]float32, bool) { return nil, false }

// fakeNamer is a test double for the namer interface: returns a fixed title,
// or a fixed error (simulating LLM timeout/AI not deployed), and records the
// call count for assertions.
type fakeNamer struct {
	title string
	err   error
	calls int
}

func (f *fakeNamer) Complete(_ context.Context, _ string) (string, error) {
	f.calls++
	if f.err != nil {
		return "", f.err
	}
	return f.title, nil
}

func TestRecomputeAll_TripAndThemeKinds(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)

	// trip recipe: 10 photos with GPS, enough to form a group.
	require.NoError(t, store.UpsertRecipes([]MomentRecipe{
		{Key: "trip", Kind: "trip", Title: "Trip", Enabled: true, ParamsJSON: `{"min_assets":3}`},
		{Key: "theme:pets", Kind: "theme", Title: "Pet Moments", Enabled: true,
			ParamsJSON: `{"min_assets":2,"clip_prompts":["a photo of a dog"],"caption_keywords":["dog"]}`},
	}))

	base := time.Date(2011, time.January, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		id := "trip-" + string(rune('a'+i))
		takenAt := base.AddDate(0, 0, i)
		_, err := db.Exec(`INSERT INTO assets(id, file_path, status, taken_at) VALUES(?,?,'indexed',?)`,
			id, "/g/"+id+".jpg", takenAt.UTC().Format("2006-01-02 15:04:05"))
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO asset_geo(asset_id, city, country) VALUES(?,?,?)`, id, "Kyoto", "JP")
		require.NoError(t, err)
	}

	// theme recipe: 2 photos matched by CLIP.
	searcher := fakeThemeSearcher{hits: map[string][]AssetScore{
		"a photo of a dog": {{AssetID: "theme-a", Score: 0.9}, {AssetID: "theme-b", Score: 0.8}},
	}}
	for i, id := range []string{"theme-a", "theme-b"} {
		takenAt := base.AddDate(0, 0, 20+i)
		_, err := db.Exec(`INSERT INTO assets(id, file_path, status, taken_at) VALUES(?,?,'indexed',?)`,
			id, "/g/"+id+".jpg", takenAt.UTC().Format("2006-01-02 15:04:05"))
		require.NoError(t, err)
	}

	namer := &fakeNamer{title: "Kyoto Trip"}
	svc := NewMomentsService(db, store, searcher, noVecLoader, namer)

	err := svc.RecomputeAll(context.Background())
	require.NoError(t, err)

	moments, err := store.ListMoments()
	require.NoError(t, err)
	require.Len(t, moments, 2, "trip + theme should each produce one moment")

	var kinds []string
	for _, m := range moments {
		kinds = append(kinds, m.RecipeKey)
	}
	require.ElementsMatch(t, []string{"trip", "theme:pets"}, kinds)
}

func TestRecomputeAll_LLMSuccessOverwritesTitle(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)
	require.NoError(t, store.UpsertRecipes([]MomentRecipe{
		{Key: "trip", Kind: "trip", Title: "Trip", Enabled: true, ParamsJSON: `{"min_assets":2}`},
	}))

	base := time.Date(2012, time.March, 1, 9, 0, 0, 0, time.UTC)
	for i := 0; i < 2; i++ {
		id := "a" + string(rune('0'+i))
		takenAt := base.AddDate(0, 0, i)
		_, err := db.Exec(`INSERT INTO assets(id, file_path, status, taken_at) VALUES(?,?,'indexed',?)`,
			id, "/g/"+id+".jpg", takenAt.UTC().Format("2006-01-02 15:04:05"))
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO asset_geo(asset_id, city, country) VALUES(?,?,?)`, id, "Osaka", "JP")
		require.NoError(t, err)
	}

	namer := &fakeNamer{title: "Cozy Spring Days"}
	svc := NewMomentsService(db, store, fakeThemeSearcher{}, noVecLoader, namer)

	require.NoError(t, svc.RecomputeAll(context.Background()))

	moments, err := store.ListMoments()
	require.NoError(t, err)
	require.Len(t, moments, 1)
	require.True(t, moments[0].NamedByLLM, "successful LLM naming should set named_by_llm=1")
	require.Equal(t, "Cozy Spring Days", moments[0].Title)
	require.Equal(t, 1, namer.calls)

	// Second recompute round: a title with named_by_llm=1 must not be overwritten
	// by the template result, nor should the LLM be called again.
	require.NoError(t, svc.RecomputeAll(context.Background()))
	moments2, err := store.ListMoments()
	require.NoError(t, err)
	require.Equal(t, "Cozy Spring Days", moments2[0].Title, "a moment already named by LLM should keep its title unchanged")
	require.Equal(t, 1, namer.calls, "an already-named moment should not call the LLM again")
}

func TestRecomputeAll_NamerFailureDoesNotBlock(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)
	require.NoError(t, store.UpsertRecipes([]MomentRecipe{
		{Key: "trip", Kind: "trip", Title: "Trip", Enabled: true, ParamsJSON: `{"min_assets":2}`},
	}))

	base := time.Date(2013, time.June, 1, 9, 0, 0, 0, time.UTC)
	for i := 0; i < 2; i++ {
		id := "b" + string(rune('0'+i))
		takenAt := base.AddDate(0, 0, i)
		_, err := db.Exec(`INSERT INTO assets(id, file_path, status, taken_at) VALUES(?,?,'indexed',?)`,
			id, "/g/"+id+".jpg", takenAt.UTC().Format("2006-01-02 15:04:05"))
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO asset_geo(asset_id, city, country) VALUES(?,?,?)`, id, "Nara", "JP")
		require.NoError(t, err)
	}

	namer := &fakeNamer{err: errors.New("ai service unavailable")}
	svc := NewMomentsService(db, store, fakeThemeSearcher{}, noVecLoader, namer)

	err := svc.RecomputeAll(context.Background())
	require.NoError(t, err, "LLM failure must be best-effort and never block recompute")

	moments, err := store.ListMoments()
	require.NoError(t, err)
	require.Len(t, moments, 1)
	require.False(t, moments[0].NamedByLLM, "naming failure should not set named_by_llm")
	require.Equal(t, "Nara Trip", moments[0].Title, "naming failure should keep the template fallback title")
}

// TestRecomputeAll_HiddenMomentSkippedInNamingLoop: the naming loop's
// candidate source is store.ListMoments(), which already filters by hidden=0
// (momentstore.go), so hidden moments are naturally never fed to the LLM —
// this adds an assertion test pinning down that behavior, to catch it if
// someone later changes the candidate source (e.g. to query the moments table
// directly) and silently drops the hidden filter.
func TestRecomputeAll_HiddenMomentSkippedInNamingLoop(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)
	require.NoError(t, store.UpsertRecipes([]MomentRecipe{
		{Key: "trip", Kind: "trip", Title: "Trip", Enabled: true, ParamsJSON: `{"min_assets":2}`},
	}))

	base := time.Date(2013, time.June, 1, 9, 0, 0, 0, time.UTC)
	for i := 0; i < 2; i++ {
		id := "h" + string(rune('0'+i))
		takenAt := base.AddDate(0, 0, i)
		_, err := db.Exec(`INSERT INTO assets(id, file_path, status, taken_at) VALUES(?,?,'indexed',?)`,
			id, "/g/"+id+".jpg", takenAt.UTC().Format("2006-01-02 15:04:05"))
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO asset_geo(asset_id, city, country) VALUES(?,?,?)`, id, "Nara", "JP")
		require.NoError(t, err)
	}

	// First round: namer fails, the moment is produced but still on the
	// template fallback (named_by_llm=0), ensuring that when we later hide it,
	// it's still in the state the naming loop would otherwise pick up.
	failingNamer := &fakeNamer{err: errors.New("ai unavailable")}
	svc := NewMomentsService(db, store, fakeThemeSearcher{}, noVecLoader, failingNamer)
	require.NoError(t, svc.RecomputeAll(context.Background()))

	moments, err := store.ListMoments()
	require.NoError(t, err)
	require.Len(t, moments, 1)
	require.False(t, moments[0].NamedByLLM)
	require.Equal(t, 1, failingNamer.calls)

	require.NoError(t, store.HideMoment(moments[0].ID))

	// Second round: swap in a namer that would succeed, but the naming loop
	// draws candidates from ListMoments() (already filtered by hidden=0), so
	// the hidden moment must not be fed to the LLM again — calls should stay 0
	// (a brand-new namer).
	successNamer := &fakeNamer{title: "Should Not Apply"}
	svc2 := NewMomentsService(db, store, fakeThemeSearcher{}, noVecLoader, successNamer)
	require.NoError(t, svc2.RecomputeAll(context.Background()))

	require.Equal(t, 0, successNamer.calls, "a hidden moment must not enter the LLM naming loop")

	stillHidden, err := store.ListMoments()
	require.NoError(t, err)
	require.Len(t, stillHidden, 0, "hidden state must persist across recompute (SyncRecipeMoments doesn't clear the hidden column)")
}

func TestRecomputeAll_ReentrancyReturnsNilImmediately(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)
	require.NoError(t, store.UpsertRecipes([]MomentRecipe{
		{Key: "trip", Kind: "trip", Title: "Trip", Enabled: true},
	}))

	namer := &fakeNamer{title: "irrelevant"}
	svc := NewMomentsService(db, store, fakeThemeSearcher{}, noVecLoader, namer)

	// Manually set the CAS flag to simulate "a recompute round is already
	// running" — same-package white-box tests can reach into internal fields
	// directly.
	svc.running.Store(true)
	defer svc.running.Store(false)

	err := svc.RecomputeAll(context.Background())
	require.NoError(t, err)

	moments, err := store.ListMoments()
	require.NoError(t, err)
	require.Empty(t, moments, "CAS re-entrancy should return immediately, doing no recompute work")
}

// TestRecomputeAll_PerRecipeFailureIsolation: when the CLIP search (ML) the
// theme engine depends on is offline, a single recipe failing must Warn +
// skip and continue processing the next recipe — must not block the
// ML-independent trip, nor clear that theme recipe's moments from the
// previous round (not calling SyncRecipeMoments means old moments are left
// as-is). RecomputeAll overall still returns nil.
func TestRecomputeAll_PerRecipeFailureIsolation(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)
	require.NoError(t, store.UpsertRecipes([]MomentRecipe{
		{Key: "trip", Kind: "trip", Title: "Trip", Enabled: true, ParamsJSON: `{"min_assets":2}`},
		{Key: "theme:pets", Kind: "theme", Title: "Pet Moments", Enabled: true,
			ParamsJSON: `{"min_assets":2,"clip_prompts":["a photo of a dog"],"caption_keywords":["dog"]}`},
	}))

	base := time.Date(2015, time.May, 1, 9, 0, 0, 0, time.UTC)
	for i := 0; i < 2; i++ {
		id := "trip-" + string(rune('a'+i))
		takenAt := base.AddDate(0, 0, i)
		_, err := db.Exec(`INSERT INTO assets(id, file_path, status, taken_at) VALUES(?,?,'indexed',?)`,
			id, "/g/"+id+".jpg", takenAt.UTC().Format("2006-01-02 15:04:05"))
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO asset_geo(asset_id, city, country) VALUES(?,?,?)`, id, "Nagoya", "JP")
		require.NoError(t, err)
	}
	for i, id := range []string{"theme-a", "theme-b"} {
		takenAt := base.AddDate(0, 0, 20+i)
		_, err := db.Exec(`INSERT INTO assets(id, file_path, status, taken_at) VALUES(?,?,'indexed',?)`,
			id, "/g/"+id+".jpg", takenAt.UTC().Format("2006-01-02 15:04:05"))
		require.NoError(t, err)
	}

	// First round: ML healthy, both trip + theme should produce moments.
	workingSearcher := fakeThemeSearcher{hits: map[string][]AssetScore{
		"a photo of a dog": {{AssetID: "theme-a", Score: 0.9}, {AssetID: "theme-b", Score: 0.8}},
	}}
	firstRun := NewMomentsService(db, store, workingSearcher, noVecLoader, &fakeNamer{title: "irrelevant"})
	require.NoError(t, firstRun.RecomputeAll(context.Background()))

	before, err := store.ListMoments()
	require.NoError(t, err)
	require.Len(t, before, 2, "with ML healthy, both trip+theme should produce moments")
	var themeBefore Moment
	for _, m := range before {
		if m.RecipeKey == "theme:pets" {
			themeBefore = m
		}
	}
	require.NotEmpty(t, themeBefore.ID, "the first round should have produced a theme:pets moment")

	// Second round: ML is offline (searcher errors), theme:pets should be
	// skipped, trip still recomputes normally.
	failingSearcher := fakeThemeSearcher{err: errors.New("clip search: connection refused (ML offline)")}
	secondRun := NewMomentsService(db, store, failingSearcher, noVecLoader, &fakeNamer{title: "irrelevant"})
	err = secondRun.RecomputeAll(context.Background())
	require.NoError(t, err, "a single recipe (theme) failing must be skipped best-effort, without blocking the round or propagating an error")

	after, err := store.ListMoments()
	require.NoError(t, err)
	require.Len(t, after, 2, "theme failing should keep its old moments uncleared; trip should still recompute normally")

	var themeAfter Moment
	for _, m := range after {
		if m.RecipeKey == "theme:pets" {
			themeAfter = m
		}
	}
	require.Equal(t, themeBefore.ID, themeAfter.ID)
	require.Equal(t, themeBefore.UpdatedAt, themeAfter.UpdatedAt,
		"a recipe skipped due to an ML blip must not call SyncRecipeMoments, updated_at unchanged")
}

// TestCleanLLMTitle_TruncatesOverlongOutput: when the model doesn't respect
// the "at most 4 words" constraint and tacks on a long explanation,
// cleanLLMTitle must do a rune-safe truncation to maxLLMTitleRunes, to
// prevent the long text from being persisted and shown to the user as-is.
func TestCleanLLMTitle_TruncatesOverlongOutput(t *testing.T) {
	long := strings.Repeat("汉字标题超长测试", 20) // 160 runes, well past the 80 cap
	got := cleanLLMTitle(long)
	require.Len(t, []rune(got), maxLLMTitleRunes)
	require.Equal(t, string([]rune(long)[:maxLLMTitleRunes]), got)

	// Short titles are unaffected.
	require.Equal(t, "Sunset Beach", cleanLLMTitle(`  "Sunset Beach"  `+"\nsome trailing explanation"))
}

// TestRecomputeAll_ThemeMomentsNeverGoThroughLLMNaming: real-device testing
// found the LLM would make a theme's curated title (recipe.Title, e.g. "Pet
// Moments") worse (e.g. mistakenly changing it to "Sunset on Highway"), so
// theme moments must never enter the LLM naming loop; trip moments should
// still try LLM naming normally. Uses the same fakeNamer to record the call
// count, asserting only the trip call happens, and the theme title stays
// exactly recipe.Title, not overwritten by fakeNamer's fixed value.
func TestRecomputeAll_ThemeMomentsNeverGoThroughLLMNaming(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)
	require.NoError(t, store.UpsertRecipes([]MomentRecipe{
		{Key: "trip", Kind: "trip", Title: "Trip", Enabled: true, ParamsJSON: `{"min_assets":2}`},
		{Key: "theme:pets", Kind: "theme", Title: "Pet Moments", Enabled: true,
			ParamsJSON: `{"min_assets":2,"clip_prompts":["a photo of a dog"],"caption_keywords":["dog"]}`},
	}))

	base := time.Date(2016, time.February, 1, 9, 0, 0, 0, time.UTC)
	for i := 0; i < 2; i++ {
		id := "d" + string(rune('0'+i))
		takenAt := base.AddDate(0, 0, i)
		_, err := db.Exec(`INSERT INTO assets(id, file_path, status, taken_at) VALUES(?,?,'indexed',?)`,
			id, "/g/"+id+".jpg", takenAt.UTC().Format("2006-01-02 15:04:05"))
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO asset_geo(asset_id, city, country) VALUES(?,?,?)`, id, "Tokyo", "JP")
		require.NoError(t, err)
	}

	searcher := fakeThemeSearcher{hits: map[string][]AssetScore{
		"a photo of a dog": {{AssetID: "theme-a", Score: 0.9}, {AssetID: "theme-b", Score: 0.8}},
	}}
	for i, id := range []string{"theme-a", "theme-b"} {
		takenAt := base.AddDate(0, 0, 20+i)
		_, err := db.Exec(`INSERT INTO assets(id, file_path, status, taken_at) VALUES(?,?,'indexed',?)`,
			id, "/g/"+id+".jpg", takenAt.UTC().Format("2006-01-02 15:04:05"))
		require.NoError(t, err)
	}

	namer := &fakeNamer{title: "Sunset On Highway"} // simulates the LLM mangling the name
	svc := NewMomentsService(db, store, searcher, noVecLoader, namer)

	require.NoError(t, svc.RecomputeAll(context.Background()))

	require.Equal(t, 1, namer.calls, "only the trip moment should trigger LLM naming, theme should never call it")

	moments, err := store.ListMoments()
	require.NoError(t, err)
	require.Len(t, moments, 2)

	var trip, theme Moment
	for _, m := range moments {
		switch m.RecipeKey {
		case "trip":
			trip = m
		case "theme:pets":
			theme = m
		}
	}
	require.True(t, trip.NamedByLLM, "trip moments should go through LLM naming normally")
	require.Equal(t, "Sunset On Highway", trip.Title)
	require.False(t, theme.NamedByLLM, "theme moments should never be marked as LLM-named")
	require.Equal(t, "Pet Moments", theme.Title, "the theme title must keep recipe.Title's curated name, not be tampered with by the LLM")
}

// TestBuildNamingPrompt_NoPhotoAppEchoAndHasHardenedConstraints: real-device
// testing found weak local models would echo the old prompt's "photo app"
// wording into the title (e.g. "Nighttime Las Vegas Photo App."), so the new
// prompt must not contain that wording, and must explicitly list the
// hardening constraints Title Case/English only/≤4 words/no punctuation or
// quotes/don't repeat the instructions, plus few-shot examples.
func TestBuildNamingPrompt_NoPhotoAppEchoAndHasHardenedConstraints(t *testing.T) {
	m := Moment{Place: "Kyoto, JP", TimeFrom: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)}
	prompt := buildNamingPrompt(m, []string{"a photo of a temple"})

	require.NotContains(t, prompt, "photo app", "the prompt must no longer contain the 'photo app' wording that gets echoed into the title")
	require.Contains(t, prompt, "Title Case")
	require.Contains(t, prompt, "English only")
	require.Contains(t, prompt, "at most 4 words")
	require.Contains(t, prompt, "no punctuation or quotes")
	require.Contains(t, prompt, "do not repeat or explain these instructions")
	require.Contains(t, prompt, "Golden Gate Evenings", "should include a few-shot example")
	require.Contains(t, prompt, "Alpine Ski Days", "should include a second few-shot example")
}

// TestRecomputeAll_PetEntitiesReplaceConceptThemePets: the replacement rule,
// positive case — when profile:pets mines ≥1 qualifying pet entity, the
// concept-version theme:pets moment must be cleared (even though the theme
// engine's own criterion could match the same photos, what should be
// produced is "the user's own dog", not "a library-wide search for dog
// elements"). Recipe key lexical order profile:pets < theme:pets (p<t),
// ListRecipes sorts by key ascending, so by the time this round's loop
// reaches theme:pets, the petEntitiesProduced flag is already set.
func TestRecomputeAll_PetEntitiesReplaceConceptThemePets(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)
	require.NoError(t, store.UpsertRecipes([]MomentRecipe{
		{Key: "profile:pets", Kind: "pet_entities", Title: "Pet Entities", Enabled: true,
			ParamsJSON: `{"lexicon":["beagle"],"min_photos":2,"min_months":1}`},
		{Key: "theme:pets", Kind: "theme", Title: "Pet Moments", Enabled: true,
			ParamsJSON: `{"min_assets":2,"clip_prompts":["a photo of a dog"],"caption_keywords":["dog"]}`},
	}))

	base := time.Date(2020, time.March, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		id := "pet-" + string(rune('a'+i))
		takenAt := base.AddDate(0, 0, i)
		_, err := db.Exec(`INSERT INTO assets(id, file_path, status, taken_at) VALUES(?,?,'indexed',?)`,
			id, "/g/"+id+".jpg", takenAt.UTC().Format("2006-01-02 15:04:05"))
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO asset_caption(asset_id, text) VALUES(?,?)`, id, "our beagle dog running")
		require.NoError(t, err)
	}

	// If the replacement rule didn't kick in, the theme engine matching these 3
	// photos via caption_keywords "dog" would also be enough to qualify
	// (min_assets=2) and produce the concept version — this test case must
	// prove it gets cleared, not that "there were never any candidates".
	searcher := fakeThemeSearcher{hits: map[string][]AssetScore{
		"a photo of a dog": {
			{AssetID: "pet-a", Score: 0.9}, {AssetID: "pet-b", Score: 0.9}, {AssetID: "pet-c", Score: 0.9},
		},
	}}
	svc := NewMomentsService(db, store, searcher, noVecLoader, &fakeNamer{title: "irrelevant"})

	require.NoError(t, svc.RecomputeAll(context.Background()))

	moments, err := store.ListMoments()
	require.NoError(t, err)

	var recipeKeys []string
	for _, m := range moments {
		recipeKeys = append(recipeKeys, m.RecipeKey)
	}
	require.NotContains(t, recipeKeys, "theme:pets", "once a personalized pet entity moment is produced it should replace the concept version")
	require.Contains(t, recipeKeys, "profile:pets", "should produce a Your Beagle entity moment")

	var petMoment Moment
	for _, m := range moments {
		if m.RecipeKey == "profile:pets" {
			petMoment = m
		}
	}
	require.Equal(t, "Your Beagle", petMoment.Title)
}

// TestRecomputeAll_NoPetEntitiesFallsBackToConceptThemePets: the replacement
// rule, negative case — when the library has no qualifying pet entity, the
// concept-version theme:pets is produced as usual (fallback semantics).
func TestRecomputeAll_NoPetEntitiesFallsBackToConceptThemePets(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)
	require.NoError(t, store.UpsertRecipes([]MomentRecipe{
		// min_photos is deliberately set far higher than this round's actual
		// photo count, to ensure profile:pets produces no qualifying entity this
		// round.
		{Key: "profile:pets", Kind: "pet_entities", Title: "Pet Entities", Enabled: true,
			ParamsJSON: `{"lexicon":["labrador"],"min_photos":50,"min_months":5}`},
		{Key: "theme:pets", Kind: "theme", Title: "Pet Moments", Enabled: true,
			ParamsJSON: `{"min_assets":2,"clip_prompts":["a photo of a dog"],"caption_keywords":["dog"]}`},
	}))

	base := time.Date(2021, time.May, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 2; i++ {
		id := "pet2-" + string(rune('a'+i))
		takenAt := base.AddDate(0, 0, i)
		_, err := db.Exec(`INSERT INTO assets(id, file_path, status, taken_at) VALUES(?,?,'indexed',?)`,
			id, "/g/"+id+".jpg", takenAt.UTC().Format("2006-01-02 15:04:05"))
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO asset_caption(asset_id, text) VALUES(?,?)`, id, "a labrador dog running")
		require.NoError(t, err)
	}

	searcher := fakeThemeSearcher{hits: map[string][]AssetScore{
		"a photo of a dog": {
			{AssetID: "pet2-a", Score: 0.9}, {AssetID: "pet2-b", Score: 0.9},
		},
	}}
	svc := NewMomentsService(db, store, searcher, noVecLoader, &fakeNamer{title: "irrelevant"})

	require.NoError(t, svc.RecomputeAll(context.Background()))

	moments, err := store.ListMoments()
	require.NoError(t, err)

	var themeMoment Moment
	var found bool
	for _, m := range moments {
		if m.RecipeKey == "theme:pets" {
			themeMoment = m
			found = true
		}
	}
	require.True(t, found, "with no qualifying pet entity, the concept-version theme:pets should be produced as usual (fallback)")
	require.Equal(t, "Pet Moments", themeMoment.Title)
}

// TestRecomputeAll_OverlongLLMTitleRejectedKeepsTemplate: an end-to-end check
// that when RecomputeAll faces an LLM output far past the word-count guard,
// it doesn't store it into moments.title as-is (or truncated) — cleanLLMTitle's
// rune truncation still applies (see maxLLMTitleRunes), but the word count
// after truncation is still > maxLLMTitleWords, so the word-count guard
// rejects the whole title outright, keeping the template fallback title.
func TestRecomputeAll_OverlongLLMTitleRejectedKeepsTemplate(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)
	require.NoError(t, store.UpsertRecipes([]MomentRecipe{
		{Key: "trip", Kind: "trip", Title: "Trip", Enabled: true, ParamsJSON: `{"min_assets":2}`},
	}))

	base := time.Date(2014, time.April, 1, 9, 0, 0, 0, time.UTC)
	for i := 0; i < 2; i++ {
		id := "c" + string(rune('0'+i))
		takenAt := base.AddDate(0, 0, i)
		_, err := db.Exec(`INSERT INTO assets(id, file_path, status, taken_at) VALUES(?,?,'indexed',?)`,
			id, "/g/"+id+".jpg", takenAt.UTC().Format("2006-01-02 15:04:05"))
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO asset_geo(asset_id, city, country) VALUES(?,?,?)`, id, "Sapporo", "JP")
		require.NoError(t, err)
	}

	overlong := strings.Repeat("a very long unwanted explanation ", 10)
	namer := &fakeNamer{title: overlong}
	svc := NewMomentsService(db, store, fakeThemeSearcher{}, noVecLoader, namer)

	require.NoError(t, svc.RecomputeAll(context.Background()))

	moments, err := store.ListMoments()
	require.NoError(t, err)
	require.Len(t, moments, 1)
	require.LessOrEqual(t, len([]rune(moments[0].Title)), maxLLMTitleRunes)
	require.False(t, moments[0].NamedByLLM, "exceeding the word count should be rejected by the word-count guard, not counted as successful LLM naming")
	require.Equal(t, "Sapporo Trip", moments[0].Title, "a rejection should keep the template fallback title")
}

// TestRecomputeAll_LLMTitleWordGuardRejectsOverSixWords: the core scenario
// confirmed on real devices — a weak local model doesn't follow the "at most
// 4 words" instruction and spits out a 7-word sentence (e.g. a
// date+weather mashup like "May 28 2011 Overcast Sky Somewhere"); the
// word-count guard should reject the whole title outright, keep the template
// fallback title, not set named_by_llm, and leave a Debug log entry for
// observability (the rejected title).
func TestRecomputeAll_LLMTitleWordGuardRejectsOverSixWords(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)
	require.NoError(t, store.UpsertRecipes([]MomentRecipe{
		{Key: "trip", Kind: "trip", Title: "Trip", Enabled: true, ParamsJSON: `{"min_assets":2}`},
	}))

	base := time.Date(2015, time.May, 1, 9, 0, 0, 0, time.UTC)
	for i := 0; i < 2; i++ {
		id := "d" + string(rune('0'+i))
		takenAt := base.AddDate(0, 0, i)
		_, err := db.Exec(`INSERT INTO assets(id, file_path, status, taken_at) VALUES(?,?,'indexed',?)`,
			id, "/g/"+id+".jpg", takenAt.UTC().Format("2006-01-02 15:04:05"))
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO asset_geo(asset_id, city, country) VALUES(?,?,?)`, id, "Naha", "JP")
		require.NoError(t, err)
	}

	rejected := "May 28 2011 Overcast Sky Somewhere Nearby" // 7 words, over the 6-word cap
	namer := &fakeNamer{title: rejected}
	svc := NewMomentsService(db, store, fakeThemeSearcher{}, noVecLoader, namer)

	obsCore, logs := observer.New(zap.DebugLevel)
	restore := zap.ReplaceGlobals(zap.New(obsCore))
	defer restore()

	require.NoError(t, svc.RecomputeAll(context.Background()))

	moments, err := store.ListMoments()
	require.NoError(t, err)
	require.Len(t, moments, 1)
	require.False(t, moments[0].NamedByLLM, "over 6 words should be rejected, not counted as successful LLM naming")
	require.Equal(t, "Naha Trip", moments[0].Title, "a rejection should leave the template fallback title unchanged")

	found := false
	for _, entry := range logs.All() {
		if strings.Contains(entry.Message, "exceeded word limit") {
			found = true
			break
		}
	}
	require.True(t, found, "a rejected title should leave a Debug log entry")
}

// TestRecomputeAll_LLMTitleWordGuardAcceptsUpToSixWords: a title with exactly
// 6 words should be accepted normally, overwriting the template name and
// setting named_by_llm=1 — the guard only rejects titles over 6 words, not
// misfiring on the boundary value.
func TestRecomputeAll_LLMTitleWordGuardAcceptsUpToSixWords(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)
	require.NoError(t, store.UpsertRecipes([]MomentRecipe{
		{Key: "trip", Kind: "trip", Title: "Trip", Enabled: true, ParamsJSON: `{"min_assets":2}`},
	}))

	base := time.Date(2015, time.June, 1, 9, 0, 0, 0, time.UTC)
	for i := 0; i < 2; i++ {
		id := "e" + string(rune('0'+i))
		takenAt := base.AddDate(0, 0, i)
		_, err := db.Exec(`INSERT INTO assets(id, file_path, status, taken_at) VALUES(?,?,'indexed',?)`,
			id, "/g/"+id+".jpg", takenAt.UTC().Format("2006-01-02 15:04:05"))
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO asset_geo(asset_id, city, country) VALUES(?,?,?)`, id, "Naha", "JP")
		require.NoError(t, err)
	}

	accepted := "One Two Three Four Five Six" // exactly 6 words
	namer := &fakeNamer{title: accepted}
	svc := NewMomentsService(db, store, fakeThemeSearcher{}, noVecLoader, namer)

	require.NoError(t, svc.RecomputeAll(context.Background()))

	moments, err := store.ListMoments()
	require.NoError(t, err)
	require.Len(t, moments, 1)
	require.True(t, moments[0].NamedByLLM, "6 words should be accepted normally, setting named_by_llm=1")
	require.Equal(t, accepted, moments[0].Title)
}
