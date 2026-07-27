// theme 引擎测试:覆盖简报 Step 1 清单——两条 ClipPrompts 有重叠命中(score
// 取 max)、caption 关键词命中并入(不覆盖更高的 clip 分)、MinScore 过滤、
// 候选池交集(排除文档/回收站/离线/live photo 视频侧)、MinAssets 门槛。
package service

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeThemeSearcher 是 clipTextSearcher 的测试替身:按 prompt 返回预设命中,
// 不接触真实 ML/向量表。
type fakeThemeSearcher struct {
	hits map[string][]AssetScore
}

func (f fakeThemeSearcher) SearchAssetsByText(_ context.Context, prompt string, _ int) ([]AssetScore, error) {
	return f.hits[prompt], nil
}

// insertThemeAsset 插入一条资产,可选自定义 status/deleted_at/offline/
// is_live_photo_video 覆盖(通过后续 UPDATE),默认满足候选池条件。
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

	// 5 个应最终入选的候选:
	insertThemeAsset(t, db, "a1", day(1))   // clip prompt1 命中 0.9
	insertThemeAsset(t, db, "a2", day(2))   // clip 两条 prompt 都命中,取 max=0.6;caption 也命中但不拉低分数
	insertThemeAsset(t, db, "a4", day(4))   // clip prompt2 命中 0.3
	insertThemeAsset(t, db, "a5", day(5))   // 仅 caption 关键词命中,记 MinScore 保底分
	insertThemeAsset(t, db, "a10", day(10)) // clip prompt1 命中 0.25(高于默认 MinScore=0.2)

	// 应被 MinScore(默认 0.2)过滤掉:0.1 < 0.2。
	insertThemeAsset(t, db, "a11", day(11))

	// 候选池排除项(各自命中但不该出现在结果里):
	insertThemeAsset(t, db, "doc6", day(6)) // 文档:hasOcrExpr 命中
	_, err := db.Exec(`INSERT INTO asset_ocr(asset_id, text, coverage, line_count, is_doc)
		VALUES('doc6','a long ocr text with many lines of content here',0.9,20,1)`)
	require.NoError(t, err)

	insertThemeAsset(t, db, "trash7", day(7)) // 回收站
	_, err = db.Exec(`UPDATE assets SET deleted_at=? WHERE id='trash7'`, "2011-01-08 00:00:00")
	require.NoError(t, err)
	insertCaption(t, db, "trash7", "a dog in the trash")

	insertThemeAsset(t, db, "offline8", day(8)) // 离线
	_, err = db.Exec(`UPDATE assets SET offline=1 WHERE id='offline8'`)
	require.NoError(t, err)

	insertThemeAsset(t, db, "livevid9", day(9)) // live photo 视频侧
	_, err = db.Exec(`UPDATE assets SET is_live_photo_video=1 WHERE id='livevid9'`)
	require.NoError(t, err)

	// caption 关键词命中:a5(应保留)、a2(命中但不应拉低其已有的更高 clip 分)、
	// trash7(应被候选池滤掉)。
	insertCaption(t, db, "a5", "a cute dog playing in the yard")
	insertCaption(t, db, "a2", "a dog and a cat both sitting here")

	searcher := fakeThemeSearcher{hits: map[string][]AssetScore{
		"prompt1": {
			{AssetID: "a1", Score: 0.9},
			{AssetID: "a2", Score: 0.5},
			{AssetID: "a10", Score: 0.25},
			{AssetID: "a11", Score: 0.1}, // 低于 MinScore,应被过滤
			{AssetID: "doc6", Score: 0.9},
			{AssetID: "offline8", Score: 0.9},
		},
		"prompt2": {
			{AssetID: "a2", Score: 0.6}, // 与 prompt1 的 0.5 取 max => 0.6
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
	require.NotContains(t, byID, "a11", "低于 MinScore 应被过滤")
	require.NotContains(t, byID, "doc6", "文档应被候选池排除")
	require.NotContains(t, byID, "trash7", "回收站应被候选池排除")
	require.NotContains(t, byID, "offline8", "离线应被候选池排除")
	require.NotContains(t, byID, "livevid9", "live photo 视频侧应被候选池排除")

	require.InDelta(t, 0.9, byID["a1"].Score, 1e-9)
	require.InDelta(t, 0.6, byID["a2"].Score, 1e-9, "两路命中取 max,不应被 caption 保底分拉低")
	require.InDelta(t, 0.3, byID["a4"].Score, 1e-9)
	require.InDelta(t, 0.2, byID["a5"].Score, 1e-9, "仅 caption 命中应记 MinScore 保底分")
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

	// 未覆盖 min_assets,回落默认值 10;只有 3 个候选,应产不出 draft。
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
