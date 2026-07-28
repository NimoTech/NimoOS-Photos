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
}

// 默认值:recipe 未显式指定(或字段缺省为零值)时的兜底,详见简报。
const (
	defaultMinAssets   = 10
	defaultMaxFeatured = 12
	defaultGapDays     = 14
	defaultTopK        = 200
	defaultMinScore    = 0.2
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

// ListMoments 列出全部时刻,按 updated_at 倒序(最近重算/命名过的排前面)。
func (s *MomentStore) ListMoments() ([]Moment, error) {
	rows, err := s.db.Query(`
		SELECT id, recipe_key, title, subtitle, cover_asset_id, time_from, time_to,
		       place, asset_count, named_by_llm, created_at, updated_at
		FROM moments ORDER BY updated_at DESC`)
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
// time_from/time_to 可能为 NULL 的情况。
func scanMoment(row momentScanner) (Moment, error) {
	var m Moment
	var cover sql.NullString
	var from, to sql.NullString
	var namedByLLM int
	if err := row.Scan(&m.ID, &m.RecipeKey, &m.Title, &m.Subtitle, &cover, &from, &to,
		&m.Place, &m.AssetCount, &namedByLLM, &m.CreatedAt, &m.UpdatedAt); err != nil {
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
