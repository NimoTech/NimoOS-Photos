// MomentStore 的测试:三表(moment_recipes/moments/moment_assets)+ repo 层语义。
// 覆盖简报 Step 1 清单:seed 幂等且不覆盖已推送 recipe、UpsertRecipes 热更、
// SyncRecipeMoments 的 upsert/成员替换/删除消失时刻/保留 LLM title 四语义、
// id 稳定性同周同 id、ParseParams 默认值,以及本轮"可编辑时刻"存储层:
// moment_edits 迁移幂等、pin/exclude 回放存活、hidden tombstone、派生字段
// (asset_count/时间窗/封面)刷新、TopFeaturedByMoment 形状。
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

// insertMomentAsset 插入一条 moment_assets 会外键引用到的资产行(与
// captionpull_test.go 的 insertCaptionAsset 同法,id 存在即可)。
func insertMomentAsset(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO assets(id, file_path, status) VALUES(?,?,'indexed')`, id, "/g/"+id+".jpg")
	require.NoError(t, err)
}

// insertMomentAssetAt 插入一条带 taken_at 的资产行,供派生时间窗刷新测试使用。
func insertMomentAssetAt(t *testing.T, db *sql.DB, id string, takenAt time.Time) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO assets(id, file_path, status, taken_at) VALUES(?,?,'indexed',?)`,
		id, "/g/"+id+".jpg", takenAt.UTC().Format("2006-01-02 15:04:05"))
	require.NoError(t, err)
}

// ── ParseParams 默认值 ──────────────────────────────────────────────────

func TestMomentStore_ParseParamsDefaults(t *testing.T) {
	// 空 params(未推送任何配置)应全部落到默认值。
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

	// 部分字段显式指定时,只有指定的字段生效,其余仍回落默认值。
	p3, err := ParseParams(MomentRecipe{ParamsJSON: `{"min_assets":5,"clip_prompts":["a cat"]}`})
	require.NoError(t, err)
	require.Equal(t, 5, p3.MinAssets)
	require.Equal(t, 12, p3.MaxFeatured, "未指定字段应回落默认值")
	require.Equal(t, []string{"a cat"}, p3.ClipPrompts)

	// 非法 JSON 应返回 err。
	_, err = ParseParams(MomentRecipe{ParamsJSON: `{bad json`})
	require.Error(t, err)
}

// ── ParseParams:profile 新字段默认值(老字段/默认值不受影响)────────────

func TestMomentStore_ParseParamsProfileDefaults(t *testing.T) {
	p, err := ParseParams(MomentRecipe{ParamsJSON: ""})
	require.NoError(t, err)
	require.Nil(t, p.Lexicon, "未指定 lexicon 应为空,不应凭空回落默认词表")
	require.Equal(t, 8, p.MinPhotos)
	require.Equal(t, 2, p.MinMonths)
	require.Equal(t, 0.45, p.ClipMinScore)
	require.Equal(t, 100, p.ClipTopK)
	require.Equal(t, 5, p.TopPersons)
	require.Equal(t, 30, p.MinPersonPhotos)
	require.Equal(t, 2, p.MinTogetherPersons)
	// 老字段默认值不受影响。
	require.Equal(t, 10, p.MinAssets)
	require.Equal(t, 12, p.MaxFeatured)
	require.Equal(t, 14, p.GapDays)
	require.Equal(t, 200, p.TopK)
	require.Equal(t, 0.2, p.MinScore)

	// 部分字段显式指定时,只有指定字段生效,其余仍回落默认值。
	p2, err := ParseParams(MomentRecipe{ParamsJSON: `{"min_photos":20,"lexicon":["beagle"]}`})
	require.NoError(t, err)
	require.Equal(t, 20, p2.MinPhotos)
	require.Equal(t, []string{"beagle"}, p2.Lexicon)
	require.Equal(t, 2, p2.MinMonths, "未指定字段应回落默认值")
}

// ── SeedDefaultRecipes:幂等 + 不覆盖已推送 recipe ──────────────────────

func TestMomentStore_SeedIdempotentAndDoesNotOverwritePushed(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)

	require.NoError(t, store.SeedDefaultRecipes())
	recipes, err := store.ListRecipes(false)
	require.NoError(t, err)
	require.Len(t, recipes, 8, "内置集:trip + 5 个 theme + 2 个 profile")

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

	// profile:pets 的 lexicon 应有实质词表(≈60-100 英文物种/品种词)。
	petParams, err := ParseParams(keys["profile:pets"])
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(petParams.Lexicon), 60, "lexicon 应覆盖足够多的物种/品种词")
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
	require.Equal(t, 10, familyParams.MinAssets, "family 复用 min_assets 字段(合影集门槛)")

	// 再次 seed 应保持幂等,不报错、不重复。
	require.NoError(t, store.SeedDefaultRecipes())
	recipes2, err := store.ListRecipes(false)
	require.NoError(t, err)
	require.Len(t, recipes2, 8)

	// 模拟运维/应用商店已推送过对 theme:pets 的热更新。
	require.NoError(t, store.UpsertRecipes([]MomentRecipe{
		{Key: "theme:pets", Kind: "theme", Title: "Custom Pets", ParamsJSON: `{"min_assets":5}`, Enabled: true},
	}))

	// 再次 seed 不应把已推送的 recipe 覆盖回默认文案。
	require.NoError(t, store.SeedDefaultRecipes())
	recipes3, err := store.ListRecipes(false)
	require.NoError(t, err)
	for _, r := range recipes3 {
		if r.Key == "theme:pets" {
			require.Equal(t, "Custom Pets", r.Title, "seed 不应覆盖已推送的 recipe")
		}
	}
}

// ── UpsertRecipes:热更新入口 ────────────────────────────────────────────

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

	// 再次 upsert 同 key,应覆盖全字段(热更新)。
	require.NoError(t, store.UpsertRecipes([]MomentRecipe{
		{Key: "theme:art", Kind: "theme", Title: "Art & Design", ParamsJSON: `{"min_assets":3}`, Enabled: false},
	}))
	recipes2, err := store.ListRecipes(false)
	require.NoError(t, err)
	require.Len(t, recipes2, 1)
	require.Equal(t, "Art & Design", recipes2[0].Title)
	require.False(t, recipes2[0].Enabled)

	// enabledOnly 过滤。
	onlyEnabled, err := store.ListRecipes(true)
	require.NoError(t, err)
	require.Len(t, onlyEnabled, 0, "recipe 已被禁用,enabledOnly 应过滤掉")
}

