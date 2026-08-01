// Tests for the theme engine: covers the Step 1 brief checklist — two
// ClipPrompts with overlapping hits (score takes the max), a caption keyword
// hit merged in (doesn't overwrite a higher clip score), MinScore filtering,
// candidate pool intersection (excluding documents/trash/offline/live photo
// companion videos), the MinAssets threshold.
package service

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeThemeSearcher is a test double for clipTextSearcher: returns preset
// hits per prompt, without touching the real ML/vector table. When err is
// non-nil, it simulates ML (the immich CLIP container) being offline —
// SearchAssetsByText returns this error for any prompt, used by
// MomentsService.RecomputeAll's per-recipe failure isolation tests.
type fakeThemeSearcher struct {
	hits map[string][]AssetScore
	err  error
}

func (f fakeThemeSearcher) SearchAssetsByText(_ context.Context, prompt string, _ int) ([]AssetScore, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.hits[prompt], nil
}

// insertThemeAsset inserts an asset; status/deleted_at/offline/
// is_live_photo_video can optionally be overridden afterward (via a
// subsequent UPDATE); by default it satisfies the candidate pool criteria.
func insertThemeAsset(t *testing.T, db *sql.DB, id string, takenAt time.Time) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO assets(id, file_path, status, taken_at) VALUES(?,?,'indexed',?)`,
		id, "/g/"+id+".jpg", takenAt.UTC().Format("2006-01-02 15:04:05"))
	require.NoError(t, err)
}

func insertCaption(t *testing.T, db *sql.DB, assetID, text string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO asset_caption(asset_id, text) VALUES(?,?)`, assetID, text)
	require.NoError(t, err)
}

func TestBuildThemeMoments_UnionScoreMaxAndCandidatePool(t *testing.T) {
	db := makeTestDB(t)

	base := time.Date(2011, time.January, 1, 12, 0, 0, 0, time.UTC)
	day := func(n int) time.Time { return base.AddDate(0, 0, n-1) }

	// 5 candidates that should end up selected:
	insertThemeAsset(t, db, "a1", day(1))   // clip prompt1 hits 0.9
	insertThemeAsset(t, db, "a2", day(2))   // both clip prompts hit, take max=0.6; caption also hits but doesn't pull the score down
	insertThemeAsset(t, db, "a4", day(4))   // clip prompt2 hits 0.3
	insertThemeAsset(t, db, "a5", day(5))   // only the caption keyword hits, gets the MinScore floor score
	insertThemeAsset(t, db, "a10", day(10)) // clip prompt1 hits 0.25 (above the default MinScore=0.2)

	// Should be filtered by MinScore (default 0.2): 0.1 < 0.2.
	insertThemeAsset(t, db, "a11", day(11))

	// Candidate pool exclusions (each has a hit but shouldn't appear in the result):
	insertThemeAsset(t, db, "doc6", day(6)) // document: hasOcrExpr matches
	_, err := db.Exec(`INSERT INTO asset_ocr(asset_id, text, coverage, line_count, is_doc)
		VALUES('doc6','a long ocr text with many lines of content here',0.9,20,1)`)
	require.NoError(t, err)

	insertThemeAsset(t, db, "trash7", day(7)) // trash
	_, err = db.Exec(`UPDATE assets SET deleted_at=? WHERE id='trash7'`, "2011-01-08 00:00:00")
	require.NoError(t, err)
	insertCaption(t, db, "trash7", "a dog in the trash")

	insertThemeAsset(t, db, "offline8", day(8)) // offline
	_, err = db.Exec(`UPDATE assets SET offline=1 WHERE id='offline8'`)
	require.NoError(t, err)

	insertThemeAsset(t, db, "livevid9", day(9)) // live photo companion video
	_, err = db.Exec(`UPDATE assets SET is_live_photo_video=1 WHERE id='livevid9'`)
	require.NoError(t, err)

	// Caption keyword hits: a5 (should be kept), a2 (hits but shouldn't pull
	// down its already-higher clip score), trash7 (should be filtered out by
	// the candidate pool).
	insertCaption(t, db, "a5", "a cute dog playing in the yard")
	insertCaption(t, db, "a2", "a dog and a cat both sitting here")

	searcher := fakeThemeSearcher{hits: map[string][]AssetScore{
		"prompt1": {
			{AssetID: "a1", Score: 0.9},
			{AssetID: "a2", Score: 0.5},
			{AssetID: "a10", Score: 0.25},
			{AssetID: "a11", Score: 0.1}, // below MinScore, should be filtered
			{AssetID: "doc6", Score: 0.9},
			{AssetID: "offline8", Score: 0.9},
		},
		"prompt2": {
			{AssetID: "a2", Score: 0.6}, // takes the max against prompt1's 0.5 => 0.6
			{AssetID: "a4", Score: 0.3},
			{AssetID: "livevid9", Score: 0.9},
		},
	}}

	recipe := MomentRecipe{
		Key:   "theme:pets",
		Kind:  "theme",
		Title: "Pet Moments",
		ParamsJSON: `{"clip_prompts":["prompt1","prompt2"],
			"caption_keywords":["dog","cat"],"min_assets":5}`,
	}

	drafts, err := BuildThemeMoments(context.Background(), db, searcher, recipe)
	require.NoError(t, err)
	require.Len(t, drafts, 1)

	d := drafts[0]
	require.Equal(t, ThemeMomentID("theme:pets"), d.ID)
	require.Equal(t, "theme:pets", d.RecipeKey)
	require.Equal(t, "Pet Moments", d.Title)
	require.Len(t, d.Assets, 5)

	byID := map[string]MomentAsset{}
	for _, a := range d.Assets {
		byID[a.AssetID] = a
	}
	require.Contains(t, byID, "a1")
	require.Contains(t, byID, "a2")
	require.Contains(t, byID, "a4")
	require.Contains(t, byID, "a5")
	require.Contains(t, byID, "a10")
	require.NotContains(t, byID, "a11", "below MinScore should be filtered")
	require.NotContains(t, byID, "doc6", "a document should be excluded by the candidate pool")
	require.NotContains(t, byID, "trash7", "trash should be excluded by the candidate pool")
	require.NotContains(t, byID, "offline8", "offline should be excluded by the candidate pool")
	require.NotContains(t, byID, "livevid9", "a live photo companion video should be excluded by the candidate pool")

	require.InDelta(t, 0.9, byID["a1"].Score, 1e-9)
	require.InDelta(t, 0.6, byID["a2"].Score, 1e-9, "a hit on both paths takes the max, must not be pulled down by the caption floor score")
	require.InDelta(t, 0.3, byID["a4"].Score, 1e-9)
	require.InDelta(t, 0.2, byID["a5"].Score, 1e-9, "a caption-only hit should get the MinScore floor score")
	require.InDelta(t, 0.25, byID["a10"].Score, 1e-9)

	require.True(t, d.TimeFrom.Equal(day(1)))
	require.True(t, d.TimeTo.Equal(day(10)))
}

