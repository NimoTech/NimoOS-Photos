// pet 实体画像引擎:把"全库搜含宠物元素"的概念版(theme:pets)升级为"用户
// 自己的那只狗/猫"的实体归纳——区分信号是复现性:自己的宠物跨月/跨年反复
// 出现,路遇的狗只出现一次(见设计 spec 产品动机)。
//
// 两段职责:
//   - MinePetEntities:纯挖掘,lexicon 逐词(含短语)做 caption 词边界匹配,
//     统计张数/跨月数,达标才归纳为一个 ProfileEntity,不落库、不碰
//     moments 表。挖掘同时消费用户对该实体既有的 pin/exclude 反馈(见下)。
//   - BuildPetEntityMoments:挖掘 → ReplaceEntities 落画像表 → 每个达标实体
//     组装一个 MomentDraft(词命中 ∪ CLIP 检索的成员并集),供
//     MomentsService.recomputeRecipe 走共用选优/落库管线,与 trip/theme
//     两个引擎同一套装配方式。
//
// pin/exclude 反馈消费(Task 3):实体的 moment id 由既有派生函数
// (ProfileEntityID)预先算出 → MomentStore.MomentEditsFor 读取该实体当前
// 生效的编辑 → exclude 命中的资产从匹配集剔除、pin 命中的资产(需在候选池
// 里真实存在,即有合法 taken_at)并入匹配集,一并参与 min_photos/min_months
// 达标判定与 first/last seen 统计。这是必要的:moment_assets 成员表的最终
// 订正已由存储层 applyMomentEdits 在每轮 SyncRecipeMoments 里统一回放
// (family/theme/trip 也吃这条通用回放,不需要各自的挖掘级消费),但"这个
// 实体是否还够格被认定为用户自己的宠物"这一判定、以及画像/卡片副标题用的
// first/last seen,只在这里的挖掘统计里产生,存储层的通用回放够不着——
// exclude 剔干净了导致跌破门槛,实体就该从这里的输出中消失(联动
// BuildPetEntityMoments 不再产出 draft、SyncRecipeMoments 的 stale-delete
// 把陈旧时刻连同 moment_assets/moment_edits 一并级联清掉)。
package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
)

// petEvidence 是 pet 实体挖掘依据的 JSON 快照结构,落 ProfileEntity.EvidenceJSON,
// 供排障与后续升级读取,不参与查询过滤。
type petEvidence struct {
	PhotoCount int    `json:"photo_count"`
	Months     int    `json:"months"`
	First      string `json:"first"`
	Last       string `json:"last"`
}