// ── SyncRecipeMoments:upsert + 成员全量替换 ────────────────────────────

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

	// 重算:成员集合变化(去掉 a2,加入 a3)——应全量替换,而非合并。
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
	require.False(t, ids["a2"], "旧成员应被全量替换清除")
}

// ── SyncRecipeMoments:删除消失的时刻(级联清成员)────────────────────────

func TestMomentStore_SyncDeletesDisappearedMoments(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)
	insertMomentAsset(t, db, "a1")
	insertMomentAsset(t, db, "a2")

	// 同一 recipe 下先产出两个时刻。
	d1 := MomentDraft{Moment: Moment{ID: "m1", RecipeKey: "trip", Title: "Trip One", AssetCount: 1}, Assets: []MomentAsset{{AssetID: "a1"}}}
	d2 := MomentDraft{Moment: Moment{ID: "m2", RecipeKey: "trip", Title: "Trip Two", AssetCount: 1}, Assets: []MomentAsset{{AssetID: "a2"}}}
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{d1, d2}))

	moments, err := store.ListMoments()
	require.NoError(t, err)
	require.Len(t, moments, 2)

	// 另一个 recipe 下的时刻不应受影响。
	dOther := MomentDraft{Moment: Moment{ID: "m3", RecipeKey: "theme:pets", Title: "Pet Moments", AssetCount: 1}, Assets: []MomentAsset{{AssetID: "a1"}}}
	require.NoError(t, store.SyncRecipeMoments("theme:pets", []MomentDraft{dOther}))

	// 下一轮重算:trip 只产出 m1,m2 消失。
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{d1}))

	moments2, err := store.ListMoments()
	require.NoError(t, err)
	require.Len(t, moments2, 2, "m2 应被删除,m1 与其它 recipe 的 m3 应保留")
	idSet := map[string]bool{}
	for _, m := range moments2 {
		idSet[m.ID] = true
	}
	require.True(t, idSet["m1"])
	require.True(t, idSet["m3"])
	require.False(t, idSet["m2"], "消失的时刻应被删除")

	// m2 的成员应随之级联清理(不会成为孤儿 moment_assets)。
	var orphanCount int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM moment_assets WHERE moment_id='m2'`).Scan(&orphanCount))
	require.Equal(t, 0, orphanCount)
}

// ── SyncRecipeMoments:保留 LLM 已命名的 title ───────────────────────────

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

	// 下一轮重算:模板重新算出了不同的 title/subtitle,但 LLM 已命名过,应保留 title。
	draft2 := draft
	draft2.Title = "Yosemite Trip (Recomputed)"
	draft2.Subtitle = "May-June 2011"
	draft2.AssetCount = 5
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{draft2}))

	moments2, err := store.ListMoments()
	require.NoError(t, err)
	require.Equal(t, "An Amazing Yosemite Getaway", moments2[0].Title, "LLM 命名过的 title 不应被重算覆盖")
	require.True(t, moments2[0].NamedByLLM)
	require.Equal(t, "May-June 2011", moments2[0].Subtitle, "非 title 字段仍应正常更新")
	require.Equal(t, 5, moments2[0].AssetCount)
}

// ── id 稳定性 ────────────────────────────────────────────────────────────

func TestMomentStore_IDStability(t *testing.T) {
	// 同一 ISO 周内不同日期,应得到同一 trip 时刻 id(重算日期微移不换 id)。
	t1 := time.Date(2011, 5, 9, 0, 0, 0, 0, time.UTC)  // 2011-W19
	t2 := time.Date(2011, 5, 12, 0, 0, 0, 0, time.UTC) // 同一周
	require.Equal(t, TripMomentID("trip", t1), TripMomentID("trip", t2))

	t3 := time.Date(2011, 5, 20, 0, 0, 0, 0, time.UTC) // 下一周
	require.NotEqual(t, TripMomentID("trip", t1), TripMomentID("trip", t3), "跨周应换 id")

	require.NotEqual(t, TripMomentID("trip", t1), TripMomentID("trip2", t1), "recipe 不同应换 id")

	// theme 时刻 id 只取决于 recipe key,滚动更新恒定。
	require.Equal(t, ThemeMomentID("theme:pets"), ThemeMomentID("theme:pets"))
	require.NotEqual(t, ThemeMomentID("theme:pets"), ThemeMomentID("theme:food"))

	// id 应为 16 位十六进制字符串(sha1 前 16 hex)。
	require.Len(t, TripMomentID("trip", t1), 16)
	require.Len(t, ThemeMomentID("theme:pets"), 16)
}

// ── ListMoments 按 updated_at desc;GetMomentAssets featured 过滤 ───────

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
	require.Equal(t, "m2", moments[0].ID, "最近更新的应排在前面")
	require.Equal(t, "m1", moments[1].ID)

	all, err := store.GetMomentAssets("m2", false)
	require.NoError(t, err)
	require.Len(t, all, 2)

	featured, err := store.GetMomentAssets("m2", true)
	require.NoError(t, err)
	require.Len(t, featured, 1)
	require.Equal(t, "a2", featured[0].AssetID)
}

// ── ListMoments:手排序语义(sort_order 列)────────────────────────────────
//
// 三段语义(见设计 spec 第一节):
//  1. 手排序(sort_order 非 NULL)的排在最前面,按 sort_order 升序;
//  2. 未手排(sort_order 为 NULL)的排在手排序之后,按 updated_at 降序;
//  3. 全库都未手排时 = 现状不变(纯 updated_at 降序,向后兼容既有断言)。
func TestMomentStore_ListMomentsSortOrderSemantics(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)
	insertMomentAsset(t, db, "a1")
	insertMomentAsset(t, db, "a2")
	insertMomentAsset(t, db, "a3")

	// 依次产出 m1(最旧)→m2→m3(最新),updated_at 递增。
	d1 := MomentDraft{Moment: Moment{ID: "m1", RecipeKey: "trip", Title: "First", AssetCount: 1}, Assets: []MomentAsset{{AssetID: "a1"}}}
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{d1}))
	time.Sleep(2 * time.Millisecond)
	d2 := MomentDraft{Moment: Moment{ID: "m2", RecipeKey: "theme:pets", Title: "Second", AssetCount: 1}, Assets: []MomentAsset{{AssetID: "a2"}}}
	require.NoError(t, store.SyncRecipeMoments("theme:pets", []MomentDraft{d2}))
	time.Sleep(2 * time.Millisecond)
	d3 := MomentDraft{Moment: Moment{ID: "m3", RecipeKey: "theme:food", Title: "Third", AssetCount: 1}, Assets: []MomentAsset{{AssetID: "a3"}}}
	require.NoError(t, store.SyncRecipeMoments("theme:food", []MomentDraft{d3}))

	// 段 3:全库未手排 = 现状不变,按 updated_at DESC(最新的 m3 在最前)。
	all, err := store.ListMoments()
	require.NoError(t, err)
	require.Len(t, all, 3)
	require.Equal(t, []string{"m3", "m2", "m1"}, []string{all[0].ID, all[1].ID, all[2].ID})
	require.Nil(t, all[0].SortOrder, "未手排时 SortOrder 应为 nil(NULL 语义保真)")

	// 段 1+2:手排 m1、m2(m1 排在 m2 前面,即便 m1 updated_at 更旧),
	// m3 仍未手排。手排的应整体排在未手排(m3)前面,内部按用户给定顺序。
	require.NoError(t, store.ReorderMoments([]string{"m1", "m2"}))

	mixed, err := store.ListMoments()
	require.NoError(t, err)
	require.Len(t, mixed, 3)
	require.Equal(t, []string{"m1", "m2", "m3"}, []string{mixed[0].ID, mixed[1].ID, mixed[2].ID},
		"手排的 m1/m2 应排在未手排的 m3 前面,且内部按手排顺序")
	require.NotNil(t, mixed[0].SortOrder)
	require.Equal(t, 10, *mixed[0].SortOrder)
	require.NotNil(t, mixed[1].SortOrder)
	require.Equal(t, 20, *mixed[1].SortOrder)
	require.Nil(t, mixed[2].SortOrder, "m3 未手排,SortOrder 应仍为 nil")
}

// ── ReorderMoments:赋值与间隙 + 未知 id 忽略 ─────────────────────────────

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

	// 中间混入一个未知 id("ghost"不存在于 moments 表),应影响 0 行、不报错。
	require.NoError(t, store.ReorderMoments([]string{"m1", "ghost", "m2", "m3"}))

	moments, err := store.ListMoments()
	require.NoError(t, err)
	byID := map[string]Moment{}
	for _, m := range moments {
		byID[m.ID] = m
	}
	require.NotNil(t, byID["m1"].SortOrder)
	require.Equal(t, 10, *byID["m1"].SortOrder, "m1 是 ids[0],赋值 (0+1)*10=10")
	require.NotNil(t, byID["m2"].SortOrder)
	require.Equal(t, 30, *byID["m2"].SortOrder, "m2 是 ids[2](ghost 占了 index 1),赋值 (2+1)*10=30")
	require.NotNil(t, byID["m3"].SortOrder)
	require.Equal(t, 40, *byID["m3"].SortOrder, "m3 是 ids[3],赋值 (3+1)*10=40")
}

// ── SyncRecipeMoments:重算不触碰已手排的 sort_order(幸存)─────────────────

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

	// 下一轮重算(同 id upsert),sort_order 不应被触碰。
	draft2 := draft
	draft2.Title = "Yosemite Trip (Recomputed)"
	draft2.AssetCount = 5
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{draft2}))

	moments2, err := store.ListMoments()
	require.NoError(t, err)
	require.Len(t, moments2, 1)
	require.NotNil(t, moments2[0].SortOrder, "Sync upsert 不应清空已手排的 sort_order")
	require.Equal(t, 10, *moments2[0].SortOrder)
	require.Equal(t, "Yosemite Trip (Recomputed)", moments2[0].Title)
}

// ── 可编辑时刻:moment_edits 迁移幂等 ─────────────────────────────────────

// TestMomentStore_MigrationIdempotent 反复打开(=反复迁移)同一个库文件三次,
// 确认 moments.hidden / moment_assets.manual 两列的幂等加列与 moment_edits
// 建表都不会在重复迁移时报错(如 "duplicate column"/"table already exists")。
func TestMomentStore_MigrationIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "migrate.db")

	for i := 0; i < 3; i++ {
		db, err := sqlite.Open(path)
		require.NoError(t, err, "第 %d 次迁移不应报错", i+1)

		// 校验新增列/新表确实存在,而不仅仅是"没报错"。
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
		require.True(t, hiddenCol, "moments.hidden 列应存在")

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
		require.True(t, manualCol, "moment_assets.manual 列应存在")

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
		require.True(t, addedAtCol, "moment_assets.added_at 列应存在")

		var tblCount int
		require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='moment_edits'`).Scan(&tblCount))
		require.Equal(t, 1, tblCount, "moment_edits 表应存在")

		require.NoError(t, db.Close())
	}
}

