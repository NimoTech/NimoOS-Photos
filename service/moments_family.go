// family 画像引擎:把"用户自己家人"从人脸聚类结果里挖出来——高频出现的
// person(persons 表)归纳为画像实体,再产出两类时刻:高频人物同框的"合影
// 集",以及已命名人物各自的"Through the Years"。区分信号沿用画像层一贯
// 逻辑——复现性(自己家人反复出现,不像宠物有词表可匹配,家人靠人脸聚类
// 频次判断),详见设计 spec 第一/二节。
//
// 三段职责:
//   - MinePersonEntities:纯挖掘,统计 persons 出现频次(distinct asset,
//     排除 excluded 人脸与 hidden 人物,join 写法照 persons.go
//     ListPersons),达标(>= MinPersonPhotos)取前 TopPersons,不落库、不碰
//     moments 表。
//   - BuildFamilyMoments:挖掘 → ReplaceEntities 落画像表 → 合影集 draft
//     (top 实体中 ≥ MinTogetherPersons 人同框)+ 具名人物 drafts(label
//     非空者各一个),供 MomentsService.recomputeRecipe 走共用选优/落库
//     管线,与 pet/trip/theme 引擎同一套装配方式。
package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// personEvidence 是 person 实体挖掘依据的 JSON 快照结构,落
// ProfileEntity.EvidenceJSON,供排障与后续升级读取,不参与查询过滤。
type personEvidence struct {
	PhotoCount int    `json:"photo_count"`
	First      string `json:"first"`
	Last       string `json:"last"`
}

// personFreq 是 person 出现频次挖掘的一行中间结果(未落 ProfileEntity 前)。
type personFreq struct {
	personID string
	name     string
	count    int
	first    time.Time
	last     time.Time
}

