// theme 引擎:把 recipe 的 clip_prompts(CLIP 语义检索)与 caption_keywords
// (caption 文本关键词)两路命中取并集,再与候选池(排除回收站/离线/文档/
// live photo 视频侧)取交集,达到 MinAssets 的产出一个"滚动更新"的主题时刻
// 草稿(见 ThemeMomentID 的设计:同一 recipe 永远映射同一个 moment id)。
package service

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

// AssetScore 是 clipTextSearcher 一次文本检索命中的最小结果:资产 id + 相似度
// 分数(约定 [0,1],与 Asset.MatchScore 同一量纲)。
type AssetScore struct {
	AssetID string
	Score   float64
}

// clipTextSearcher 是 theme 引擎需要的 CLIP 文本检索能力,真实现见
// SearchService.SearchAssetsByText(search.go);测试注入 fake,避免 theme 引擎
// 直接依赖 ML 层。
type clipTextSearcher interface {
	SearchAssetsByText(ctx context.Context, prompt string, topK int) ([]AssetScore, error)
}

// BuildThemeMoments 是 theme 引擎的入口:每条 ClipPrompts 走 searcher 取
// TopK、按 MinScore 过滤;CaptionKeywords 对 asset_caption 做
// instr(lower(text), kw) 匹配(命中记 MinScore 保底分);两路取并集(同一
// 资产被两路都命中时分数取 max)。并集再与候选池取交集(候选池判据与 trip
// 引擎一致,见 loadThemeCandidatePool),成员数达 MinAssets 才产出单个
// MomentDraft;不足则返回空切片(该 recipe 本轮没有可展示的主题时刻,不是
// 错误)。
func BuildThemeMoments(ctx context.Context, db *sql.DB, searcher clipTextSearcher, recipe MomentRecipe) ([]MomentDraft, error) {
	params, err := ParseParams(recipe)
	if err != nil {
		return nil, err
	}

	// score 是"资产 id → 本轮两路取并集后的最终分数"的累积表。
	score := map[string]float64{}

	for _, prompt := range params.ClipPrompts {
		hits, err := searcher.SearchAssetsByText(ctx, prompt, params.TopK)
		if err != nil {
			return nil, fmt.Errorf("moments: theme clip search %q: %w", prompt, err)
		}
		for _, h := range hits {
			if h.Score < params.MinScore {
				continue
			}
			if cur, ok := score[h.AssetID]; !ok || h.Score > cur {
				score[h.AssetID] = h.Score
			}
		}
	}

	if len(params.CaptionKeywords) > 0 {
		hits, err := matchCaptionKeywords(ctx, db, params.CaptionKeywords)
		if err != nil {
			return nil, err
		}
		for _, id := range hits {
			// 关键词命中没有连续相似度可用,记 MinScore 作保底分——刚好卡在
			// 过滤线上,既不会被自己的门槛刷掉,也不会喧宾夺主盖过 CLIP 的
			// 高置信命中(取 max 时 CLIP 分数更高则保留 CLIP 分数)。
			if cur, ok := score[id]; !ok || params.MinScore > cur {
				score[id] = params.MinScore
			}
		}
	}

	if len(score) == 0 {
		return nil, nil
	}

	pool, err := loadThemeCandidatePool(ctx, db)
	if err != nil {
		return nil, err
	}

	var assets []MomentAsset
	var from, to time.Time
	for id, s := range score {
		takenAt, ok := pool[id]
		if !ok {
			continue // 不在候选池(回收站/离线/文档/live photo 视频侧/无 taken_at)。
		}
		assets = append(assets, MomentAsset{AssetID: id, Score: s})
		if from.IsZero() || takenAt.Before(from) {
			from = takenAt
		}
		if to.IsZero() || takenAt.After(to) {
			to = takenAt
		}
	}

	if len(assets) < params.MinAssets {
		return nil, nil
	}

	// 按分数降序、同分按 id 排序,保证多轮重算的成员顺序确定、可复现(与
	// trip 引擎按 taken_at 排序同样的稳定性诉求)。
	sort.Slice(assets, func(i, j int) bool {
		if assets[i].Score != assets[j].Score {
			return assets[i].Score > assets[j].Score
		}
		return assets[i].AssetID < assets[j].AssetID
	})

	draft := MomentDraft{
		Moment: Moment{
			ID:         ThemeMomentID(recipe.Key),
			RecipeKey:  recipe.Key,
			Title:      recipe.Title,
			TimeFrom:   from,
			TimeTo:     to,
			AssetCount: len(assets),
		},
		Assets: assets,
	}
	return []MomentDraft{draft}, nil
}

// matchCaptionKeywords 返回 asset_caption 中文本(小写后)包含任一关键词
// (同样小写)的资产 id 去重列表。与 docscore/ocrSearch 既有的
// instr(lower(text), lower(?)) > 0 判据同款,一条 SQL 里 OR 起来所有关键词,
// 不区分具体命中了哪个词——theme 引擎只关心"命中与否"。
func matchCaptionKeywords(ctx context.Context, db *sql.DB, keywords []string) ([]string, error) {
	if len(keywords) == 0 {
		return nil, nil
	}
	clauses := make([]string, len(keywords))
	args := make([]interface{}, len(keywords))
	for i, kw := range keywords {
		clauses[i] = "instr(lower(text), ?) > 0"
		args[i] = strings.ToLower(kw)
	}
	q := `SELECT DISTINCT asset_id FROM asset_caption WHERE ` + strings.Join(clauses, " OR ")
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("moments: caption keyword query: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("moments: scan caption keyword hit: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// loadThemeCandidatePool 查询 theme 引擎的候选池:status='indexed'、非回收站
// (deleted_at IS NULL AND offline=0)、排除文档(hasOcrExpr 取反)、排除
// is_live_photo_video(与 trip 引擎的 loadTripCandidates 同一批判据,见
// moments_trip.go 顶部注释——理由同样适用于 theme:live photo 视频侧不该被当
// 成独立照片计入主题成员),且要求 taken_at 非空(否则无法纳入
// TimeFrom/TimeTo 的成员时间范围计算)。返回 id → taken_at 的映射,供
// BuildThemeMoments 与两路命中的并集取交集。
func loadThemeCandidatePool(ctx context.Context, db *sql.DB) (map[string]time.Time, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT a.id, a.taken_at
		FROM assets a
		WHERE a.status='indexed' AND a.deleted_at IS NULL AND a.offline=0
		  AND a.is_live_photo_video=0
		  AND a.taken_at IS NOT NULL
		  AND NOT (`+hasOcrExpr+`)`)
	if err != nil {
		return nil, fmt.Errorf("moments: theme candidate query: %w", err)
	}
	defer rows.Close()

	out := map[string]time.Time{}
	for rows.Next() {
		var id string
		var ts sql.NullString
		if err := rows.Scan(&id, &ts); err != nil {
			return nil, fmt.Errorf("moments: scan theme candidate: %w", err)
		}
		t := parseSQLiteTime(ts)
		if t == nil {
			continue // taken_at 已在 WHERE 里限定非空,这里只是双重保险。
		}
		out[id] = *t
	}
	return out, rows.Err()
}