// ── 可编辑时刻:pin 幸存重算 ─────────────────────────────────────────────

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

	// 用户强行把 a2(引擎本轮未纳入)钉入。
	count, err := store.PinMomentAssets("m1", []string{"a2"})
	require.NoError(t, err)
	require.Equal(t, 2, count)

	// 下一轮重算:引擎依旧只产出 a1,但 a2 应因 edits 回放而幸存。
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{draft}))

	members, err := store.GetMomentAssets("m1", false)
	require.NoError(t, err)
	ids := map[string]MomentAsset{}
	for _, m := range members {
		ids[m.AssetID] = m
	}
	require.Contains(t, ids, "a1")
	require.Contains(t, ids, "a2", "pin 应在重算后依然存活")
	require.True(t, ids["a2"].Manual, "回放插入的成员应标记 manual=1")
}

// ── 可编辑时刻:pin 不降级引擎已纳入成员(INSERT OR IGNORE 语义)──────────

func TestMomentStore_PinDoesNotDowngradeExistingEngineMember(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)
	insertMomentAsset(t, db, "a1")

	// a1 已被引擎本轮纳入为精选成员(featured=1, score>0)。
	draft := MomentDraft{
		Moment: Moment{ID: "m1", RecipeKey: "trip", Title: "Trip", AssetCount: 1},
		Assets: []MomentAsset{{AssetID: "a1", Featured: true, Score: 0.9}},
	}
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{draft}))

	// 对同一 asset 调用 PinMomentAssets:INSERT OR IGNORE 不应把已有行降级
	// 覆盖为 manual 插入的 featured=0/score=0。
	count, err := store.PinMomentAssets("m1", []string{"a1"})
	require.NoError(t, err)
	require.Equal(t, 1, count)

	members, err := store.GetMomentAssets("m1", false)
	require.NoError(t, err)
	require.Len(t, members, 1)
	require.True(t, members[0].Featured, "pin 已是引擎成员的 asset 不应把 featured 降级为 0")
	require.Equal(t, 0.9, members[0].Score, "pin 已是引擎成员的 asset 不应把 score 清零")

	// moment_edits 应留下 pin 记录。
	pins, excludes, err := store.MomentEditsFor("m1")
	require.NoError(t, err)
	require.Equal(t, []string{"a1"}, pins)
	require.Empty(t, excludes)

	// 下一轮重算(引擎依旧纳入 a1)回放后依然不应降级。
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{draft}))
	members2, err := store.GetMomentAssets("m1", false)
	require.NoError(t, err)
	require.Len(t, members2, 1)
	require.True(t, members2[0].Featured, "重算回放后仍不应降级 featured")
	require.Equal(t, 0.9, members2[0].Score, "重算回放后仍不应降级 score")
}