// MinePersonEntities 是 family 画像挖掘的纯函数入口:统计 persons 出现频次
// (distinct asset,join 写法照 persons.go ListPersons——face_person join
// face_detections 且 excluded=0、join persons 且 hidden=0),assets 侧判据
// 逐字对齐 loadThemeCandidatePool(moments_theme.go)的候选池口径——
// status='indexed'、非回收站(deleted_at IS NULL AND offline=0)、排除文档
// (hasOcrExpr 取反)、排除 is_live_photo_video、要求 taken_at 非空——否则
// 频次达标计数会虚高于成员侧实际可用的候选池(成员侧 buildTogetherDraft/
// buildNamedPersonDraft 都要与候选池取交集,若挖掘口径更宽松,可能出现
// "达标了但成员数怎么都凑不够 MinAssets"的口径落差)。
// photo_count >= MinPersonPhotos 才达标;达标者按频次降序(并列按
// person_id 字典序稳定,保证多轮挖掘结果确定、可复现)取前 TopPersons。
// 未命名人物(persons.name 为空)同样参与挖掘、可以入选实体——"具名"的
// 门槛只在 BuildFamilyMoments 产出个人 draft 时才生效,画像表本身如实记录
// 挖掘结果,不因未命名而排除(与用户后续去 People 页面补名字的时序解耦)。
func MinePersonEntities(ctx context.Context, db *sql.DB, recipe MomentRecipe) ([]ProfileEntity, error) {
	params, err := ParseParams(recipe)
	if err != nil {
		return nil, err
	}

	rows, err := db.QueryContext(ctx, `
		SELECT fp.person_id, COALESCE(p.name,''), COUNT(DISTINCT fd.asset_id) AS cnt,
		       MIN(a.taken_at), MAX(a.taken_at)
		FROM face_person fp
		JOIN face_detections fd ON fd.id=fp.face_id AND fd.excluded=0
		JOIN persons p ON p.id=fp.person_id AND p.hidden=0
		JOIN assets a ON a.id=fd.asset_id AND a.status='indexed' AND a.deleted_at IS NULL AND a.offline=0 AND a.is_live_photo_video=0
		WHERE a.taken_at IS NOT NULL AND NOT (`+hasOcrExpr+`)
		GROUP BY fp.person_id
		HAVING cnt >= ?
		ORDER BY cnt DESC, fp.person_id ASC`, params.MinPersonPhotos)
	if err != nil {
		return nil, fmt.Errorf("moments: person frequency query: %w", err)
	}
	defer rows.Close()

	var freqs []personFreq
	for rows.Next() {
		var f personFreq
		var first, last sql.NullString
		if err := rows.Scan(&f.personID, &f.name, &f.count, &first, &last); err != nil {
			return nil, fmt.Errorf("moments: scan person frequency: %w", err)
		}
		if t := parseSQLiteTime(first); t != nil {
			f.first = *t
		}
		if t := parseSQLiteTime(last); t != nil {
			f.last = *t
		}
		freqs = append(freqs, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("moments: iterate person frequency: %w", err)
	}

	if len(freqs) > params.TopPersons {
		freqs = freqs[:params.TopPersons]
	}

	out := make([]ProfileEntity, 0, len(freqs))
	for _, f := range freqs {
		ev, _ := json.Marshal(personEvidence{
			PhotoCount: f.count,
			First:      f.first.UTC().Format("2006-01-02"),
			Last:       f.last.UTC().Format("2006-01-02"),
		})
		out = append(out, ProfileEntity{
			ID:           ProfileEntityID("person", f.personID),
			Kind:         "person",
			Key:          f.personID,
			Label:        f.name,
			EvidenceJSON: string(ev),
			PhotoCount:   f.count,
			FirstSeen:    f.first,
			LastSeen:     f.last,
		})
	}
	return out, nil
}

// BuildFamilyMoments 是 family 引擎入口:先 MinePersonEntities 挖掘全库
// 达标高频人物 → profileStore.ReplaceEntities("person", ...) 幂等落画像表
// (无达标实体也要以空集调用,清空上一轮画像)→ 产出两类 draft:
//   - 合影集(top 实体中 ≥ MinTogetherPersons 人同框的照片,至多一个,id 固定
//     为 ProfileEntityID("family","together"));
//   - 具名人物时刻(label 非空的实体各一个,id=实体 id,成员=该人全部照片)。
//
// 两类 draft 的成员都要与候选池(loadThemeCandidatePool,排除回收站/离线/
// 文档/live photo 视频侧,同 theme/trip 引擎判据)取交集,且 >= MinAssets
// 才产出;成员 Score 统一置 0——不像 pet/theme 引擎有 CLIP 分数可用,精选交
// 给 PickFeaturedAndCover 事后按美学分挑(与 trip 引擎同法,见
// BuildTripMoments)。无达标实体返回空切片(不是错误)。
func BuildFamilyMoments(ctx context.Context, db *sql.DB, profileStore *ProfileStore, recipe MomentRecipe) ([]MomentDraft, error) {
	entities, err := MinePersonEntities(ctx, db, recipe)
	if err != nil {
		return nil, err
	}
	if err := profileStore.ReplaceEntities("person", entities); err != nil {
		return nil, fmt.Errorf("moments: replace person entities: %w", err)
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

	var drafts []MomentDraft

	together, err := buildTogetherDraft(ctx, db, entities, pool, recipe, params)
	if err != nil {
		return nil, err
	}
	if together != nil {
		drafts = append(drafts, *together)
	}

	for _, e := range entities {
		if e.Label == "" {
			// 未命名人物不产出个人 draft:避免展示 "Person 1" 这类无意义占位名,
			// 自然激励用户去 People 页面命名(见设计 spec 第二节)。
			continue
		}
		draft, err := buildNamedPersonDraft(ctx, db, e, pool, recipe, params)
		if err != nil {
			return nil, err
		}
		if draft != nil {
			drafts = append(drafts, *draft)
		}
	}

	return drafts, nil
}

// buildTogetherDraft 查询 top 实体中 ≥ MinTogetherPersons 人同框的照片
// (face_person/face_detections join,GROUP BY asset HAVING COUNT(DISTINCT
// person_id) >= N,excluded 排除惯例同 MinePersonEntities),与候选池取交集,
// 达标(>= MinAssets)才返回 draft,否则返回 nil(不是错误)。
func buildTogetherDraft(ctx context.Context, db *sql.DB, entities []ProfileEntity, pool map[string]time.Time, recipe MomentRecipe, params RecipeParams) (*MomentDraft, error) {
	personIDs := make([]string, len(entities))
	for i, e := range entities {
		personIDs[i] = e.Key
	}
	if len(personIDs) == 0 {
		// 防御:调用方 BuildFamilyMoments 在 entities 为空时已提前返回,这里
		// 理论不可达;但 strings.Repeat(..., -1) 会 panic,加一行防御比依赖
		// "调用方总是先检查"更稳妥。
		return nil, nil
	}
	placeholders := strings.Repeat("?,", len(personIDs)-1) + "?"
	args := make([]interface{}, 0, len(personIDs)+1)
	for _, id := range personIDs {
		args = append(args, id)
	}
	args = append(args, params.MinTogetherPersons)

	q := fmt.Sprintf(`
		SELECT fd.asset_id
		FROM face_person fp
		JOIN face_detections fd ON fd.id=fp.face_id AND fd.excluded=0
		WHERE fp.person_id IN (%s)
		GROUP BY fd.asset_id
		HAVING COUNT(DISTINCT fp.person_id) >= ?`, placeholders)
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("moments: family together query: %w", err)
	}
	defer rows.Close()

	var assets []MomentAsset
	var from, to time.Time
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("moments: scan together asset: %w", err)
		}
		takenAt, ok := pool[id]
		if !ok {
			continue // 不在候选池(回收站/离线/文档/live photo 视频侧/无 taken_at)。
		}
		assets = append(assets, MomentAsset{AssetID: id})
		if from.IsZero() || takenAt.Before(from) {
			from = takenAt
		}
		if to.IsZero() || takenAt.After(to) {
			to = takenAt
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("moments: iterate together asset: %w", err)
	}
	if len(assets) < params.MinAssets {
		return nil, nil
	}
	sort.Slice(assets, func(i, j int) bool { return assets[i].AssetID < assets[j].AssetID })

	return &MomentDraft{
		Moment: Moment{
			ID:         ProfileEntityID("family", "together"),
			RecipeKey:  recipe.Key,
			Title:      "Family Moments",
			Subtitle:   petEntitySubtitle(from, to),
			TimeFrom:   from,
			TimeTo:     to,
			AssetCount: len(assets),
		},
		Assets: assets,
	}, nil
}

