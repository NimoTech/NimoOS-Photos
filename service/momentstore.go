// Smart Moments 数据层:三表(moment_recipes/moments/moment_assets)的 repo。
//
// moment_recipes 是"时刻类型=数据"的热更新载体——engine 按 kind 分派算法,
// clip_prompts/caption_keywords/阈值等参数纯数据上新,PUT recipes 即可生效,
// 无需改代码(真正需要新算法的 kind 除外)。
//
// moments/moment_assets 是活实体:稳定派生 id(TripMomentID/ThemeMomentID),
// 每轮重算按 id upsert + 成员 diff 式 upsert(既有成员保留 added_at、缺席者
// 删除但豁免 pin 成员),用户看到的时刻不因重算而闪断;LLM 已命名过的 title(named_by_llm=1)重算时原样保留,只有
// 模板打底阶段(named_by_llm=0)的 title 才会被下一轮重算的模板结果覆盖。
package service

import (
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// MomentRecipe 对应 moment_recipes 表一行。ParamsJSON 是原始 JSON 字符串,
// 具体字段经 ParseParams 解析为 RecipeParams(带默认值填充)。
type MomentRecipe struct {
	Key        string
	Kind       string // "trip" | "theme"
	Title      string
	ParamsJSON string
	Enabled    bool
	UpdatedAt  int64 // Unix ms
}

// RecipeParams 是 moment_recipes.params 列的解析结果。json tag 用蛇形命名,
// 与 PUT /v1/photos/moments/recipes 的推送 JSON 格式一致。
type RecipeParams struct {
	ClipPrompts     []string `json:"clip_prompts"`
	CaptionKeywords []string `json:"caption_keywords"`
	MinAssets       int      `json:"min_assets"`
	MaxFeatured     int      `json:"max_featured"`
	GapDays         int      `json:"gap_days"`
	TopK            int      `json:"top_k"`
	MinScore        float64  `json:"min_score"`

	// ── Moments M2 画像层新增字段(profile:pets / profile:family 专用)──────
	// Lexicon 无默认回落(未指定就是空词表,凭空回落一份猜的词表比空更危险);
	// 其余字段(含 MinAssets,family 的"合影集门槛"复用该既有字段)照旧
	// "零值即未指定"回落默认值,见 ParseParams 注释。
	Lexicon            []string `json:"lexicon"`              // profile:pets:物种/品种英文词表
	MinPhotos          int      `json:"min_photos"`           // profile:pets:达标最少张数
	MinMonths          int      `json:"min_months"`           // profile:pets:达标最少跨月数
	ClipMinScore       float64  `json:"clip_min_score"`       // profile:pets:CLIP 检索最低分
	ClipTopK           int      `json:"clip_top_k"`           // profile:pets:CLIP 检索 top-K
	TopPersons         int      `json:"top_persons"`          // profile:family:具名人物时刻取前 K 高频人物
	MinPersonPhotos    int      `json:"min_person_photos"`    // profile:family:人物达标最少张数
	MinTogetherPersons int      `json:"min_together_persons"` // profile:family:合影集同框最少人数
}

// 默认值:recipe 未显式指定(或字段缺省为零值)时的兜底,详见简报。
const (
	defaultMinAssets   = 10
	defaultMaxFeatured = 12
	defaultGapDays     = 14
	defaultTopK        = 200
	defaultMinScore    = 0.2

	// profile:pets 默认值(见设计 spec 第一节)。
	defaultMinPhotos    = 8
	defaultMinMonths    = 2
	defaultClipMinScore = 0.45
	defaultClipTopK     = 100

	// profile:family 默认值(见设计 spec 第一节)。
	defaultTopPersons         = 5
	defaultMinPersonPhotos    = 30
	defaultMinTogetherPersons = 2
)

// ParseParams 解析 recipe.ParamsJSON 为 RecipeParams,对缺省(零值)字段填充
// 默认值。注意:这里用"零值即未指定"判断是否回落默认值——recipe 参数语义
// 上没有"故意设为 0"的合理场景(min_assets=0/gap_days=0 等没有实际意义),
// 所以零值回落是安全的简化,不需要区分"未出现的 key"与"显式写 0"。
func ParseParams(r MomentRecipe) (RecipeParams, error) {
	var p RecipeParams
	if s := strings.TrimSpace(r.ParamsJSON); s != "" {
		if err := json.Unmarshal([]byte(s), &p); err != nil {
			return RecipeParams{}, fmt.Errorf("moments: parse recipe params %q: %w", r.Key, err)
		}
	}
	if p.MinAssets == 0 {
		p.MinAssets = defaultMinAssets
	}
	if p.MaxFeatured == 0 {
		p.MaxFeatured = defaultMaxFeatured
	}
	if p.GapDays == 0 {
		p.GapDays = defaultGapDays
	}
	if p.TopK == 0 {
		p.TopK = defaultTopK
	}
	if p.MinScore == 0 {
		p.MinScore = defaultMinScore
	}
	if p.MinPhotos == 0 {
		p.MinPhotos = defaultMinPhotos
	}
	if p.MinMonths == 0 {
		p.MinMonths = defaultMinMonths
	}
	if p.ClipMinScore == 0 {
		p.ClipMinScore = defaultClipMinScore
	}
	if p.ClipTopK == 0 {
		p.ClipTopK = defaultClipTopK
	}
	if p.TopPersons == 0 {
		p.TopPersons = defaultTopPersons
	}
	if p.MinPersonPhotos == 0 {
		p.MinPersonPhotos = defaultMinPersonPhotos
	}
	if p.MinTogetherPersons == 0 {
		p.MinTogetherPersons = defaultMinTogetherPersons
	}
	// Lexicon 故意不回落默认值:未指定就是空词表(见字段注释)。
	return p, nil
}

// Moment 对应 moments 表一行(活实体)。TimeFrom/TimeTo 为零值 time.Time 时
// 表示该列在库中是 NULL(主题类时刻没有固定时间窗)。
type Moment struct {
	ID           string
	RecipeKey    string
	Title        string
	Subtitle     string
	CoverAssetID string
	Place        string
	TimeFrom     time.Time
	TimeTo       time.Time
	AssetCount   int
	NamedByLLM   bool
	CreatedAt    int64 // Unix ms
	UpdatedAt    int64 // Unix ms
	// SortOrder 对应 sort_order 列:nil=未手排(NULL 语义保真,与"手排到 0"
	// 区分);非 nil 时是 ReorderMoments 写入的 (i+1)*10 序号。
	SortOrder *int
	// Hidden 对应 hidden 列:用户"隐藏此时刻"的 tombstone。ListMoments 按
	// hidden=0 过滤,SyncRecipeMoments 重算不会清除(不在 upsert 列清单里,
	// 与 named_by_llm/sort_order 同法)。
	Hidden bool
}

// MomentAsset 对应 moment_assets 表一行。
type MomentAsset struct {
	AssetID  string
	Featured bool
	Score    float64
	// Manual 对应 manual 列:1=该成员是用户 pin 编辑回放插入(非引擎本轮产出),
	// 仅供展示/排障区分来源。
	Manual bool
	// AddedAt 对应 added_at 列:成员加入时刻的 Unix ms 时间戳,0=NULL(存量/
	// 加入时间未知,不参与"本周新增"计数)。仅供内部/测试使用,资产端点不
	// 直接暴露该字段(见简报)。
	AddedAt int64
}

// MomentPlace 是 About 多地点展示的一条聚合结果:某城市在时刻成员中出现的
// 次数,供 PlacesByMoment 按次数降序返回。
type MomentPlace struct {
	Name  string
	Count int
}

// MomentDraft 是引擎每轮重算产出的候选时刻(尚未落库的草稿):嵌入 Moment 的
// 展示字段 + 本轮全量成员集合。SyncRecipeMoments 按 ID 幂等合并进库。
type MomentDraft struct {
	Moment
	Assets []MomentAsset
}

// MomentStore 是 Smart Moments 三表的 repo 层,纯 SQL,无 ORM(照本库
// captionpull.go 等既有 store 的风格)。
type MomentStore struct {
	db *sql.DB
}

// NewMomentStore 构造 MomentStore。
func NewMomentStore(db *sql.DB) *MomentStore {
	return &MomentStore{db: db}
}

// nowMs 返回当前 Unix 毫秒时间戳(moments/moment_recipes 的 *_at 列约定)。
func nowMs() int64 {
	return time.Now().UnixMilli()
}

// ── recipe 种子 ──────────────────────────────────────────────────────────

// seedRecipe 是内置 recipe 的声明式描述,拼装成 MomentRecipe 落库。
type seedRecipe struct {
	key    string
	kind   string
	title  string
	params RecipeParams
}

// defaultSeedRecipes 是启动时 seed 的内置集:trip(时间窗×地点)+ 首批
// theme(caption 关键词 + CLIP prompt 并集匹配)。clip_prompts 是给 CLIP 的
// 自然描述句(而非关键词堆砌),caption_keywords 是小写单词,供
// instr(lower(text),...) 匹配 asset_caption。文案面向英文用户,产品默认
// 展示语言是英文,中文由前端 i18n 负责。
func defaultSeedRecipes() []seedRecipe {
	return []seedRecipe{
		{
			key: "trip", kind: "trip", title: "Trip",
			// trip 的展示名由引擎按 "{主城} Trip" 模板动态生成(见设计 spec
			// 第二节),这里的 title 只是 recipe 管理列表里的通用标签。
			params: RecipeParams{},
		},
		{
			key: "theme:pets", kind: "theme", title: "Pet Moments",
			params: RecipeParams{
				ClipPrompts:     []string{"a photo of a pet dog or cat", "a cute animal companion"},
				CaptionKeywords: []string{"dog", "cat", "puppy", "kitten", "pet"},
			},
		},
		{
			key: "theme:food", kind: "theme", title: "Food Moments",
			params: RecipeParams{
				ClipPrompts:     []string{"a plate of food on a table", "a close-up photo of a delicious meal"},
				CaptionKeywords: []string{"food", "meal", "dish", "restaurant", "cooking", "dinner"},
			},
		},
		{
			key: "theme:snow", kind: "theme", title: "Snow Days",
			params: RecipeParams{
				ClipPrompts:     []string{"a landscape covered in fresh snow", "people playing in the snow"},
				CaptionKeywords: []string{"snow", "snowy", "snowman", "skiing", "snowboard", "winter"},
			},
		},
		{
			key: "theme:beach", kind: "theme", title: "Beach Days",
			params: RecipeParams{
				ClipPrompts:     []string{"a sandy beach with ocean waves", "people relaxing on a beach by the sea"},
				CaptionKeywords: []string{"beach", "ocean", "sand", "seaside", "surf", "shore"},
			},
		},
		{
			key: "theme:sunset", kind: "theme", title: "Sunset Views",
			params: RecipeParams{
				ClipPrompts:     []string{"a beautiful sunset with an orange sky", "the sun setting over the horizon"},
				CaptionKeywords: []string{"sunset", "dusk", "golden hour", "horizon"},
			},
		},
		{
			// profile:pets 是画像层挖掘配置(kind=pet_entities,与上面
			// theme:pets 的"全库搜含宠物元素"概念版不同):对 lexicon 每词做
			// caption 词边界匹配,统计张数+跨月数,达标(≥min_photos 且
			// ≥min_months)才归纳成"用户自己的那只狗/猫"实体,详见设计
			// spec 第一节。lexicon 覆盖常见狗/猫品种 + 鸟类 + 小宠,英文,
			// 供词边界匹配;多词短语(如 "maine coon")按整短语边界匹配。
			key: "profile:pets", kind: "pet_entities", title: "Pet Entities",
			params: RecipeParams{
				Lexicon:   petEntityLexicon(),
				MinPhotos: defaultMinPhotos, MinMonths: defaultMinMonths,
				ClipMinScore: defaultClipMinScore, ClipTopK: defaultClipTopK,
			},
		},
		{
			// profile:family 是画像层家人挖掘配置(kind=family):具名人物
			// 出现频次达标 top-K 归纳为具名人物时刻,高频人物同框归纳为合影集,
			// 详见设计 spec 第一节。
			key: "profile:family", kind: "family", title: "Family Entities",
			params: RecipeParams{
				TopPersons: defaultTopPersons, MinPersonPhotos: defaultMinPersonPhotos,
				MinTogetherPersons: defaultMinTogetherPersons, MinAssets: defaultMinAssets,
			},
		},
	}
}

// petEntityLexicon 是 profile:pets 挖掘用的物种/品种英文词表:覆盖常见狗/猫
// 品种 + 鸟类 + 小型宠物,约 60-100 词,供 caption 词边界匹配。刻意不含过于
// 宽泛的词(如单独的 "dog"/"cat"——那是 theme:pets 概念版的职责,画像层要的
// 是能收敛到"用户特定那只"的具体品种/物种词,复现性信号才有区分力)。
//
// 审查后修复(高频误匹配词,美国市场场景):
//   - 删除 newfoundland(纽芬兰,地名同形)、finch(常见姓氏,如 Atticus Finch)、
//     canary(加纳利群岛,Canary Islands 同形)、goldfish(Goldfish 品牌饼干同名,
//     且真金鱼 caption 通常连写 "goldfish in a bowl" 一类短语,裸词误召回风险
//     大于收益)。
//   - 单词有歧义的品种消歧为短语:akita→"akita dog"、boxer→"boxer dog"、
//     greyhound→"greyhound dog"(避免与拳击手 boxer / 田径 greyhound 巴士等
//     混淆);裸 "shepherd" 换成具体品种 "german shepherd"/"australian shepherd"
//     (裸词本身就不够具体,不如直接两个高频具体品种)。
//   - 猫的花纹词(tabby/calico/tuxedo/ginger 等)本身极常见于口语但单独一词
//     歧义大(如 "tuxedo" 可指礼服),但 VLM 生成的 caption 描述猫时几乎只说
//     花纹+"cat"(如 "a tabby cat"),极少报具体品种——若删掉这类词,大部分
//     用户的猫根本挖不出来实体。因此这里的取舍是:花纹词全部保留,但一律
//     锚成 "<花纹> cat" 双词短语(而非裸花纹词),把"猫品种词有限"的专属性
//     风险交给挖掘门槛(min_photos/min_months 的复现性判据)兜底,而不是在
//     词表层面因噎废食。
func petEntityLexicon() []string {
	return []string{
		// ── Dogs(品种,不含泛化的 "dog"/"puppy";歧义词见上方注释消歧)──
		"beagle", "labrador", "corgi", "husky", "poodle", "terrier", "retriever",
		"bulldog", "dachshund", "chihuahua", "pug", "german shepherd",
		"australian shepherd", "collie", "spaniel", "dalmatian", "boxer dog",
		"rottweiler", "doberman", "schnauzer", "mastiff", "greyhound dog",
		"whippet", "pomeranian", "shih tzu", "maltese", "chow chow", "akita dog",
		"samoyed", "malamute", "bernese mountain dog", "labradoodle",
		"goldendoodle", "basset hound", "bloodhound",
		// ── Cats(品种 + 花纹词,花纹词均锚成 "<花纹> cat" 短语,见上方注释)──
		"tabby cat", "siamese", "persian cat", "maine coon", "ragdoll", "sphynx",
		"bengal cat", "calico cat", "tuxedo cat", "ginger cat", "orange cat",
		"tortoiseshell cat", "british shorthair", "scottish fold", "abyssinian",
		"burmese cat", "russian blue", "himalayan cat",
		// ── Birds(canary/finch 因地名/姓氏同形误召回已删,见上方注释)──────
		"parrot", "parakeet", "cockatiel", "budgie", "macaw", "cockatoo",
		"lovebird",
		// ── Small pets(goldfish 因品牌同名已删,见上方注释)───────────────
		"hamster", "rabbit", "bunny", "guinea pig", "turtle", "tortoise",
		"gecko", "ferret", "chinchilla", "hedgehog", "iguana",
	}
}

// SeedDefaultRecipes 用 INSERT OR IGNORE 写入内置 recipe 集,已存在的 key
// (含运维/应用商店已推送过热更新的)原样跳过,不覆盖。可在每次启动时
// 安全重复调用(幂等)。
func (s *MomentStore) SeedDefaultRecipes() error {
	now := nowMs()
	for _, sr := range defaultSeedRecipes() {
		b, err := json.Marshal(sr.params)
		if err != nil {
			return fmt.Errorf("moments: marshal seed params %q: %w", sr.key, err)
		}
		if _, err := s.db.Exec(`
			INSERT OR IGNORE INTO moment_recipes(key, kind, title, params, enabled, updated_at)
			VALUES (?, ?, ?, ?, 1, ?)`,
			sr.key, sr.kind, sr.title, string(b), now,
		); err != nil {
			return fmt.Errorf("moments: seed recipe %q: %w", sr.key, err)
		}
	}
	return nil
}

// ── recipe 读写 ──────────────────────────────────────────────────────────

// ListRecipes 列出全部 recipe;enabledOnly=true 时只返回 enabled=1 的。
func (s *MomentStore) ListRecipes(enabledOnly bool) ([]MomentRecipe, error) {
	q := `SELECT key, kind, title, params, enabled, updated_at FROM moment_recipes`
	if enabledOnly {
		q += ` WHERE enabled=1`
	}
	q += ` ORDER BY key`
	rows, err := s.db.Query(q)
	if err != nil {
		return nil, fmt.Errorf("moments: list recipes: %w", err)
	}
	defer rows.Close()

	var out []MomentRecipe
	for rows.Next() {
		var r MomentRecipe
		var enabled int
		if err := rows.Scan(&r.Key, &r.Kind, &r.Title, &r.ParamsJSON, &enabled, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("moments: scan recipe: %w", err)
		}
		r.Enabled = enabled != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpsertRecipes 是 recipe 的热更新入口:按 Key upsert 全字段,updated_at 一律
// 写入服务器当前时间(忽略调用方传入的 UpdatedAt),供 `PUT
// /v1/photos/moments/recipes` 推送新/改类型定义使用。
func (s *MomentStore) UpsertRecipes(recipes []MomentRecipe) error {
	if len(recipes) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("moments: upsert recipes begin: %w", err)
	}
	now := nowMs()
	for _, r := range recipes {
		enabled := 0
		if r.Enabled {
			enabled = 1
		}
		if _, err := tx.Exec(`
			INSERT INTO moment_recipes(key, kind, title, params, enabled, updated_at)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET
				kind       = excluded.kind,
				title      = excluded.title,
				params     = excluded.params,
				enabled    = excluded.enabled,
				updated_at = excluded.updated_at`,
			r.Key, r.Kind, r.Title, r.ParamsJSON, enabled, now,
		); err != nil {
			tx.Rollback()
			return fmt.Errorf("moments: upsert recipe %q: %w", r.Key, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("moments: upsert recipes commit: %w", err)
	}
	return nil
}

// ── moments 读写 ─────────────────────────────────────────────────────────

// nullTimeArg 把零值 time.Time 转成 SQL NULL 参数,非零值格式化为本库既有
// DATETIME 惯例的字符串(与 assets.taken_at 同型,见 places.go 写法)。
func nullTimeArg(t time.Time) interface{} {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format("2006-01-02 15:04:05")
}

// SyncRecipeMoments 是幂等重算的落库入口:事务内对每个 draft 按 ID upsert
// moments(named_by_llm=1 的既有行保留其 title/named_by_llm,其余字段更新)
// + 成员 diff 式 upsert(ON CONFLICT 刷新 featured/score/manual 但不触碰
// added_at;缺席成员删除但豁免有 pin 编辑者,防止"删了又被回放补插"把
// added_at 轮刷成假新鲜);随后删除该 recipeKey 下
// 不在本轮 drafts id 集合里的旧 moments(级联清成员),使消失的时刻(如
// gap 重新切分后不再成团的旧 trip)从库中退出。
//
// 用户可见的时刻不因重算而闪断:同 id 的 upsert 不会先删再插,只是字段
// 原地刷新。
func (s *MomentStore) SyncRecipeMoments(recipeKey string, drafts []MomentDraft) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("moments: sync begin: %w", err)
	}
	now := nowMs()
	keepIDs := make([]string, 0, len(drafts))

	for _, d := range drafts {
		namedByLLM := 0 // 草稿(模板打底)永远不是 LLM 命名;沿用/覆盖靠下面的 CASE。
		if _, err := tx.Exec(`
			INSERT INTO moments(id, recipe_key, title, subtitle, cover_asset_id, time_from, time_to, place, asset_count, named_by_llm, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				recipe_key     = excluded.recipe_key,
				title          = CASE WHEN named_by_llm=1 THEN title ELSE excluded.title END,
				subtitle       = excluded.subtitle,
				cover_asset_id = excluded.cover_asset_id,
				time_from      = excluded.time_from,
				time_to        = excluded.time_to,
				place          = excluded.place,
				asset_count    = excluded.asset_count,
				updated_at     = excluded.updated_at`,
			d.ID, d.RecipeKey, d.Title, d.Subtitle, nullableStr(d.CoverAssetID),
			nullTimeArg(d.TimeFrom), nullTimeArg(d.TimeTo), d.Place, d.AssetCount,
			namedByLLM, now, now,
		); err != nil {
			tx.Rollback()
			return fmt.Errorf("moments: upsert moment %q: %w", d.ID, err)
		}

		// 成员 diff 式 upsert(spec 1.2 四步语义,保住 added_at,语义等价于旧
		// "整体替换 delete+insert"):
		//  1. 对本轮 draft 每个成员 upsert:冲突分支只刷新 featured/score/
		//     manual,不触碰 added_at(既有成员保留原加入时间;NULL 也保留
		//     NULL);真正新插入的行打当前时间戳。
		//  2. 删除"本轮未产出"的旧成员,但豁免 pin 成员——否则"删了又被
		//     下面的 applyMomentEdits 回放补插"会把 pin 成员的 added_at
		//     每轮刷新成 now(假新鲜坑,见 spec)。
		for _, a := range d.Assets {
			featured := 0
			if a.Featured {
				featured = 1
			}
			manual := 0
			if a.Manual {
				manual = 1
			}
			if _, err := tx.Exec(`
				INSERT INTO moment_assets(moment_id, asset_id, featured, score, manual, added_at)
				VALUES (?, ?, ?, ?, ?, ?)
				ON CONFLICT(moment_id, asset_id) DO UPDATE SET
					featured = excluded.featured,
					score    = excluded.score,
					manual   = excluded.manual`,
				d.ID, a.AssetID, featured, a.Score, manual, now,
			); err != nil {
				tx.Rollback()
				return fmt.Errorf("moments: upsert member %q/%q: %w", d.ID, a.AssetID, err)
			}
		}

		deleteMembersQ := `
			DELETE FROM moment_assets
			WHERE moment_id = ?`
		deleteMembersArgs := []interface{}{d.ID}
		if len(d.Assets) > 0 {
			placeholders := make([]string, len(d.Assets))
			for i, a := range d.Assets {
				placeholders[i] = "?"
				deleteMembersArgs = append(deleteMembersArgs, a.AssetID)
			}
			deleteMembersQ += ` AND asset_id NOT IN (` + strings.Join(placeholders, ",") + `)`
		}
		// 删除豁免只认活资产(aliveAssetExpr):pin 但已进回收站/离线的资产不
		// 再被豁免,随本次 diff upsert 一并删除(edits 行本身不删,供日后资产
		// 复活时 applyMomentEdits 自动补插回队)。
		deleteMembersQ += `
			AND asset_id NOT IN (
				SELECT me.asset_id FROM moment_edits me
				JOIN assets a ON a.id = me.asset_id
				WHERE me.moment_id=? AND me.op='pin' AND ` + aliveAssetExpr + `
			)`
		deleteMembersArgs = append(deleteMembersArgs, d.ID)
		if _, err := tx.Exec(deleteMembersQ, deleteMembersArgs...); err != nil {
			tx.Rollback()
			return fmt.Errorf("moments: delete stale members %q: %w", d.ID, err)
		}

		// edits 回放:引擎重算不知道用户此前的 pin/exclude 编辑,成员 diff
		// upsert 之后立刻把编辑叠加回去,防止被本轮重算悄悄冲掉。仅当该
		// moment 存在 edits 时才触发派生刷新(count/时间窗/封面重挑)——没有
		// 编辑记录的 moment 维持引擎本轮算出的派生值,不必多余重算。
		hasEdits, err := applyMomentEdits(tx, d.ID, now)
		if err != nil {
			tx.Rollback()
			return err
		}
		if hasEdits {
			// hadTimeWindow:只有 trip 类时刻(draft 带具体 TimeFrom)才按成员
			// taken_at 重算时间窗;主题类时刻(TimeFrom 为零值)时间窗恒为
			// NULL,不应被这里的重算意外赋值。
			if err := refreshMomentDerived(tx, d.ID, !d.TimeFrom.IsZero()); err != nil {
				tx.Rollback()
				return fmt.Errorf("moments: sync refresh derived %q: %w", d.ID, err)
			}
		}
		keepIDs = append(keepIDs, d.ID)
	}

	// 删除该 recipe 下本轮未产出的旧时刻(moment_assets 靠 FK ON DELETE
	// CASCADE 级联清理,无需单独 DELETE)。drafts 为空时清空该 recipe 下
	// 全部时刻。
	deleteQ := `DELETE FROM moments WHERE recipe_key=?`
	args := []interface{}{recipeKey}
	if len(keepIDs) > 0 {
		placeholders := make([]string, len(keepIDs))
		for i, id := range keepIDs {
			placeholders[i] = "?"
			args = append(args, id)
		}
		deleteQ += ` AND id NOT IN (` + strings.Join(placeholders, ",") + `)`
	}
	if _, err := tx.Exec(deleteQ, args...); err != nil {
		tx.Rollback()
		return fmt.Errorf("moments: delete stale moments for %q: %w", recipeKey, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("moments: sync commit: %w", err)
	}
	return nil
}

// aliveAssetExpr 是"活资产"判据 SQL 片段,与 moments_theme.go
// loadThemeCandidatePool(约 L192)同口径:已完成索引(status='indexed')、
// 非回收站(deleted_at IS NULL)、非离线(offline=0)。依赖外层查询把 assets
// 表起别名为 a。pin 相关三处(diff upsert 删除豁免/回放补插/立即插入)统一
// 用这条口径判断资产是否"活着"——债务清扫:此前三处只校验 assets 表存在
// 性,不认活资产,导致 pin 的照片进回收站/离线后依然賴在时刻里不走;现在
// 死资产的 pin 编辑记录(moment_edits)本身仍保留,只是不再豁免/补插其
// 成员身份,资产从回收站/离线状态恢复后下一轮回放会自动归队。
const aliveAssetExpr = `a.status='indexed' AND a.deleted_at IS NULL AND a.offline=0`

// nullableStr 把空字符串转成 SQL NULL(cover_asset_id 允许 NULL,表示"尚无
// 封面");非空字符串原样传入。
func nullableStr(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

// ListMoments 列出全部时刻。排序语义(见设计 spec 第一节):手排序
// (sort_order 非 NULL)的排在最前面,按 sort_order 升序(用户拖拽给定的
// 顺序);未手排(sort_order NULL)的排在手排序之后,按 updated_at 倒序
// (最近重算/命名过的排前面)。全库都未手排时 = 现状不变。
func (s *MomentStore) ListMoments() ([]Moment, error) {
	rows, err := s.db.Query(`
		SELECT id, recipe_key, title, subtitle, cover_asset_id, time_from, time_to,
		       place, asset_count, named_by_llm, created_at, updated_at, sort_order, hidden
		FROM moments
		WHERE hidden=0
		ORDER BY (sort_order IS NULL) ASC, sort_order ASC, updated_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("moments: list: %w", err)
	}
	defer rows.Close()

	var out []Moment
	for rows.Next() {
		m, err := scanMoment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// momentScanner 是 *sql.Row / *sql.Rows 共用的最小 Scan 接口。
type momentScanner interface {
	Scan(dest ...interface{}) error
}

// scanMoment 按 ListMoments 的列顺序扫描一行 moments,处理 cover_asset_id/
// time_from/time_to/sort_order 可能为 NULL 的情况。
func scanMoment(row momentScanner) (Moment, error) {
	var m Moment
	var cover sql.NullString
	var from, to sql.NullString
	var namedByLLM int
	var sortOrder sql.NullInt64
	var hidden int
	if err := row.Scan(&m.ID, &m.RecipeKey, &m.Title, &m.Subtitle, &cover, &from, &to,
		&m.Place, &m.AssetCount, &namedByLLM, &m.CreatedAt, &m.UpdatedAt, &sortOrder, &hidden); err != nil {
		return Moment{}, fmt.Errorf("moments: scan moment: %w", err)
	}
	if cover.Valid {
		m.CoverAssetID = cover.String
	}
	if t := parseSQLiteTime(from); t != nil {
		m.TimeFrom = *t
	}
	if t := parseSQLiteTime(to); t != nil {
		m.TimeTo = *t
	}
	m.NamedByLLM = namedByLLM != 0
	if sortOrder.Valid {
		v := int(sortOrder.Int64)
		m.SortOrder = &v
	}
	m.Hidden = hidden != 0
	return m, nil
}

// GetMomentAssets 返回某时刻的成员,按 score 倒序;featuredOnly=true 时只
// 返回精选(featured=1)成员。
func (s *MomentStore) GetMomentAssets(id string, featuredOnly bool) ([]MomentAsset, error) {
	q := `SELECT asset_id, featured, score, manual, added_at FROM moment_assets WHERE moment_id=?`
	if featuredOnly {
		q += ` AND featured=1`
	}
	q += ` ORDER BY score DESC`
	rows, err := s.db.Query(q, id)
	if err != nil {
		return nil, fmt.Errorf("moments: get assets %q: %w", id, err)
	}
	defer rows.Close()

	var out []MomentAsset
	for rows.Next() {
		var a MomentAsset
		var featured, manual int
		var addedAt sql.NullInt64
		if err := rows.Scan(&a.AssetID, &featured, &a.Score, &manual, &addedAt); err != nil {
			return nil, fmt.Errorf("moments: scan member: %w", err)
		}
		a.Featured = featured != 0
		a.Manual = manual != 0
		if addedAt.Valid {
			a.AddedAt = addedAt.Int64
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ── 可编辑时刻:pin/exclude/hidden ────────────────────────────────────────

// PinMomentAssets 把若干 asset 强制并入某时刻:落一条 op='pin' 的
// moment_edits 记录(覆盖此前该 asset 上的任何编辑),并立即改成员——已是
// 引擎本轮纳入的成员(featured/score 已有值)用 INSERT OR IGNORE 保留原样
// 不降级,缺席的成员补一条 manual=1/featured=0/score=0 的行。assets 表里
// 不存在的 id 静默忽略(既不写 edits,也不改成员)。随后触发派生刷新
// (count/时间窗/封面重挑),返回刷新后的 asset_count。
func (s *MomentStore) PinMomentAssets(momentID string, assetIDs []string) (int, error) {
	return s.applyMomentEditOp(momentID, assetIDs, "pin")
}

// ExcludeMomentAssets 把若干 asset 强制剔除出某时刻:落一条 op='exclude' 的
// moment_edits 记录(覆盖此前该 asset 上的任何编辑),并立即从成员表删除。
// assets 表里不存在的 id 静默忽略。随后触发派生刷新,返回刷新后的
// asset_count(允许降为 0)。
func (s *MomentStore) ExcludeMomentAssets(momentID string, assetIDs []string) (int, error) {
	return s.applyMomentEditOp(momentID, assetIDs, "exclude")
}

// applyMomentEditOp 是 Pin/ExcludeMomentAssets 共用的实现:事务内逐个 asset
// 校验存在性(不存在则跳过)→ upsert moment_edits(后写覆盖先写)→ 立即改
// 成员(pin 只对活资产——aliveAssetExpr——立即生效,死资产仅记 edits 意图,
// 见 aliveAssetExpr 注释)→ 统计本次实际改动的成员行数,为 0(全未知 id/
// pin 目标是死资产/exclude 目标本不是成员等空操作)时跳过派生刷新与
// updated_at(债务清扫:此前无论是否有行变化都刷新,导致空操作也把时刻
// 顶到 ListMoments 排序前端)→ 读回 asset_count。
func (s *MomentStore) applyMomentEditOp(momentID string, assetIDs []string, op string) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("moments: %s begin: %w", op, err)
	}
	now := nowMs()
	var affected int64 // 本次调用实际改动的成员行数(pin INSERT/exclude DELETE 生效数之和)
	for _, assetID := range assetIDs {
		// 未知 id(assets 表不存在)静默忽略:不写 edits、不改成员——moment_edits
		// 对 assets 有外键约束(本库 DSN 开着 _foreign_keys=on),盲写会报错,
		// 这里显式判存在性,语义上也更清楚地对应"未知 id 忽略"。
		var exists int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM assets WHERE id=?`, assetID).Scan(&exists); err != nil {
			tx.Rollback()
			return 0, fmt.Errorf("moments: %s check asset %q: %w", op, assetID, err)
		}
		if exists == 0 {
			continue
		}
		if _, err := tx.Exec(`
			INSERT INTO moment_edits(moment_id, asset_id, op, created_at)
			VALUES (?, ?, ?, ?)
			ON CONFLICT(moment_id, asset_id) DO UPDATE SET
				op         = excluded.op,
				created_at = excluded.created_at`,
			momentID, assetID, op, now,
		); err != nil {
			tx.Rollback()
			return 0, fmt.Errorf("moments: %s upsert edit %q/%q: %w", op, momentID, assetID, err)
		}
		if op == "pin" {
			// 立即插入只认活资产(aliveAssetExpr):死资产(回收站/离线)的 pin
			// 意图已写入 edits,但不立即计入成员/count——与 SyncRecipeMoments
			// 回放的口径保持一致,资产复活后下一轮回放自动归队。
			res, err := tx.Exec(`
				INSERT OR IGNORE INTO moment_assets(moment_id, asset_id, featured, score, manual, added_at)
				SELECT ?, a.id, 0, 0, 1, ?
				FROM assets a
				WHERE a.id=? AND `+aliveAssetExpr,
				momentID, now, assetID,
			)
			if err != nil {
				tx.Rollback()
				return 0, fmt.Errorf("moments: pin insert member %q/%q: %w", momentID, assetID, err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				tx.Rollback()
				return 0, fmt.Errorf("moments: pin rows affected %q/%q: %w", momentID, assetID, err)
			}
			affected += n
		} else {
			res, err := tx.Exec(`DELETE FROM moment_assets WHERE moment_id=? AND asset_id=?`,
				momentID, assetID,
			)
			if err != nil {
				tx.Rollback()
				return 0, fmt.Errorf("moments: exclude delete member %q/%q: %w", momentID, assetID, err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				tx.Rollback()
				return 0, fmt.Errorf("moments: exclude rows affected %q/%q: %w", momentID, assetID, err)
			}
			affected += n
		}
	}

	if affected == 0 {
		// 本次调用未造成任何成员行变化,跳过派生刷新与 updated_at,直接读回
		// 当前 asset_count(不应因空操作而变化)。
		var count int
		if err := tx.QueryRow(`SELECT asset_count FROM moments WHERE id=?`, momentID).Scan(&count); err != nil {
			tx.Rollback()
			return 0, fmt.Errorf("moments: %s read count %q: %w", op, momentID, err)
		}
		if err := tx.Commit(); err != nil {
			return 0, fmt.Errorf("moments: %s commit %q: %w", op, momentID, err)
		}
		return count, nil
	}

	hadTimeWindow, err := momentHasTimeWindow(tx, momentID)
	if err != nil {
		tx.Rollback()
		return 0, err
	}
	if err := refreshMomentDerived(tx, momentID, hadTimeWindow); err != nil {
		tx.Rollback()
		return 0, fmt.Errorf("moments: %s refresh derived %q: %w", op, momentID, err)
	}

	var count int
	if err := tx.QueryRow(`SELECT asset_count FROM moments WHERE id=?`, momentID).Scan(&count); err != nil {
		tx.Rollback()
		return 0, fmt.Errorf("moments: %s read count %q: %w", op, momentID, err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("moments: %s commit %q: %w", op, momentID, err)
	}
	return count, nil
}

// HideMoment 把某时刻标记为隐藏(tombstone):ListMoments 之后不再返回它,
// 但行本身保留(SyncRecipeMoments 重算不会清除该标记,upsert 列清单里
// 没有 hidden 列)。
func (s *MomentStore) HideMoment(momentID string) error {
	_, err := s.db.Exec(`UPDATE moments SET hidden=1, updated_at=? WHERE id=?`, nowMs(), momentID)
	if err != nil {
		return fmt.Errorf("moments: hide %q: %w", momentID, err)
	}
	return nil
}

// MomentEditsFor 返回某时刻当前生效的编辑记录,按 op 分两个切片(供 Task 3
// 挖掘引擎读取,判断某 asset 是否已被用户手动排除/钉入,避免重算把编辑
// 意图悄悄吞掉)。没有编辑记录时返回两个空切片,不报错。
func (s *MomentStore) MomentEditsFor(momentID string) (pins []string, excludes []string, err error) {
	rows, err := s.db.Query(`SELECT asset_id, op FROM moment_edits WHERE moment_id=? ORDER BY asset_id`, momentID)
	if err != nil {
		return nil, nil, fmt.Errorf("moments: edits for %q: %w", momentID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var assetID, op string
		if err := rows.Scan(&assetID, &op); err != nil {
			return nil, nil, fmt.Errorf("moments: scan edit: %w", err)
		}
		switch op {
		case "pin":
			pins = append(pins, assetID)
		case "exclude":
			excludes = append(excludes, assetID)
		}
	}
	return pins, excludes, rows.Err()
}

// TopFeaturedByMoment 一次查询取出全库 featured 成员(按 score 降序),JOIN
// moments 排除各自的封面(封面已经单独展示,不需要在"精选"列表里重复出现),
// 在 Go 侧按 moment 分组各截取前 perMoment 个。因为 SQL 已按 score 降序
// 返回,同一 moment 的行在结果流中天然保持相对顺序,分组截取即为"该
// moment 内 score 最高的前 N 个"。perMoment<=0 视为不截断。
func (s *MomentStore) TopFeaturedByMoment(perMoment int) (map[string][]string, error) {
	rows, err := s.db.Query(`
		SELECT ma.moment_id, ma.asset_id, ma.score
		FROM moment_assets ma
		JOIN moments m ON m.id = ma.moment_id
		WHERE ma.featured=1 AND (m.cover_asset_id IS NULL OR ma.asset_id <> m.cover_asset_id)
		ORDER BY ma.score DESC, ma.asset_id ASC`)
	if err != nil {
		return nil, fmt.Errorf("moments: top featured: %w", err)
	}
	defer rows.Close()

	out := map[string][]string{}
	for rows.Next() {
		var momentID, assetID string
		var score float64
		if err := rows.Scan(&momentID, &assetID, &score); err != nil {
			return nil, fmt.Errorf("moments: scan top featured: %w", err)
		}
		if perMoment > 0 && len(out[momentID]) >= perMoment {
			continue
		}
		out[momentID] = append(out[momentID], assetID)
	}
	return out, rows.Err()
}

// CoverRatioByMoment 一次查询取出全库封面(cover_asset_id)的宽高比 w/h,
// JOIN asset_exif 取该封面 asset 的 EXIF 尺寸——INNER JOIN 天然实现"缺 exif
// 行不入 map"的语义(width/height 任一为 0 或该封面根本没有 asset_exif 行,
// 调用方对未出现的 id 均按 0=未知处理,由路由层落地为 JSON cover_ratio=0)。
// 与 TopFeaturedByMoment/AddedThisWeekByMoment 同法:一条整表查询,不对每个
// 时刻单独查询(无 N+1)。
func (s *MomentStore) CoverRatioByMoment() (map[string]float64, error) {
	rows, err := s.db.Query(`
		SELECT m.id, e.width, e.height
		FROM moments m
		JOIN asset_exif e ON e.asset_id = m.cover_asset_id`)
	if err != nil {
		return nil, fmt.Errorf("moments: cover ratio: %w", err)
	}
	defer rows.Close()

	out := map[string]float64{}
	for rows.Next() {
		var momentID string
		var width, height sql.NullInt64
		if err := rows.Scan(&momentID, &width, &height); err != nil {
			return nil, fmt.Errorf("moments: scan cover ratio: %w", err)
		}
		// width/height 任一缺失(NULL)或 <=0 均视为无效尺寸,不产出比例。
		if !width.Valid || !height.Valid || width.Int64 <= 0 || height.Int64 <= 0 {
			continue
		}
		out[momentID] = float64(width.Int64) / float64(height.Int64)
	}
	return out, rows.Err()
}

// sevenDaysMs 是 AddedThisWeekByMoment 的统计窗口(7 天,毫秒)。
const sevenDaysMs = int64(7 * 24 * 60 * 60 * 1000)

// AddedThisWeekByMoment 一次查询统计全库每个时刻"本周新增"的成员数:
// added_at 非 NULL 且 >= nowMs-7d 才计入(NULL=存量/加入时间未知,不计,
// 避免上线首周全库照片都显示 +N)。与 TopFeaturedByMoment 同法,一条整表
// 查询 Go 侧按 moment_id 分组,不对每个时刻单独查询(无 N+1)。返回的 map
// 只包含 count>0 的 moment id;调用方对未出现的 id 应按 0 处理。
func (s *MomentStore) AddedThisWeekByMoment(nowMs int64) (map[string]int, error) {
	cutoff := nowMs - sevenDaysMs
	rows, err := s.db.Query(`
		SELECT moment_id, COUNT(*)
		FROM moment_assets
		WHERE added_at IS NOT NULL AND added_at >= ?
		GROUP BY moment_id`, cutoff)
	if err != nil {
		return nil, fmt.Errorf("moments: added this week: %w", err)
	}
	defer rows.Close()

	out := map[string]int{}
	for rows.Next() {
		var momentID string
		var count int
		if err := rows.Scan(&momentID, &count); err != nil {
			return nil, fmt.Errorf("moments: scan added this week: %w", err)
		}
		out[momentID] = count
	}
	return out, rows.Err()
}

// PlacesByMoment 返回某时刻成员按城市聚合的出现次数,供 About 多地点展示
// (spec 第三节):JOIN asset_geo,city 为空或该成员无 geo 行的不计入;按
// count DESC、city ASC(tie-break,保证结果确定性)排序,截取前 limit 条
// (limit<=0 视为不截断)。
func (s *MomentStore) PlacesByMoment(momentID string, limit int) ([]MomentPlace, error) {
	q := `
		SELECT g.city, COUNT(*) AS c
		FROM moment_assets ma
		JOIN asset_geo g ON g.asset_id = ma.asset_id
		WHERE ma.moment_id = ? AND g.city IS NOT NULL AND g.city <> ''
		GROUP BY g.city
		ORDER BY c DESC, g.city ASC`
	args := []interface{}{momentID}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("moments: places by moment %q: %w", momentID, err)
	}
	defer rows.Close()

	out := make([]MomentPlace, 0)
	for rows.Next() {
		var p MomentPlace
		if err := rows.Scan(&p.Name, &p.Count); err != nil {
			return nil, fmt.Errorf("moments: scan place %q: %w", momentID, err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// momentHasTimeWindow 判断某时刻当前是否有具体时间窗(time_from 非 NULL,
// 即 trip 类时刻);主题类时刻(time_from 恒 NULL)不应被派生刷新意外赋予
// 一个时间窗。
func momentHasTimeWindow(tx *sql.Tx, momentID string) (bool, error) {
	var from sql.NullString
	if err := tx.QueryRow(`SELECT time_from FROM moments WHERE id=?`, momentID).Scan(&from); err != nil {
		return false, fmt.Errorf("moments: check time window %q: %w", momentID, err)
	}
	return from.Valid, nil
}

// applyMomentEdits 是 SyncRecipeMoments 的回放钩子:成员 diff upsert 后,把
// 用户此前对该 moment 做过的 pin/exclude 编辑重新叠加回去。exclude 先剔除,
// pin 后并入(INSERT OR IGNORE 不会降级引擎本轮已纳入的成员——已是
// featured/score 有值的行原样保留,只在缺席时补一条 manual=1/featured=0/
// score=0/added_at=now 的行;因 SyncRecipeMoments 第 2 步已豁免 pin 成员的
// 删除,常态下 pin 成员已在表内,这里的 INSERT OR IGNORE 不会触发,added_at
// 不会被刷新)。返回值 hasEdits 表示该 moment 是否存在任何编辑记录,供调用方
// 决定是否需要派生刷新(没有编辑的 moment 维持引擎本轮算出的派生值,不必
// 多余重算)。now 是 SyncRecipeMoments 本轮的时间戳,仅用于真正新插入的
// pin 行补 added_at。
func applyMomentEdits(tx *sql.Tx, momentID string, now int64) (bool, error) {
	if _, err := tx.Exec(`
		DELETE FROM moment_assets
		WHERE moment_id = ?
		  AND asset_id IN (SELECT asset_id FROM moment_edits WHERE moment_id=? AND op='exclude')`,
		momentID, momentID,
	); err != nil {
		return false, fmt.Errorf("moments: replay exclude %q: %w", momentID, err)
	}
	// 回放补插只认活资产(aliveAssetExpr):死资产(回收站/离线)的 pin edits
	// 不会被补插回成员——它们已在上面的 SyncRecipeMoments 第 2 步(不再豁免)
	// 或本函数的 exclude 回放中被清出成员表。
	if _, err := tx.Exec(`
		INSERT OR IGNORE INTO moment_assets(moment_id, asset_id, featured, score, manual, added_at)
		SELECT me.moment_id, me.asset_id, 0, 0, 1, ?
		FROM moment_edits me
		JOIN assets a ON a.id = me.asset_id
		WHERE me.moment_id=? AND me.op='pin' AND `+aliveAssetExpr,
		now, momentID,
	); err != nil {
		return false, fmt.Errorf("moments: replay pin %q: %w", momentID, err)
	}
	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM moment_edits WHERE moment_id=?`, momentID).Scan(&count); err != nil {
		return false, fmt.Errorf("moments: replay count edits %q: %w", momentID, err)
	}
	return count > 0, nil
}

// refreshMomentDerived 是 pin/exclude 的成员变动后,重算该时刻派生字段的
// 公共实现:
//   - asset_count:成员表 COUNT(*)。
//   - 时间窗(仅 hadTimeWindow=true 时):按当前成员 JOIN assets 取
//     MIN/MAX(taken_at);hadTimeWindow=false(主题类时刻)时不触碰
//     time_from/time_to,保持其 NULL 语义。
//   - 封面:当前封面仍是成员则不动;否则按"featured 最高分 → 任一成员(按
//     score DESC, asset_id ASC 取第一,避免测试抖动)→ 无成员则 NULL"依次
//     回落重挑。
func refreshMomentDerived(tx *sql.Tx, momentID string, hadTimeWindow bool) error {
	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM moment_assets WHERE moment_id=?`, momentID).Scan(&count); err != nil {
		return fmt.Errorf("moments: refresh count %q: %w", momentID, err)
	}

	setClauses := []string{"asset_count=?"}
	args := []interface{}{count}

	if hadTimeWindow {
		var from, to sql.NullString
		if err := tx.QueryRow(`
			SELECT MIN(a.taken_at), MAX(a.taken_at)
			FROM moment_assets ma JOIN assets a ON a.id = ma.asset_id
			WHERE ma.moment_id=?`, momentID).Scan(&from, &to); err != nil {
			return fmt.Errorf("moments: refresh time window %q: %w", momentID, err)
		}
		var fromTime, toTime time.Time
		if t := parseSQLiteTime(from); t != nil {
			fromTime = *t
		}
		if t := parseSQLiteTime(to); t != nil {
			toTime = *t
		}
		setClauses = append(setClauses, "time_from=?", "time_to=?")
		args = append(args, nullTimeArg(fromTime), nullTimeArg(toTime))
	}

	var currentCover sql.NullString
	if err := tx.QueryRow(`SELECT cover_asset_id FROM moments WHERE id=?`, momentID).Scan(&currentCover); err != nil {
		return fmt.Errorf("moments: refresh read cover %q: %w", momentID, err)
	}
	coverStillMember := false
	if currentCover.Valid {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM moment_assets WHERE moment_id=? AND asset_id=?`,
			momentID, currentCover.String).Scan(&n); err != nil {
			return fmt.Errorf("moments: refresh check cover member %q: %w", momentID, err)
		}
		coverStillMember = n > 0
	}

	newCover := currentCover
	if !currentCover.Valid || !coverStillMember {
		var pick sql.NullString
		err := tx.QueryRow(`
			SELECT asset_id FROM moment_assets WHERE moment_id=? AND featured=1
			ORDER BY score DESC, asset_id ASC LIMIT 1`, momentID).Scan(&pick)
		if err == sql.ErrNoRows {
			err = tx.QueryRow(`
				SELECT asset_id FROM moment_assets WHERE moment_id=?
				ORDER BY score DESC, asset_id ASC LIMIT 1`, momentID).Scan(&pick)
		}
		if err != nil && err != sql.ErrNoRows {
			return fmt.Errorf("moments: refresh pick cover %q: %w", momentID, err)
		}
		newCover = pick // sql.ErrNoRows 时 pick 保持零值(Valid=false)→ 落回 NULL
	}
	setClauses = append(setClauses, "cover_asset_id=?", "updated_at=?")
	var coverArg interface{}
	if newCover.Valid {
		coverArg = newCover.String
	}
	args = append(args, coverArg, nowMs())
	args = append(args, momentID)

	q := fmt.Sprintf(`UPDATE moments SET %s WHERE id=?`, strings.Join(setClauses, ", "))
	if _, err := tx.Exec(q, args...); err != nil {
		return fmt.Errorf("moments: refresh update %q: %w", momentID, err)
	}
	return nil
}

// SetMomentTitle 把某时刻的展示名置为 LLM 润色结果,并标记
// named_by_llm=1——之后的 SyncRecipeMoments 重算会保留这个 title,不再被
// 模板结果覆盖。
func (s *MomentStore) SetMomentTitle(id, title string) error {
	_, err := s.db.Exec(`UPDATE moments SET title=?, named_by_llm=1, updated_at=? WHERE id=?`,
		title, nowMs(), id)
	if err != nil {
		return fmt.Errorf("moments: set title %q: %w", id, err)
	}
	return nil
}

// ReorderMoments 是拖拽排序的落库入口:事务内按 ids 的顺序依次把
// sort_order 赋为 (i+1)*10(留间隙,便于未来"插入到两者之间"而不必整体重排)。
// ids 里不存在的 moment id(如前端列表略旧、时刻已被重算删除)UPDATE 影响
// 0 行,忽略不报错——不因单个失效 id 让整批操作失败。空 ids 由调用方
// (handler)提前拦截为 400,这里不做该校验。
func (s *MomentStore) ReorderMoments(ids []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("moments: reorder begin: %w", err)
	}
	for i, id := range ids {
		sortOrder := (i + 1) * 10
		if _, err := tx.Exec(`UPDATE moments SET sort_order=? WHERE id=?`, sortOrder, id); err != nil {
			tx.Rollback()
			return fmt.Errorf("moments: reorder update %q: %w", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("moments: reorder commit: %w", err)
	}
	return nil
}

// ── 稳定 id 派生 ─────────────────────────────────────────────────────────

// hashID16 返回输入字符串 sha1 摘要的前 16 位十六进制字符(64 bit,足够
// 在本库单机 sqlite 规模下避免碰撞,且比全 40 位 hex 更适合做展示层 id)。
func hashID16(s string) string {
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}

// TripMomentID 派生旅行类时刻的稳定 id:hash(recipeKey + "|" + ISO 周)。
// 用 ISO 周(而非公历周或具体日期)是因为同一趟旅程重算时,切分出的
// 起始日期可能因新增/减少边界照片而微移几天,但只要还落在同一个 ISO 周
// 内,id 就不变——避免用户看到的时刻在重算间"改名换姓"。
func TripMomentID(recipeKey string, from time.Time) string {
	year, week := from.ISOWeek()
	weekKey := fmt.Sprintf("%d-W%02d", year, week)
	return hashID16(recipeKey + "|" + weekKey)
}

// ThemeMomentID 派生主题类时刻的稳定 id:hash(recipeKey)。主题类是"每
// recipe 一个滚动更新的活集合"(不像 trip 按时间窗分段),所以 id 只取决于
// recipe key,同一主题永远映射到同一个 moment 行,重算即原地刷新成员。
func ThemeMomentID(recipeKey string) string {
	return hashID16(recipeKey)
}