// ── 可编辑时刻:exclude 幸存重算 ─────────────────────────────────────────

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

	// 下一轮重算:引擎依旧产出 a1+a2,但 a2 应因 exclude 回放而被剔除。
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{draft}))

	members, err := store.GetMomentAssets("m1", false)
	require.NoError(t, err)
	require.Len(t, members, 1)
	require.Equal(t, "a1", members[0].AssetID)
}

// ── 可编辑时刻:pin 覆盖 exclude(同一 asset 后写的编辑生效)──────────────

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

	// 先排除 a2,再反悔改成钉入——后写的编辑(pin)应覆盖先写的(exclude)。
	_, err := store.ExcludeMomentAssets("m1", []string{"a2"})
	require.NoError(t, err)
	count, err := store.PinMomentAssets("m1", []string{"a2"})
	require.NoError(t, err)
	require.Equal(t, 2, count)

	pins, excludes, err := store.MomentEditsFor("m1")
	require.NoError(t, err)
	require.Equal(t, []string{"a2"}, pins)
	require.Empty(t, excludes, "pin 应覆盖此前的 exclude 记录,而非并存")

	// 重算后 a2 应作为成员留存(pin 生效,而非被 exclude 剔除)。
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{draft}))
	members, err := store.GetMomentAssets("m1", false)
	require.NoError(t, err)
	ids := map[string]bool{}
	for _, m := range members {
		ids[m.AssetID] = true
	}
	require.True(t, ids["a2"])
}

// ── 可编辑时刻:主题类时刻时间窗免疫(TimeFrom/TimeTo 恒为 NULL)────────────

func TestMomentStore_ThemeMomentTimeWindowImmuneToEditRecompute(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)
	insertMomentAssetAt(t, db, "a1", time.Date(2011, 5, 10, 0, 0, 0, 0, time.UTC))
	insertMomentAssetAt(t, db, "a2", time.Date(2011, 6, 1, 0, 0, 0, 0, time.UTC))

	// theme 类草稿:TimeFrom/TimeTo 保持零值(不设置),落库应为 NULL。
	draft := MomentDraft{
		Moment: Moment{ID: "m1", RecipeKey: "theme:pets", Title: "Pets", AssetCount: 1},
		Assets: []MomentAsset{{AssetID: "a1", Featured: true, Score: 0.9}},
	}
	require.NoError(t, store.SyncRecipeMoments("theme:pets", []MomentDraft{draft}))

	moments, err := store.ListMoments()
	require.NoError(t, err)
	require.Len(t, moments, 1)
	require.True(t, moments[0].TimeFrom.IsZero(), "theme 时刻初始时间窗应为零值(NULL)")
	require.True(t, moments[0].TimeTo.IsZero())

	// Pin 一个带 taken_at 的资产——若时间窗被误刷,会被撑成非零值。
	_, err = store.PinMomentAssets("m1", []string{"a2"})
	require.NoError(t, err)
	moments, err = store.ListMoments()
	require.NoError(t, err)
	require.True(t, moments[0].TimeFrom.IsZero(), "pin 后 theme 时刻时间窗仍应保持 NULL")
	require.True(t, moments[0].TimeTo.IsZero())

	// Exclude 现有成员,同样应免疫。
	_, err = store.ExcludeMomentAssets("m1", []string{"a1"})
	require.NoError(t, err)
	moments, err = store.ListMoments()
	require.NoError(t, err)
	require.True(t, moments[0].TimeFrom.IsZero(), "exclude 后 theme 时刻时间窗仍应保持 NULL")
	require.True(t, moments[0].TimeTo.IsZero())

	// 再触发一轮带 edits 回放的 SyncRecipeMoments(hasEdits=true 会进入
	// refreshMomentDerived,须确认 hadTimeWindow 判定继续为 false)。
	require.NoError(t, store.SyncRecipeMoments("theme:pets", []MomentDraft{draft}))
	moments, err = store.ListMoments()
	require.NoError(t, err)
	require.True(t, moments[0].TimeFrom.IsZero(), "带 edits 回放的重算后 theme 时刻时间窗仍应保持 NULL")
	require.True(t, moments[0].TimeTo.IsZero())
}

