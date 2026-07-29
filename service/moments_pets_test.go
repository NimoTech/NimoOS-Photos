// pet 实体挖掘 + 实体时刻引擎测试:覆盖简报 Step 1 清单——beagle 14 张跨 2
// 月达标产实体、labrador 1 张不达标、短语词 "boxer dog" 命中且裸 "boxer"
// 文本不命中;draft title/subtitle 格式(同年/跨年)/成员并集(词命中 ∪
// CLIP fake);无达标实体清空画像。替换规则(theme:pets 双向)的用例追加在
// moments_test.go(需要完整 RecomputeAll 装配)。
//
// pet 实体挖掘消费 pin/exclude 反馈(Task 3):exclude 收窄 first/last
// seen、exclude 跌破 min_photos 门槛后实体从挖掘输出消失、pin 并入匹配集
// 增加计数/延展跨度、pin 命中但不在候选池(如已回收站/离线)的资产不计入
// 统计。用例见 "── MinePetEntities:消费 pin/exclude 反馈" 分组。
package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// petTestRecipe 拼一个 kind=pet_entities 的测试 recipe,复用 moments_theme_test.go
// 定义的 insertThemeAsset/insertCaption 辅助函数与 fakeThemeSearcher。
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

// ── MinePetEntities:达标线 + 短语词边界 ──────────────────────────────────

func TestMinePetEntities_ThresholdsAndPhraseBoundary(t *testing.T) {
	db := makeTestDB(t)

	month1 := time.Date(2011, time.August, 1, 12, 0, 0, 0, time.UTC)
	month2 := time.Date(2026, time.July, 1, 12, 0, 0, 0, time.UTC)

	// beagle:14 张跨 2 个月,应达标(默认门槛 8 张/2 月,这里显式传 8/2)。
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

	// labrador:仅 1 张,不达标。
	insertThemeAsset(t, db, "lab1", month1)
	insertCaption(t, db, "lab1", "a labrador retrieving a ball")

	// "boxer dog":8 张跨 2 个月的短语命中(达标),外加 1 张裸 "boxer"
	// (caption 里没有 "dog")不应计入该实体张数——词边界匹配的是整短语,不是
	// 短语里的单个词。
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

	require.NotContains(t, byKey, "labrador", "1 张不足以达标(min_photos=8)")

	require.Contains(t, byKey, "boxer dog")
	require.Equal(t, 8, byKey["boxer dog"].PhotoCount, "裸 'boxer'(无 'dog')不应计入短语实体张数")
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

// ── MinePetEntities:消费 pin/exclude 反馈 ────────────────────────────────

// seedStubMoment 直接插入一条最简 moments 行,只为满足 moment_edits 对
// moments(id) 的外键约束——测试直接调用 store.Pin/ExcludeMomentAssets 写
// 编辑记录,不经过完整 BuildPetEntityMoments/SyncRecipeMoments 装配流程。
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

	// 用户认定最早(beagle-a0)与最晚(beagle-b6)两张不是自己的狗,排除。
	_, err := store.ExcludeMomentAssets(momentID, []string{"beagle-a0", "beagle-b6"})
	require.NoError(t, err)

	entities, err := MinePetEntities(context.Background(), db, store, recipe)
	require.NoError(t, err)
	require.Len(t, entities, 1)

	e := entities[0]
	require.Equal(t, 12, e.PhotoCount, "14 张排除 2 张后应为 12")
	require.True(t, e.FirstSeen.Equal(month1.AddDate(0, 0, 1)), "首张应收窄到 beagle-a1")
	require.True(t, e.LastSeen.Equal(month2.AddDate(0, 0, 5)), "末张应收窄到 beagle-b5")
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

	// 恰好达标线(8 张),排除 1 张后跌破 min_photos=8,实体应从挖掘输出消失
	// ——这是有意语义:复现性证据不足以支撑"这是我自己的宠物"这一判断。
	_, err := store.ExcludeMomentAssets(momentID, []string{"lab-a"})
	require.NoError(t, err)

	entities, err := MinePetEntities(context.Background(), db, store, recipe)
	require.NoError(t, err)
	require.Empty(t, entities, "跌破 min_photos 门槛后实体不应再被挖掘出来")
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

	// 一张 caption 里完全没提 "beagle" 的照片,拍摄时间早于当前 first
	// seen,验证 pin 并入后计数增加且跨度向前延展。
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
	require.Equal(t, 15, e.PhotoCount, "14 张词命中 + 1 张 pin 确认样本")
	require.True(t, e.FirstSeen.Equal(earlier), "pin 的更早样本应延展 first seen")
	require.True(t, e.LastSeen.Equal(month2.AddDate(0, 0, 6)), "last seen 不受影响")
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

	// 已回收站的资产:仍在 assets 表里(满足 moment_edits 外键),但不在
	// loadThemeCandidatePool 候选池——pin 不应把它计入统计。
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
	require.Equal(t, 8, entities[0].PhotoCount, "回收站资产的 pin 不应计入统计")
}

