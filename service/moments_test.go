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
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
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

// TestRecomputeAll_HiddenMomentSkippedInNamingLoop:命名循环的候选来源是
// store.ListMoments(),该方法已按 hidden=0 过滤(momentstore.go),所以隐藏
// 时刻天然不会被喂给 LLM——这里补一个断言测试锁定该行为,防止未来有人改动
// 候选来源(比如换成直接查 moments 表)时悄悄漏了 hidden 过滤。
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

	// 第一轮:namer 失败,时刻产出但仍是模板打底(named_by_llm=0),
	// 保证后面隐藏它时还处于"命名循环本会挑中"的状态。
	failingNamer := &fakeNamer{err: errors.New("ai unavailable")}
	svc := NewMomentsService(db, store, fakeThemeSearcher{}, noVecLoader, failingNamer)
	require.NoError(t, svc.RecomputeAll(context.Background()))

	moments, err := store.ListMoments()
	require.NoError(t, err)
	require.Len(t, moments, 1)
	require.False(t, moments[0].NamedByLLM)
	require.Equal(t, 1, failingNamer.calls)

	require.NoError(t, store.HideMoment(moments[0].ID))

	// 第二轮:namer 换成会成功的,但命名循环取候选走 ListMoments(已过滤
	// hidden=0),隐藏的时刻不应再被喂给 LLM——calls 应保持 0(全新 namer)。
	successNamer := &fakeNamer{title: "Should Not Apply"}
	svc2 := NewMomentsService(db, store, fakeThemeSearcher{}, noVecLoader, successNamer)
	require.NoError(t, svc2.RecomputeAll(context.Background()))

	require.Equal(t, 0, successNamer.calls, "隐藏的时刻不应进入 LLM 命名循环")

	stillHidden, err := store.ListMoments()
	require.NoError(t, err)
	require.Len(t, stillHidden, 0, "隐藏语义应在重算后保持(SyncRecipeMoments 不清 hidden 列)")
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

// TestRecomputeAll_PerRecipeFailureIsolation:theme 引擎依赖的 CLIP 检索(ML)
// 掉线时,单个 recipe 失败必须 Warn + 跳过、继续处理下一个 recipe——不阻塞
// 不依赖 ML 的 trip、也不清空该 theme recipe 上一轮产出的旧时刻(不调用
// SyncRecipeMoments 意味着旧时刻原样保留)。RecomputeAll 整体仍返回 nil。
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

	// 第一轮:ML 正常,trip + theme 都应产出。
	workingSearcher := fakeThemeSearcher{hits: map[string][]AssetScore{
		"a photo of a dog": {{AssetID: "theme-a", Score: 0.9}, {AssetID: "theme-b", Score: 0.8}},
	}}
	firstRun := NewMomentsService(db, store, workingSearcher, noVecLoader, &fakeNamer{title: "irrelevant"})
	require.NoError(t, firstRun.RecomputeAll(context.Background()))

	before, err := store.ListMoments()
	require.NoError(t, err)
	require.Len(t, before, 2, "ML 正常时 trip+theme 都应产出")
	var themeBefore Moment
	for _, m := range before {
		if m.RecipeKey == "theme:pets" {
			themeBefore = m
		}
	}
	require.NotEmpty(t, themeBefore.ID, "第一轮应已产出 theme:pets 时刻")

	// 第二轮:ML 掉线(searcher 报错),theme:pets 应被跳过,trip 仍正常重算。
	failingSearcher := fakeThemeSearcher{err: errors.New("clip search: connection refused (ML 掉线)")}
	secondRun := NewMomentsService(db, store, failingSearcher, noVecLoader, &fakeNamer{title: "irrelevant"})
	err = secondRun.RecomputeAll(context.Background())
	require.NoError(t, err, "单个 recipe(theme)失败必须 best-effort 跳过,不阻塞整轮、不向上传播 error")

	after, err := store.ListMoments()
	require.NoError(t, err)
	require.Len(t, after, 2, "theme 失败应保留旧时刻不被清空;trip 仍应正常重算产出")

	var themeAfter Moment
	for _, m := range after {
		if m.RecipeKey == "theme:pets" {
			themeAfter = m
		}
	}
	require.Equal(t, themeBefore.ID, themeAfter.ID)
	require.Equal(t, themeBefore.UpdatedAt, themeAfter.UpdatedAt,
		"ML 闪断跳过的 recipe 不应调用 SyncRecipeMoments,updated_at 不变")
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