// MinePetEntities 是 pet 画像挖掘的纯函数入口:对 recipe.Lexicon 每个物种/
// 品种词(可能是多词短语,如 "maine coon"/"boxer dog")做 caption 词边界
// 匹配(复用 matchCaptionKeywords 的精滤思路——SQL instr 粗筛 + Go 正则
// `\bkw\b` 精滤,\b 对多词短语天然按整短语边界匹配,不会被短语里的单个词
// 误触发,如裸 "boxer" 不会命中 "boxer dog"),与既有候选池
// (loadThemeCandidatePool,排除回收站/离线/文档/live photo 视频侧,同
// theme/trip 引擎判据)取交集,统计张数与 distinct 年月数;
// photo_count >= MinPhotos 且 months >= MinMonths 才归纳为一个
// ProfileEntity(复现性判据)。返回按 Key 字典序排列,保证多轮挖掘结果顺序
// 确定、可复现。Lexicon 为空时返回空(未配置词表,不是错误)。store 用于读取
// 每个实体既有的 pin/exclude 编辑反馈(见文件头注释),据此订正匹配集后再
// 判定达标与统计 first/last seen。
func MinePetEntities(ctx context.Context, db *sql.DB, store *MomentStore, recipe MomentRecipe) ([]ProfileEntity, error) {
	params, err := ParseParams(recipe)
	if err != nil {
		return nil, err
	}
	if len(params.Lexicon) == 0 {
		return nil, nil
	}

	pool, err := loadThemeCandidatePool(ctx, db)
	if err != nil {
		return nil, err
	}

	var out []ProfileEntity
	for _, species := range params.Lexicon {
		hits, err := matchCaptionKeywords(ctx, db, []string{species})
		if err != nil {
			return nil, fmt.Errorf("moments: pet lexicon match %q: %w", species, err)
		}

		// 消费该实体既有的 pin/exclude 反馈:moment id 与 BuildPetEntityMoments
		// 落库时用的 e.ID 同一派生法,预先算出即可直接查编辑记录。exclude
		// 命中的资产从匹配集剔除,pin 命中的资产(视作用户确认样本)并入,
		// 二者一起参与下面的达标判定与 first/last seen 统计——否则用户"这
		// 不是我的狗"的反馈只会体现在成员表,下一轮重算的挖掘统计仍会把它
		// 悄悄吞回来。
		momentID := ProfileEntityID("pet", species)
		pins, excludes, err := store.MomentEditsFor(momentID)
		if err != nil {
			return nil, fmt.Errorf("moments: pet entity edits %q: %w", momentID, err)
		}
		excludeSet := make(map[string]bool, len(excludes))
		for _, id := range excludes {
			excludeSet[id] = true
		}
		matched := make(map[string]bool, len(hits)+len(pins))
		for _, id := range hits {
			if !excludeSet[id] {
				matched[id] = true
			}
		}
		for _, id := range pins {
			if !excludeSet[id] { // pin 与 exclude 同时存在理论不该发生(后写覆盖先写),这里保守以 exclude 优先。
				matched[id] = true
			}
		}

		var photoCount int
		var first, last time.Time
		months := map[string]bool{}
		for id := range matched {
			takenAt, ok := pool[id]
			if !ok {
				continue // 不在候选池(回收站/离线/文档/live photo 视频侧/无 taken_at)——pin 的资产也需要真实存在于候选池才能纳入统计。
			}
			photoCount++
			months[takenAt.Format("2006-01")] = true
			if first.IsZero() || takenAt.Before(first) {
				first = takenAt
			}
			if last.IsZero() || takenAt.After(last) {
				last = takenAt
			}
		}

		if photoCount < params.MinPhotos || len(months) < params.MinMonths {
			continue // 复现性不足:路人的狗/一次性偶遇,不归纳为用户自己的宠物。
		}

		ev, _ := json.Marshal(petEvidence{
			PhotoCount: photoCount,
			Months:     len(months),
			First:      first.UTC().Format("2006-01-02"),
			Last:       last.UTC().Format("2006-01-02"),
		})

		out = append(out, ProfileEntity{
			ID:           momentID,
			Kind:         "pet",
			Key:          species,
			Label:        titleCasePhrase(species),
			EvidenceJSON: string(ev),
			PhotoCount:   photoCount,
			FirstSeen:    first,
			LastSeen:     last,
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// BuildPetEntityMoments 是 pet 实体时刻引擎入口:先 MinePetEntities 挖掘全库
// 达标宠物实体 → profileStore.ReplaceEntities("pet", ...) 幂等落画像表
// (无达标实体也要以空集调用,清空上一轮画像——如用户的狗走丢了/词表调整
// 导致不再命中时,画像不该残留过时数据)→ 每个达标实体产出一个
// MomentDraft:成员 = 该词 caption 词边界命中 ∪ CLIP("a photo of a "+
// species,ClipMinScore/ClipTopK 过滤)之交候选池,Score 取 CLIP 分,词边界
// 命中但未被 CLIP 命中的资产记 ClipMinScore 保底分(与 theme 引擎两路取
// 并集同一手法,见 BuildThemeMoments)。TimeFrom/TimeTo 取自挖掘阶段算出的
// first/last(词命中口径,与实体的达标判据一致,不随 CLIP 并集额外延展)。
// 精选/封面由调用方 MomentsService.recomputeRecipe 事后经
// PickFeaturedAndCover 统一填充,这里只产出成员与初始分数。无达标实体返回
// 空切片(不是错误)。store 透传给 MinePetEntities 用于消费 pin/exclude 反馈
// (TimeFrom/TimeTo 取自挖掘阶段已订正的 first/last,天然带出订正结果;成员
// 列表本身的 pin/exclude 订正由存储层 SyncRecipeMoments 的通用回放兜底,这里
// 不重复处理,与 trip/theme/family 三个引擎一致)。
func BuildPetEntityMoments(ctx context.Context, db *sql.DB, searcher clipTextSearcher, profileStore *ProfileStore, store *MomentStore, recipe MomentRecipe) ([]MomentDraft, error) {
	entities, err := MinePetEntities(ctx, db, store, recipe)
	if err != nil {
		return nil, err
	}
	if err := profileStore.ReplaceEntities("pet", entities); err != nil {
		return nil, fmt.Errorf("moments: replace pet entities: %w", err)
	}
	if len(entities) == 0 {
		return nil, nil
	}

	params, err := ParseParams(recipe)
	if err != nil {
		return nil, err
	}
	pool, err := loadThemeCandidatePool(ctx, db)
	if err != nil {
		return nil, err
	}

	drafts := make([]MomentDraft, 0, len(entities))
	for _, e := range entities {
		species := e.Key

		score := map[string]float64{}

		wordHits, err := matchCaptionKeywords(ctx, db, []string{species})
		if err != nil {
			return nil, fmt.Errorf("moments: pet entity word hits %q: %w", species, err)
		}
		for _, id := range wordHits {
			score[id] = params.ClipMinScore // 保底分,可能被下面更高的 CLIP 分覆盖。
		}

		clipHits, err := searcher.SearchAssetsByText(ctx, "a photo of a "+species, params.ClipTopK)
		if err != nil {
			return nil, fmt.Errorf("moments: pet entity clip search %q: %w", species, err)
		}
		for _, h := range clipHits {
			if h.Score < params.ClipMinScore {
				continue
			}
			if cur, ok := score[h.AssetID]; !ok || h.Score > cur {
				score[h.AssetID] = h.Score
			}
		}

		var assets []MomentAsset
		for id, s := range score {
			if _, ok := pool[id]; !ok {
				continue // 不在候选池(回收站/离线/文档/live photo 视频侧)。
			}
			assets = append(assets, MomentAsset{AssetID: id, Score: s})
		}
		sort.Slice(assets, func(i, j int) bool {
			if assets[i].Score != assets[j].Score {
				return assets[i].Score > assets[j].Score
			}
			return assets[i].AssetID < assets[j].AssetID
		})

		drafts = append(drafts, MomentDraft{
			Moment: Moment{
				ID:         e.ID,
				RecipeKey:  recipe.Key,
				Title:      "Your " + e.Label,
				Subtitle:   petEntitySubtitle(e.FirstSeen, e.LastSeen),
				TimeFrom:   e.FirstSeen,
				TimeTo:     e.LastSeen,
				AssetCount: len(assets),
			},
			Assets: assets,
		})
	}

	return drafts, nil
}

// petEntitySubtitle 生成 pet 实体时刻卡片副标题:年份跨度,同年只写一年
// ("2020"),跨年则 en dash 两侧各一空格("2011 – 2026")。与 tripSubtitle
// 的月份粒度不同——pet 实体常年复现,首末张跨度往往数年,年份粒度更贴切,
// 精确到月反而琐碎。
func petEntitySubtitle(from, to time.Time) string {
	if from.Year() == to.Year() {
		return fmt.Sprintf("%d", from.Year())
	}
	return fmt.Sprintf("%d", from.Year()) + " – " + fmt.Sprintf("%d", to.Year())
}

// titleCasePhrase 把一个(可能多词的)小写短语转成 Title Case,逐词首字母
// 大写:"beagle" -> "Beagle"、"maine coon" -> "Maine Coon"、"boxer dog" ->
// "Boxer Dog"。不用标准库 strings.Title(已 deprecated 且对本场景够用的
// ASCII 单词就是多此一举),手写更清晰。
func titleCasePhrase(s string) string {
	words := strings.Fields(s)
	for i, w := range words {
		if w == "" {
			continue
		}
		r := []rune(w)
		r[0] = unicode.ToUpper(r[0])
		words[i] = string(r)
	}
	return strings.Join(words, " ")
}