// ── petEntitySubtitle:年份跨度格式 ───────────────────────────────────────

func TestPetEntitySubtitle(t *testing.T) {
	sameYear := petEntitySubtitle(
		time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2020, 12, 31, 0, 0, 0, 0, time.UTC))
	require.Equal(t, "2020", sameYear, "同年只写一年")

	crossYear := petEntitySubtitle(
		time.Date(2011, 8, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC))
	require.Equal(t, "2011 – 2026", crossYear, "跨年 en dash 两侧各一空格")
}

// ── BuildPetEntityMoments:draft 组装 + 成员并集 + 落画像表 ───────────────

func TestBuildPetEntityMoments_DraftsAndMemberUnion(t *testing.T) {
	db := makeTestDB(t)
	profileStore := NewProfileStore(db)

	base := time.Date(2020, time.March, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 8; i++ {
		id := "beagle-" + string(rune('a'+i))
		insertThemeAsset(t, db, id, base.AddDate(0, 0, i))
		insertCaption(t, db, id, "our beagle at the park")
	}
	// 仅被 CLIP 命中、caption 里没有 "beagle" 字样,验证并集(而非交集)。
	insertThemeAsset(t, db, "beagle-clip-only", base.AddDate(0, 0, 30))
	insertCaption(t, db, "beagle-clip-only", "a lazy afternoon nap")

	recipe := petTestRecipe([]string{"beagle"}, 8, 1)

	searcher := fakeThemeSearcher{hits: map[string][]AssetScore{
		"a photo of a beagle": {
			{AssetID: "beagle-clip-only", Score: 0.9},
			{AssetID: "beagle-a", Score: 0.5}, // 词命中的资产同时被 CLIP 命中,取更高分。
		},
	}}

	drafts, err := BuildPetEntityMoments(context.Background(), db, searcher, profileStore, NewMomentStore(db), recipe)
	require.NoError(t, err)
	require.Len(t, drafts, 1)

	d := drafts[0]
	require.Equal(t, ProfileEntityID("pet", "beagle"), d.ID)
	require.Equal(t, "profile:pets", d.RecipeKey)
	require.Equal(t, "Your Beagle", d.Title)
	require.Equal(t, "2020", d.Subtitle, "同年只写一年")
	require.Len(t, d.Assets, 9, "8 张词命中 ∪ 1 张仅 CLIP 命中")

	byID := map[string]MomentAsset{}
	for _, a := range d.Assets {
		byID[a.AssetID] = a
	}
	require.Contains(t, byID, "beagle-clip-only")
	require.InDelta(t, 0.9, byID["beagle-clip-only"].Score, 1e-9)
	require.InDelta(t, 0.5, byID["beagle-a"].Score, 1e-9, "词命中+CLIP 命中取更高分")
	require.InDelta(t, 0.45, byID["beagle-b"].Score, 1e-9, "仅词命中应记 ClipMinScore 保底分")

	saved, err := profileStore.ListEntities("pet")
	require.NoError(t, err)
	require.Len(t, saved, 1)
	require.Equal(t, "beagle", saved[0].Key)
}

func TestBuildPetEntityMoments_NoQualifyingClearsProfile(t *testing.T) {
	db := makeTestDB(t)
	profileStore := NewProfileStore(db)

	// 陈旧画像:模拟上一轮曾达标(如今词表变化/宠物不再复现,本轮不再达标)。
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
	require.Empty(t, saved, "无达标实体应清空上一轮画像,不残留陈旧数据")
}