// buildNamedPersonDraft 查询某具名人物(label 非空)的全部照片(排除
// excluded 人脸),与候选池取交集,达标(>= MinAssets)才返回 draft,否则
// 返回 nil(不是错误)。
func buildNamedPersonDraft(ctx context.Context, db *sql.DB, e ProfileEntity, pool map[string]time.Time, recipe MomentRecipe, params RecipeParams) (*MomentDraft, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT DISTINCT fd.asset_id
		FROM face_person fp
		JOIN face_detections fd ON fd.id=fp.face_id AND fd.excluded=0
		WHERE fp.person_id=?`, e.Key)
	if err != nil {
		return nil, fmt.Errorf("moments: named person asset query %q: %w", e.Key, err)
	}
	defer rows.Close()

	var assets []MomentAsset
	var from, to time.Time
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("moments: scan named person asset: %w", err)
		}
		takenAt, ok := pool[id]
		if !ok {
			continue // 不在候选池(回收站/离线/文档/live photo 视频侧/无 taken_at)。
		}
		assets = append(assets, MomentAsset{AssetID: id})
		if from.IsZero() || takenAt.Before(from) {
			from = takenAt
		}
		if to.IsZero() || takenAt.After(to) {
			to = takenAt
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("moments: iterate named person asset: %w", err)
	}
	if len(assets) < params.MinAssets {
		return nil, nil
	}
	sort.Slice(assets, func(i, j int) bool { return assets[i].AssetID < assets[j].AssetID })

	return &MomentDraft{
		Moment: Moment{
			ID:         e.ID,
			RecipeKey:  recipe.Key,
			Title:      e.Label + " Through the Years",
			Subtitle:   petEntitySubtitle(from, to),
			TimeFrom:   from,
			TimeTo:     to,
			AssetCount: len(assets),
		},
		Assets: assets,
	}, nil
}