// TestRecomputeAll_ThemeMomentsNeverGoThroughLLMNaming:真机验收发现 LLM
// 会把 theme 策划好的标题(recipe.Title,如"Pet Moments")改差(如误改成
// "Sunset on Highway"),故 theme 时刻必须永不进 LLM 命名循环;trip 时刻仍
// 应正常尝试 LLM 命名。用同一个 fakeNamer 记录调用次数,断言最终只有 trip
// 那一次调用,且 theme 标题原样是 recipe.Title、未被 fakeNamer 的固定值覆盖。
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

	namer := &fakeNamer{title: "Sunset On Highway"} // 模拟 LLM 会瞎改名的场景
	svc := NewMomentsService(db, store, searcher, noVecLoader, namer)

	require.NoError(t, svc.RecomputeAll(context.Background()))

	require.Equal(t, 1, namer.calls, "只有 trip 时刻应触发 LLM 命名,theme 一次都不该调用")

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
	require.True(t, trip.NamedByLLM, "trip 时刻应正常走 LLM 命名")
	require.Equal(t, "Sunset On Highway", trip.Title)
	require.False(t, theme.NamedByLLM, "theme 时刻永不应被标记为 LLM 命名")
	require.Equal(t, "Pet Moments", theme.Title, "theme 标题必须保持 recipe.Title 策划好的名字,不被 LLM 篡改")
}

// TestBuildNamingPrompt_NoPhotoAppEchoAndHasHardenedConstraints:真机验收
// 发现弱本地模型会把旧 prompt 里的 "photo app" 措辞回声进标题(如"Nighttime
// Las Vegas Photo App."),故新 prompt 必须不含这个措辞,且显式列出
// Title Case/English only/≤4 words/无标点引号/不要复述指令等加固约束,并
// 带上 few-shot 示例。
func TestBuildNamingPrompt_NoPhotoAppEchoAndHasHardenedConstraints(t *testing.T) {
	m := Moment{Place: "Kyoto, JP", TimeFrom: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)}
	prompt := buildNamingPrompt(m, []string{"a photo of a temple"})

	require.NotContains(t, prompt, "photo app", "prompt 不应再出现会被模型回声进标题的 'photo app' 措辞")
	require.Contains(t, prompt, "Title Case")
	require.Contains(t, prompt, "English only")
	require.Contains(t, prompt, "at most 4 words")
	require.Contains(t, prompt, "no punctuation or quotes")
	require.Contains(t, prompt, "do not repeat or explain these instructions")
	require.Contains(t, prompt, "Golden Gate Evenings", "应带 few-shot 示例")
	require.Contains(t, prompt, "Alpine Ski Days", "应带第二条 few-shot 示例")
}

// TestRecomputeAll_PetEntitiesReplaceConceptThemePets:替换规则正向——
// profile:pets 挖掘出 ≥1 个达标宠物实体时,概念版 theme:pets 时刻必须被
// 清空(即使 theme 引擎本身的判据也能命中同一批照片,产出的是"用户自己的
// 那只狗"而不是"全库搜索含狗元素")。recipe key 字典序 profile:pets <
// theme:pets(p<t),ListRecipes 按 key 升序,故本轮循环处理到 theme:pets 时
// petEntitiesProduced 标志已经就位。
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

	// 若替换规则不生效,theme 引擎凭 caption_keywords "dog" 命中这 3 张也足以
	// 达标(min_assets=2)产出概念版——本用例要证明它被清空,而不是"本来就没
	// 有候选"。
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
	require.NotContains(t, recipeKeys, "theme:pets", "已产出个人化宠物实体时刻应替换掉概念版")
	require.Contains(t, recipeKeys, "profile:pets", "应产出 Your Beagle 实体时刻")

	var petMoment Moment
	for _, m := range moments {
		if m.RecipeKey == "profile:pets" {
			petMoment = m
		}
	}
	require.Equal(t, "Your Beagle", petMoment.Title)
}

