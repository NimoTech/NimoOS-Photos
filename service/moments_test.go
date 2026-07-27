// MomentsService 的测试:覆盖简报 Step 1 清单——重算调通 trip/theme 两个
// kind、LLM 命名成功覆盖 title、namer 失败不影响 RecomputeAll 返回 nil、
// CAS 重入直接返回。用真 MomentStore(makeTestDB)+ fakeThemeSearcher(已在
// moments_theme_test.go 定义,同包复用)+ fake namer,不接触真实 ML/AI。
package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// noVecLoader 是 clipVecLoader 的测试替身:永远返回"无向量",PickFeaturedAndCover
// 据此跳过连拍去重、直接按 aesthetic_score(测试里也不打分,故全部并列)
// 进候选池——不影响本文件测试关注的"调通"语义。
func noVecLoader(_ string) ([]float32, bool) { return nil, false }

// fakeNamer 是 namer 接口的测试替身:固定返回同一个标题,或固定返回错误
// (模拟 LLM 超时/AI 未部署),并记录被调用次数供断言。
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

	// trip recipe:10 张带 GPS 的照片,足量成团。
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

	// theme recipe:2 张被 CLIP 命中的照片。
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
	require.Len(t, moments, 2, "trip + theme 各产出一个时刻")

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
	require.True(t, moments[0].NamedByLLM, "LLM 命名成功应置 named_by_llm=1")
	require.Equal(t, "Cozy Spring Days", moments[0].Title)
	require.Equal(t, 1, namer.calls)

	// 第二轮重算:named_by_llm=1 的 title 不应被模板结果覆盖,也不应再次调用 LLM。
	require.NoError(t, svc.RecomputeAll(context.Background()))
	moments2, err := store.ListMoments()
	require.NoError(t, err)
	require.Equal(t, "Cozy Spring Days", moments2[0].Title, "已 LLM 命名的时刻标题应保持不变")
	require.Equal(t, 1, namer.calls, "已命名的时刻不应重复调用 LLM")
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
	require.NoError(t, err, "LLM 失败必须是 best-effort,绝不阻塞重算")

	moments, err := store.ListMoments()
	require.NoError(t, err)
	require.Len(t, moments, 1)
	require.False(t, moments[0].NamedByLLM, "命名失败不应置 named_by_llm")
	require.Equal(t, "Nara Trip", moments[0].Title, "命名失败应保留模板打底标题")
}

func TestRecomputeAll_ReentrancyReturnsNilImmediately(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)
	require.NoError(t, store.UpsertRecipes([]MomentRecipe{
		{Key: "trip", Kind: "trip", Title: "Trip", Enabled: true},
	}))

	namer := &fakeNamer{title: "irrelevant"}
	svc := NewMomentsService(db, store, fakeThemeSearcher{}, noVecLoader, namer)

	// 手动置位 CAS 标志,模拟"已有一轮重算在跑"——同包白盒测试可以直接摸
	// 内部字段。
	svc.running.Store(true)
	defer svc.running.Store(false)

	err := svc.RecomputeAll(context.Background())
	require.NoError(t, err)

	moments, err := store.ListMoments()
	require.NoError(t, err)
	require.Empty(t, moments, "CAS 重入应直接返回,不做任何重算工作")
}

// TestCleanLLMTitle_TruncatesOverlongOutput:模型不守"至多 4 个单词"约束、
// 附赠一段长解释时,cleanLLMTitle 必须按 rune 安全截断到
// maxLLMTitleRunes,防止长文原样落库展示给用户。
func TestCleanLLMTitle_TruncatesOverlongOutput(t *testing.T) {
	long := strings.Repeat("汉字标题超长测试", 20) // 160 个 rune,远超 80 上限
	got := cleanLLMTitle(long)
	require.Len(t, []rune(got), maxLLMTitleRunes)
	require.Equal(t, string([]rune(long)[:maxLLMTitleRunes]), got)

	// 短标题不受影响。
	require.Equal(t, "Sunset Beach", cleanLLMTitle(`  "Sunset Beach"  `+"\nsome trailing explanation"))
}

// TestRecomputeAll_OverlongLLMTitleIsTruncatedBeforeStore:端到端确认
// RecomputeAll 落库前会截断超长 LLM 输出,而不是原样存进 moments.title。
func TestRecomputeAll_OverlongLLMTitleIsTruncatedBeforeStore(t *testing.T) {
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
}
