// Smart Moments 数据层:三表(moment_recipes/moments/moment_assets)的 repo。
//
// moment_recipes 是"时刻类型=数据"的热更新载体——engine 按 kind 分派算法,
// clip_prompts/caption_keywords/阈值等参数纯数据上新,PUT recipes 即可生效,
// 无需改代码(真正需要新算法的 kind 除外)。
//
// moments/moment_assets 是活实体:稳定派生 id(TripMomentID/ThemeMomentID),
// 每轮重算按 id upsert + 成员全量替换(delete+insert),用户看到的时刻不因
// 重算而闪断;LLM 已命名过的 title(named_by_llm=1)重算时原样保留,只有
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
}

// MomentAsset 对应 moment_assets 表一行。
type MomentAsset struct {
	AssetID  string
	Featured bool
	Score    float64
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
// + 成员全量替换(delete+insert moment_assets);随后删除该 recipeKey 下
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

		// 成员全量替换:先清空旧成员,再整批插入本轮结果,事务内完成,
		// 不会出现"清空后插入前"的空窗被并发读到。
		if _, err := tx.Exec(`DELETE FROM moment_assets WHERE moment_id=?`, d.ID); err != nil {
			tx.Rollback()
			return fmt.Errorf("moments: clear members %q: %w", d.ID, err)
		}
		for _, a := range d.Assets {
			featured := 0
			if a.Featured {
				featured = 1
			}
			if _, err := tx.Exec(`
				INSERT INTO moment_assets(moment_id, asset_id, featured, score) VALUES (?, ?, ?, ?)`,
				d.ID, a.AssetID, featured, a.Score,
			); err != nil {
				tx.Rollback()
				return fmt.Errorf("moments: insert member %q/%q: %w", d.ID, a.AssetID, err)
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
		       place, asset_count, named_by_llm, created_at, updated_at, sort_order
		FROM moments
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
	if err := row.Scan(&m.ID, &m.RecipeKey, &m.Title, &m.Subtitle, &cover, &from, &to,
		&m.Place, &m.AssetCount, &namedByLLM, &m.CreatedAt, &m.UpdatedAt, &sortOrder); err != nil {
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
	return m, nil
}

// GetMomentAssets 返回某时刻的成员,按 score 倒序;featuredOnly=true 时只
// 返回精选(featured=1)成员。
func (s *MomentStore) GetMomentAssets(id string, featuredOnly bool) ([]MomentAsset, error) {
	q := `SELECT asset_id, featured, score FROM moment_assets WHERE moment_id=?`
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
		var featured int
		if err := rows.Scan(&a.AssetID, &featured, &a.Score); err != nil {
			return nil, fmt.Errorf("moments: scan member: %w", err)
		}
		a.Featured = featured != 0
		out = append(out, a)
	}
	return out, rows.Err()
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