// TestRecomputeAll_NoPetEntitiesFallsBackToConceptThemePets:替换规则反向——
// 全库无达标宠物实体时,概念版 theme:pets 照常产出(回退语义)。
func TestRecomputeAll_NoPetEntitiesFallsBackToConceptThemePets(t *testing.T) {
	db := makeTestDB(t)
	store := NewMomentStore(db)
	require.NoError(t, store.UpsertRecipes([]MomentRecipe{
		// min_photos 故意设得远高于本轮实际张数,确保 profile:pets 本轮无
		// 达标实体产出。
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
	require.True(t, found, "无达标宠物实体时,概念版 theme:pets 应照常产出(回退)")
	require.Equal(t, "Pet Moments", themeMoment.Title)
}

// TestRecomputeAll_OverlongLLMTitleRejectedKeepsTemplate:端到端确认
// RecomputeAll 面对远超词数守卫的长句 LLM 输出时,不会原样(或截断后)存进
// moments.title——cleanLLMTitle 的 rune 截断仍在(见 maxLLMTitleRunes),但
// 截断后词数依旧 > maxLLMTitleWords,词数守卫会整条拒收、保留模板打底标题。
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
	require.False(t, moments[0].NamedByLLM, "超词数应被词数守卫拒收,不算 LLM 命名成功")
	require.Equal(t, "Sapporo Trip", moments[0].Title, "拒收应保留模板打底标题")
}

// TestRecomputeAll_LLMTitleWordGuardRejectsOverSixWords:真机实证的核心场景——
// 本地弱模型不守"至多 4 词"指令、吐出 7 词整句(如 "May 28 2011 Overcast Sky
// Somewhere" 这类日期+天气拼接),词数守卫应整条拒收、保留模板打底标题、且
// 不置 named_by_llm,同时留一条 Debug 日志供观测(被拒标题)。
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

	rejected := "May 28 2011 Overcast Sky Somewhere Nearby" // 7 词,超过 6 词上限
	namer := &fakeNamer{title: rejected}
	svc := NewMomentsService(db, store, fakeThemeSearcher{}, noVecLoader, namer)

	obsCore, logs := observer.New(zap.DebugLevel)
	restore := zap.ReplaceGlobals(zap.New(obsCore))
	defer restore()

	require.NoError(t, svc.RecomputeAll(context.Background()))

	moments, err := store.ListMoments()
	require.NoError(t, err)
	require.Len(t, moments, 1)
	require.False(t, moments[0].NamedByLLM, "超 6 词应被拒收,不算 LLM 命名成功")
	require.Equal(t, "Naha Trip", moments[0].Title, "拒收应保留模板打底标题不变")

	found := false
	for _, entry := range logs.All() {
		if strings.Contains(entry.Message, "词数超限") {
			found = true
			break
		}
	}
	require.True(t, found, "被拒标题应走 Debug 日志留痕")
}

// TestRecomputeAll_LLMTitleWordGuardAcceptsUpToSixWords:恰好 6 词的标题应正常
// 收下、覆盖模板名、置 named_by_llm=1——守卫只拒收超过 6 词的,不误伤边界值。
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

	accepted := "One Two Three Four Five Six" // 恰好 6 词
	namer := &fakeNamer{title: accepted}
	svc := NewMomentsService(db, store, fakeThemeSearcher{}, noVecLoader, namer)

	require.NoError(t, svc.RecomputeAll(context.Background()))

	moments, err := store.ListMoments()
	require.NoError(t, err)
	require.Len(t, moments, 1)
	require.True(t, moments[0].NamedByLLM, "6 词应正常收下,置 named_by_llm=1")
	require.Equal(t, accepted, moments[0].Title)
}