// ── 可编辑时刻:派生刷新(count + 时间窗 + 封面重挑)──────────────────────

func TestMomentStore_DerivedRefreshOnEdit(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)
	tFrom := time.Date(2011, 5, 10, 0, 0, 0, 0, time.UTC)
	tMid := time.Date(2011, 5, 12, 0, 0, 0, 0, time.UTC)
	tLate := time.Date(2011, 5, 20, 0, 0, 0, 0, time.UTC) // 排除之外的时间点,pin 后应把时间窗撑宽
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

	// pin a3(时间在窗口外)——count 应变 3,时间窗右端应扩到 a3 的 taken_at。
	count, err := store.PinMomentAssets("m1", []string{"a3"})
	require.NoError(t, err)
	require.Equal(t, 3, count)

	moments, err := store.ListMoments()
	require.NoError(t, err)
	require.Len(t, moments, 1)
	require.Equal(t, 3, moments[0].AssetCount)
	require.True(t, moments[0].TimeTo.Equal(tLate), "pin 应触发时间窗按新成员集合重算")
	require.Equal(t, "a1", moments[0].CoverAssetID, "cover 仍是成员,不应被重挑")

	// 排除当前封面 a1——封面应重挑为 featured 中分数最高的剩余成员(此处无
	// 其余 featured 成员,应回落"任一成员"档:按 score DESC, asset_id 确定序。
	count2, err := store.ExcludeMomentAssets("m1", []string{"a1"})
	require.NoError(t, err)
	require.Equal(t, 2, count2)

	moments2, err := store.ListMoments()
	require.NoError(t, err)
	require.Equal(t, 2, moments2[0].AssetCount)
	require.NotEqual(t, "a1", moments2[0].CoverAssetID, "旧封面已被剔除,不应继续挂着")
	require.Contains(t, []string{"a2", "a3"}, moments2[0].CoverAssetID)
	require.Equal(t, "a2", moments2[0].CoverAssetID, "无 featured 候选时回落任一成员,按 score DESC 取第一(a2=0.5>a3=0)")
}

// ── 可编辑时刻:成员清空允许 count=0(全排除后不报错、封面回落 NULL)──────

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
	require.Equal(t, 0, count, "成员清空应允许 count=0,不报错")

	moments, err := store.ListMoments()
	require.NoError(t, err)
	require.Equal(t, 0, moments[0].AssetCount)
	require.Equal(t, "", moments[0].CoverAssetID, "无成员时封面应回落 NULL/空")
}

// ── 可编辑时刻:hidden tombstone——upsert 保留 + ListMoments 过滤 ─────────

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

	// hidden 后 ListMoments 应过滤掉该时刻。
	moments, err := store.ListMoments()
	require.NoError(t, err)
	require.Len(t, moments, 0)

	// 重算(同 id upsert)不应把 hidden 重置为 0——upsert 列清单不含 hidden,
	// 与 named_by_llm 同法自然保留。
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{draft}))
	var hidden int
	require.NoError(t, db.QueryRow(`SELECT hidden FROM moments WHERE id=?`, "m1").Scan(&hidden))
	require.Equal(t, 1, hidden, "重算不应清除 hidden tombstone")

	moments2, err := store.ListMoments()
	require.NoError(t, err)
	require.Len(t, moments2, 0, "重算后仍应被 ListMoments 过滤")
}

// ── 可编辑时刻:PinMomentAssets 立即生效与未知 id 忽略 ──────────────────

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

	// 混入一个 assets 表里不存在的 id,应静默忽略,不报错、不影响已知 id。
	count, err := store.PinMomentAssets("m1", []string{"a2", "ghost-asset"})
	require.NoError(t, err)
	require.Equal(t, 2, count, "未知 id 应被忽略,只有 a2 生效")

	// 立即生效:无需等待下一轮 Sync,GetMomentAssets 应马上看到 a2。
	members, err := store.GetMomentAssets("m1", false)
	require.NoError(t, err)
	ids := map[string]bool{}
	for _, m := range members {
		ids[m.AssetID] = true
	}
	require.True(t, ids["a1"])
	require.True(t, ids["a2"])
	require.False(t, ids["ghost-asset"])

	// 未知 id 也不应留下 moment_edits 记录。
	pins, _, err := store.MomentEditsFor("m1")
	require.NoError(t, err)
	require.Equal(t, []string{"a2"}, pins)
}

// ── 可编辑时刻:MomentEditsFor 形状 ──────────────────────────────────────

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

	// 没有任何编辑记录的 moment 应返回空切片,不报错。
	pins2, excludes2, err := store.MomentEditsFor("no-such-moment")
	require.NoError(t, err)
	require.Empty(t, pins2)
	require.Empty(t, excludes2)
}

// ── 可编辑时刻:TopFeaturedByMoment 形状(非封面、score 序、≤N)──────────

