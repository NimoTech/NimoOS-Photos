// MomentStore 的测试:三表(moment_recipes/moments/moment_assets)+ repo 层语义。
// 覆盖简报 Step 1 清单:seed 幂等且不覆盖已推送 recipe、UpsertRecipes 热更、
// SyncRecipeMoments 的 upsert/成员替换/删除消失时刻/保留 LLM title 四语义、
// id 稳定性同周同 id、ParseParams 默认值。
package service

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// insertMomentAsset 插入一条 moment_assets 会外键引用到的资产行(与
// captionpull_test.go 的 insertCaptionAsset 同法,id 存在即可)。
func insertMomentAsset(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO assets(id, file_path, status) VALUES(?,?,'indexed')`, id, "/g/"+id+".jpg")
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

// ── SeedDefaultRecipes:幂等 + 不覆盖已推送 recipe ──────────────────────

func TestMomentStore_SeedIdempotentAndDoesNotOverwritePushed(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)

	require.NoError(t, store.SeedDefaultRecipes())
	recipes, err := store.ListRecipes(false)
	require.NoError(t, err)
	require.Len(t, recipes, 6, "内置集:trip + 5 个 theme")

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
	require.Equal(t, "theme", keys["theme:pets"].Kind)
	require.Equal(t, "trip", keys["trip"].Kind)

	// 再次 seed 应保持幂等,不报错、不重复。
	require.NoError(t, store.SeedDefaultRecipes())
	recipes2, err := store.ListRecipes(false)
	require.NoError(t, err)
	require.Len(t, recipes2, 6)

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
			ID:        "m1",
			RecipeKey: "trip",
			Title:     "Yosemite Trip",
			Subtitle:  "May 2011 · Yosemite",
			Place:     "Yosemite",
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