func TestBuildThemeMoments_BelowMinAssetsReturnsEmpty(t *testing.T) {
	db := makeTestDB(t)

	base := time.Date(2011, time.January, 1, 12, 0, 0, 0, time.UTC)
	insertThemeAsset(t, db, "a1", base)
	insertThemeAsset(t, db, "a2", base.AddDate(0, 0, 1))
	insertThemeAsset(t, db, "a3", base.AddDate(0, 0, 2))

	searcher := fakeThemeSearcher{hits: map[string][]AssetScore{
		"prompt1": {
			{AssetID: "a1", Score: 0.9},
			{AssetID: "a2", Score: 0.9},
			{AssetID: "a3", Score: 0.9},
		},
	}}

	// min_assets not overridden, falls back to the default of 10; with only 3
	// candidates, no draft should be produced.
	recipe := MomentRecipe{
		Key:        "theme:pets",
		Kind:       "theme",
		Title:      "Pet Moments",
		ParamsJSON: `{"clip_prompts":["prompt1"]}`,
	}

	drafts, err := BuildThemeMoments(context.Background(), db, searcher, recipe)
	require.NoError(t, err)
	require.Empty(t, drafts)
}

// TestMatchCaptionKeywords_WordBoundaryNotSubstring: real-device testing
// found the old substring criterion instr(lower(text), kw) has no word
// boundary, so "cat"⊂vacation/location, "pet"⊂carpet, "ice"⊂nice/service
// were all falsely matched (the root cause of theme:pets 1306/theme:snow
// 1610 over-matching out of 6882 library-wide). Asserts that after the fix:
// a genuine whole-word hit is still recorded, while a substring false match
// must be excluded.
func TestMatchCaptionKeywords_WordBoundaryNotSubstring(t *testing.T) {
	db := makeTestDB(t)
	insertThemeAsset(t, db, "vac1", time.Now())
	insertCaption(t, db, "vac1", "our vacation in rome")

	insertThemeAsset(t, db, "cat1", time.Now())
	insertCaption(t, db, "cat1", "a cat on the sofa")

	insertThemeAsset(t, db, "carpet1", time.Now())
	insertCaption(t, db, "carpet1", "a red carpet in the hallway")

	insertThemeAsset(t, db, "nice1", time.Now())
	insertCaption(t, db, "nice1", "such a nice service today")

	hits, err := matchCaptionKeywords(context.Background(), db, []string{"cat", "pet", "ice"})
	require.NoError(t, err)

	require.NotContains(t, hits, "vac1", `"vacation" contains the substring "cat" but isn't the whole word, should not match`)
	require.Contains(t, hits, "cat1", `"a cat on the sofa" matches "cat" as a whole word`)
	require.NotContains(t, hits, "carpet1", `"carpet" contains the substring "pet" but isn't the whole word, should not match`)
	require.NotContains(t, hits, "nice1", `"nice"/"service" contain the substring "ice" but aren't the whole word, should not match`)
}

func TestBuildThemeMoments_NoHitsReturnsEmpty(t *testing.T) {
	db := makeTestDB(t)
	searcher := fakeThemeSearcher{hits: map[string][]AssetScore{}}
	recipe := MomentRecipe{
		Key:        "theme:pets",
		Kind:       "theme",
		Title:      "Pet Moments",
		ParamsJSON: `{"clip_prompts":["prompt1"],"caption_keywords":["dog"]}`,
	}
	drafts, err := BuildThemeMoments(context.Background(), db, searcher, recipe)
	require.NoError(t, err)
	require.Empty(t, drafts)
}