func TestMomentStore_TopFeaturedByMoment(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)
	for _, id := range []string{"a1", "a2", "a3", "a4", "b1", "b2"} {
		insertMomentAsset(t, db, id)
	}

	d1 := MomentDraft{
		Moment: Moment{ID: "m1", RecipeKey: "trip", Title: "Trip One", AssetCount: 4, CoverAssetID: "a1"},
		Assets: []MomentAsset{
			{AssetID: "a1", Featured: true, Score: 0.95}, // 封面,应被排除
			{AssetID: "a2", Featured: true, Score: 0.9},
			{AssetID: "a3", Featured: true, Score: 0.8},
			{AssetID: "a4", Featured: false, Score: 0.99}, // 非 featured,不应出现
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
	require.Equal(t, []string{"a2"}, top["m1"], "封面 a1 应被排除,取剩余 featured 中分数最高的一个")
	require.Equal(t, []string{"b1"}, top["m2"])

	top2, err := store.TopFeaturedByMoment(2)
	require.NoError(t, err)
	require.Equal(t, []string{"a2", "a3"}, top2["m1"], "按 score DESC,封面之外前 2 个")
	require.Equal(t, []string{"b1", "b2"}, top2["m2"])
}

// ── CoverRatioByMoment:封面宽高比(正常/缺 exif 行/零值→0)────────────

// insertMomentAssetWithExif 插入一条带 asset_exif(width/height)的资产,
// 供 CoverRatioByMoment 测试用。
func insertMomentAssetWithExif(t *testing.T, db *sql.DB, id string, width, height int) {
	t.Helper()
	insertMomentAsset(t, db, id)
	_, err := db.Exec(`INSERT INTO asset_exif(asset_id, width, height) VALUES (?, ?, ?)`, id, width, height)
	require.NoError(t, err)
}

func TestMomentStore_CoverRatioByMoment_Normal(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)
	insertMomentAssetWithExif(t, db, "a1", 1600, 2000) // 竖版封面,ratio=0.8

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
	insertMomentAsset(t, db, "a1") // 无 asset_exif 行(未索引到尺寸)

	draft := MomentDraft{
		Moment: Moment{ID: "m1", RecipeKey: "trip", Title: "Trip", AssetCount: 1, CoverAssetID: "a1"},
		Assets: []MomentAsset{{AssetID: "a1", Score: 0.5}},
	}
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{draft}))

	ratios, err := store.CoverRatioByMoment()
	require.NoError(t, err)
	_, ok := ratios["m1"]
	require.False(t, ok, "缺 asset_exif 行的 moment 不应出现在 map 里,调用方应按 0 处理")
}

func TestMomentStore_CoverRatioByMoment_ZeroWidthOrHeightExcluded(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)
	insertMomentAssetWithExif(t, db, "a1", 0, 2000)    // width=0
	insertMomentAssetWithExif(t, db, "a2", 1600, 0)    // height=0
	insertMomentAssetWithExif(t, db, "a3", 1600, 1200) // 正常,ratio=1.333...

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
	require.False(t, ok1, "width=0 不应出现在 map 里")
	_, ok2 := ratios["m2"]
	require.False(t, ok2, "height=0 不应出现在 map 里")
	require.InDelta(t, 1600.0/1200.0, ratios["m3"], 1e-9)
}

// ── diff 式 upsert:added_at 语义(spec 1.2)───────────────────────────────

// momentAssetAddedAt 读回 moment_assets.added_at(NULL 时返回 0,与
// MomentAsset.AddedAt 的 0=NULL 约定一致),供本组测试断言用。
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

// TestMomentStore_SyncDiffUpsertNewMemberGetsAddedAtNow:首次 Sync 产出的新
// 成员应打上当前时间戳(非 NULL)。
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
	require.GreaterOrEqual(t, addedAt, before, "新成员 added_at 应打当前时间戳")
	require.LessOrEqual(t, addedAt, after)
}

// TestMomentStore_SyncDiffUpsertExistingMemberAddedAtUnchanged:同一成员跨轮
// Sync(冲突分支)不应触碰已有的 added_at,即便 featured/score 变化。
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
	require.Equal(t, firstAddedAt, secondAddedAt, "冲突分支不应触碰既有成员的 added_at")

	// featured/score 仍应正常更新(diff upsert 语义等价于原全量替换)。
	members, err := store.GetMomentAssets("m1", false)
	require.NoError(t, err)
	require.Len(t, members, 1)
	require.True(t, members[0].Featured)
	require.Equal(t, 0.9, members[0].Score)
}

// TestMomentStore_SyncDiffUpsertDisappearedMemberDeleted:引擎本轮未产出的
// 旧成员(非 pin)应被删除(diff upsert 的第 2 步),重新出现时视为全新成员
// (added_at 重新打 now)。
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

	// 下一轮只产出 a1,a2 消失(非 pin)——应被删除,不是仅仅"不更新"。
	draft2 := draft
	draft2.Assets = []MomentAsset{{AssetID: "a1", Score: 0.5}}
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{draft2}))

	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM moment_assets WHERE moment_id='m1' AND asset_id='a2'`).Scan(&count))
	require.Equal(t, 0, count, "消失的非 pin 成员应被 diff upsert 删除")
}

// TestMomentStore_SyncDiffUpsertPinExemptFromDeletionAddedAtStable:pin 成员
// 即便引擎本轮未产出也不应被删除(spec 点名的"假新鲜坑"),连续两轮 Sync 后
// added_at 应保持稳定(不因"删了又被回放补插"而每轮刷新成 now)。
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

	// 用户钉入 a2(引擎本轮未纳入)。
	_, err := store.PinMomentAssets("m1", []string{"a2"})
	require.NoError(t, err)
	pinnedAddedAt := momentAssetAddedAt(t, db, "m1", "a2")
	require.NotZero(t, pinnedAddedAt, "PinMomentAssets 应补 added_at=now")

	// 连续两轮重算,引擎依旧只产出 a1,a2 应始终幸存且 added_at 不变。
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{draft}))
	afterRound1 := momentAssetAddedAt(t, db, "m1", "a2")
	require.Equal(t, pinnedAddedAt, afterRound1, "pin 成员豁免删除,第一轮重算后 added_at 不应刷新")

	time.Sleep(2 * time.Millisecond)
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{draft}))
	afterRound2 := momentAssetAddedAt(t, db, "m1", "a2")
	require.Equal(t, pinnedAddedAt, afterRound2, "第二轮重算后 added_at 仍不应刷新(假新鲜坑回归)")

	members, err := store.GetMomentAssets("m1", false)
	require.NoError(t, err)
	ids := map[string]bool{}
	for _, m := range members {
		ids[m.AssetID] = true
	}
	require.True(t, ids["a1"])
	require.True(t, ids["a2"], "pin 成员应在两轮重算后依然存活")
}

// ── PinMomentAssets 立即插入路径补 added_at=now ─────────────────────────

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

// ── 债务清扫:pin 回放"活资产"口径对齐 ───────────────────────────────────
//
// 活资产口径与 moments_theme.go loadThemeCandidatePool 同源:
// status='indexed' AND deleted_at IS NULL AND offline=0。此前 pin 相关三处
// (立即插入/diff upsert 删除豁免/回放补插)只校验 assets 表存在性,不认
// 活资产口径,导致"pin 的照片进回收站后依然赖在时刻里不走"的分叉。

// momentUpdatedAt 读回 moments.updated_at,供"假刷新"回归测试断言。
func momentUpdatedAt(t *testing.T, db *sql.DB, momentID string) int64 {
	t.Helper()
	var v int64
	require.NoError(t, db.QueryRow(`SELECT updated_at FROM moments WHERE id=?`, momentID).Scan(&v))
	return v
}

// TestMomentStore_PinMomentAssetsIgnoresDeadAssetImmediateInsert:对一个已在
// 回收站(deleted_at 非 NULL)的资产直接 PinMomentAssets——assets 表里 id
// 存在,故 edits 记录仍应写入(供日后从回收站还原时自动归队),但立即插入
// 成员这一步应被"活资产"口径拦下,不应马上把死资产计入成员/count。
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
	require.Equal(t, 1, count, "回收站资产不应被立即计入 count")

	members, err := store.GetMomentAssets("m1", false)
	require.NoError(t, err)
	for _, m := range members {
		require.NotEqual(t, "a2", m.AssetID, "回收站资产不应被立即插入成员")
	}

	pins, _, err := store.MomentEditsFor("m1")
	require.NoError(t, err)
	require.Contains(t, pins, "a2", "edits 记录应保留,供恢复后自动归队")
}

// TestMomentStore_PinReplayRemovesDeadAssetAndRejoinsOnRestore:pin 一个活资产
// 幸存重算之后,该资产进回收站(deleted_at 置位)——下一轮 Sync 不应再豁免
// 它的删除,成员应被移除、count 同步下降;从回收站还原(deleted_at 清空)后
// 再 Sync,应自动归队(edits 行全程保留,不因资产死而丢失 pin 意图)。
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

	// 用户钉入 a2(引擎本轮未纳入),此时 a2 仍是活资产,立即生效。
	count, err := store.PinMomentAssets("m1", []string{"a2"})
	require.NoError(t, err)
	require.Equal(t, 2, count)

	// a2 进回收站。
	_, err = db.Exec(`UPDATE assets SET deleted_at=? WHERE id=?`, "2020-01-01 00:00:00", "a2")
	require.NoError(t, err)

	// 下一轮重算:a2 已非活资产,pin 不再豁免其被删除,应被 diff upsert 移除。
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{draft}))
	members, err := store.GetMomentAssets("m1", false)
	require.NoError(t, err)
	ids := map[string]bool{}
	for _, m := range members {
		ids[m.AssetID] = true
	}
	require.True(t, ids["a1"])
	require.False(t, ids["a2"], "进回收站的 pin 资产下一轮重算应被移除")

	moments, err := store.ListMoments()
	require.NoError(t, err)
	require.Equal(t, 1, moments[0].AssetCount, "count 应随死资产移除同步下降")

	// edits 记录应全程保留(未被清除),供恢复后继续生效。
	pinsBeforeRestore, _, err := store.MomentEditsFor("m1")
	require.NoError(t, err)
	require.Contains(t, pinsBeforeRestore, "a2", "pin 意图应在资产死亡期间保留")

	// 从回收站还原。
	_, err = db.Exec(`UPDATE assets SET deleted_at=NULL WHERE id=?`, "a2")
	require.NoError(t, err)

	// 下一轮重算:a2 恢复为活资产,pin 回放应自动把它补插回成员。
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{draft}))
	members2, err := store.GetMomentAssets("m1", false)
	require.NoError(t, err)
	ids2 := map[string]bool{}
	for _, m := range members2 {
		ids2[m.AssetID] = true
	}
	require.True(t, ids2["a2"], "从回收站还原后应自动归队")

	moments2, err := store.ListMoments()
	require.NoError(t, err)
	require.Equal(t, 2, moments2[0].AssetCount, "归队后 count 应恢复")
}

// ── 债务清扫:全未知 ids 假刷新修复 ─────────────────────────────────────
//
// 此前 applyMomentEditOp 无论本次调用是否造成任何成员行变化,都会走
// refreshMomentDerived + 刷新 updated_at,导致"全传未知 id"这种空操作也把
// 时刻顶到 ListMoments 排序前端(八期终审点名)。改为统计本次实际受影响的
// 成员行数(pin 的 INSERT 生效数/exclude 的 DELETE 生效数),为 0 时跳过
// 派生刷新,直接返回当前 asset_count,updated_at 保持不变。

func TestMomentStore_EditOpAllUnknownIDsSkipsFakeRefresh(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)
	insertMomentAsset(t, db, "a1")
	insertMomentAsset(t, db, "b1")

	// 用两个不同 recipe_key 分别落 m1/m2:SyncRecipeMoments 会清掉同一
	// recipe_key 下本轮未产出的旧时刻,同 recipe_key 复用会把 m1 误删。
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

	// 两者均未手排(sort_order NULL),按 updated_at DESC:m2(更新)在前。
	moments, err := store.ListMoments()
	require.NoError(t, err)
	require.Equal(t, "m2", moments[0].ID)
	require.Equal(t, "m1", moments[1].ID)

	beforeUpdatedAt := momentUpdatedAt(t, db, "m1")

	// 对 m1 用全未知 id 调 Pin:assets 表里都不存在,静默忽略,成员行数应
	// 无任何变化。
	count, err := store.PinMomentAssets("m1", []string{"ghost1", "ghost2"})
	require.NoError(t, err)
	require.Equal(t, 1, count, "全未知 ids 不应改变 count")

	afterUpdatedAt := momentUpdatedAt(t, db, "m1")
	require.Equal(t, beforeUpdatedAt, afterUpdatedAt, "全未知 ids 不应刷新 updated_at(假刷新回归)")

	moments2, err := store.ListMoments()
	require.NoError(t, err)
	require.Equal(t, "m2", moments2[0].ID, "排序位置不应因假刷新而改变")
	require.Equal(t, "m1", moments2[1].ID)

	// ExcludeMomentAssets 同理:全未知 id 同样不应刷新 updated_at。
	count2, err := store.ExcludeMomentAssets("m1", []string{"ghost3"})
	require.NoError(t, err)
	require.Equal(t, 1, count2)
	require.Equal(t, beforeUpdatedAt, momentUpdatedAt(t, db, "m1"), "exclude 全未知 ids 同样不应刷新 updated_at")
}

// ── 债务清扫:补两条便宜测试 ─────────────────────────────────────────────

// TestMomentStore_SyncRecipeMomentsEmptyDraftAssetsDeletesAllNonPinMembers:
// draft 的成员切片为空(引擎本轮判定该 moment 已无任何成员)时,diff upsert
// 应删除全部非 pin 成员(pin 成员依旧豁免,覆盖九期终审点名的"空成员 draft"
// 边界,此前无显式单测)。
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

	// 用户钉入一个引擎本轮以外的资产。
	insertMomentAsset(t, db, "a3")
	_, err := store.PinMomentAssets("m1", []string{"a3"})
	require.NoError(t, err)

	// 下一轮:draft 成员切片为空(引擎判定该 moment 已无成员)。
	emptyDraft := draft
	emptyDraft.Assets = nil
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{emptyDraft}))

	members, err := store.GetMomentAssets("m1", false)
	require.NoError(t, err)
	ids := map[string]bool{}
	for _, m := range members {
		ids[m.AssetID] = true
	}
	require.False(t, ids["a1"], "空 draft 应删除非 pin 旧成员")
	require.False(t, ids["a2"], "空 draft 应删除非 pin 旧成员")
	require.True(t, ids["a3"], "pin 成员即便 draft 成员为空也应豁免删除")
	require.Len(t, members, 1)
}

// ── AddedThisWeekByMoment:口径(NULL 不计/7 天窗/无 N+1 形状)────────────

func TestMomentStore_AddedThisWeekByMoment_NullNotCountedAndSevenDayWindow(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)
	insertMomentAsset(t, db, "a1") // 存量:added_at 手动置 NULL
	insertMomentAsset(t, db, "a2") // 本周新增
	insertMomentAsset(t, db, "a3") // 8 天前新增,窗口外
	insertMomentAsset(t, db, "a4") // 恰好窗口边界内(now-7d 之后)

	draft := MomentDraft{
		Moment: Moment{ID: "m1", RecipeKey: "trip", Title: "Trip", AssetCount: 4},
		Assets: []MomentAsset{
			{AssetID: "a1"}, {AssetID: "a2"}, {AssetID: "a3"}, {AssetID: "a4"},
		},
	}
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{draft}))

	now := nowMs()
	sevenDaysMs := int64(7 * 24 * 60 * 60 * 1000)

	// a1 模拟存量:回填为 NULL(diff upsert 首次 INSERT 打了 now,这里手动覆盖
	// 模拟"升级前已存在、加入时间未知"的场景)。
	_, err := db.Exec(`UPDATE moment_assets SET added_at=NULL WHERE moment_id='m1' AND asset_id='a1'`)
	require.NoError(t, err)
	// a2:窗口内(2 天前)。
	_, err = db.Exec(`UPDATE moment_assets SET added_at=? WHERE moment_id='m1' AND asset_id='a2'`,
		now-2*24*60*60*1000)
	require.NoError(t, err)
	// a3:窗口外(8 天前)。
	_, err = db.Exec(`UPDATE moment_assets SET added_at=? WHERE moment_id='m1' AND asset_id='a3'`,
		now-8*24*60*60*1000)
	require.NoError(t, err)
	// a4:恰好等于窗口边界(now-7d),边界应计入(>=）。
	_, err = db.Exec(`UPDATE moment_assets SET added_at=? WHERE moment_id='m1' AND asset_id='a4'`,
		now-sevenDaysMs)
	require.NoError(t, err)

	counts, err := store.AddedThisWeekByMoment(now)
	require.NoError(t, err)
	require.Equal(t, 2, counts["m1"], "只有 a2/a4 落在 7 天窗口内且非 NULL,a1(NULL)与 a3(窗口外)不计")
}

// TestMomentStore_AddedThisWeekByMoment_MultiMomentGrouping 用一次查询覆盖
// 多个 moment(无 N+1 的形状验证:同一次调用应正确分组到各自 moment_id)。
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
	require.Equal(t, 2, counts["m1"], "m1 有 a1/a2 两个本周新增成员")
	require.Equal(t, 1, counts["m2"])
}

// ── PlacesByMoment:排序/tie-break/上限/无 geo 回退 ──────────────────────

// insertMomentAssetWithGeo 插入一条带 asset_geo 的资产,供 PlacesByMoment 测试用。
func insertMomentAssetWithGeo(t *testing.T, db *sql.DB, id, city string) {
	t.Helper()
	insertMomentAsset(t, db, id)
	_, err := db.Exec(`INSERT INTO asset_geo(asset_id, city) VALUES (?, ?)`, id, city)
	require.NoError(t, err)
}

func TestMomentStore_PlacesByMoment_OrderAndTieBreak(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)
	// Bozeman ×3、Rexburg ×1、Twin Falls ×1(平局按 city ASC 排)。
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
	}, places, "按 count DESC,同 count 按 city ASC tie-break")
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
	require.Len(t, places, 8, "10 个不同城市,上限应截到 8 条")
}

func TestMomentStore_PlacesByMoment_NoGeoExcludedAndEmptyReturnsEmpty(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)
	insertMomentAsset(t, db, "a1") // 无 asset_geo 行
	_, err := db.Exec(`INSERT INTO asset_geo(asset_id, city) VALUES (?, ?)`, "a1", "")
	require.NoError(t, err)        // city 为空字符串同样不应计入
	insertMomentAsset(t, db, "a2") // 完全无 asset_geo 行(未 geocode)

	draft := MomentDraft{
		Moment: Moment{ID: "m1", RecipeKey: "trip", Title: "Trip", AssetCount: 2},
		Assets: []MomentAsset{{AssetID: "a1"}, {AssetID: "a2"}},
	}
	require.NoError(t, store.SyncRecipeMoments("trip", []MomentDraft{draft}))

	places, err := store.PlacesByMoment("m1", 8)
	require.NoError(t, err)
	require.Empty(t, places, "city 为空或无 geo 行的成员都不应计入 places")
}
