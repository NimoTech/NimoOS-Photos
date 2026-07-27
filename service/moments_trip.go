// trip 引擎:把候选池中带 GPS 的照片按拍摄时间"全局"(不预先按城市分组)
// 切成若干旅行段,每段落地一个 trip 类型的 MomentDraft。
//
// 之所以"全局切段、事后按城市命名"而不是像 places.go 的 Visits 那样先按
// city_id 分组再各自切段:一趟真实的旅行常常跨越多个城市(如"东京→大阪"),
// 若先按城分组会把同一趟旅行拆成好几个"独立到访",trip 时刻要呈现的是用户
// 体感的"一趟旅程",所以先按时间全局聚出段,再用段内城市频次挑一个(或两个)
// 代表性地名。
package service

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

// tripCandidate 是候选池中一条待切段资产的最小信息。
type tripCandidate struct {
	id      string
	takenAt time.Time
	city    string
	country string
}

// tripSegment 是切段结果在有序候选切片中的下标区间(闭区间 [start, end])。
type tripSegment struct {
	start, end int
}

// secondCityRatio 是"第二城"被认可为双城命名的最低段内占比阈值:超过此
// 比例才认为该城不是偶然路过,值得并列进标题。
const secondCityRatio = 0.3

// BuildTripMoments 是 trip 引擎的纯函数入口:从候选池取有 GPS 的资产按拍摄
// 时间排序,按 recipe 的 GapDays 全局切段,段内量达 MinAssets 的产出一个
// MomentDraft(不落库,由调用方经 MomentStore.SyncRecipeMoments 幂等合并)。
func BuildTripMoments(ctx context.Context, db *sql.DB, recipe MomentRecipe) ([]MomentDraft, error) {
	params, err := ParseParams(recipe)
	if err != nil {
		return nil, err
	}

	items, err := loadTripCandidates(ctx, db)
	if err != nil {
		return nil, err
	}

	times := make([]time.Time, len(items))
	for i, it := range items {
		times[i] = it.takenAt
	}
	segs := splitByGap(times, params.GapDays)

	drafts := make([]MomentDraft, 0, len(segs))
	for _, seg := range segs {
		n := seg.end - seg.start + 1
		if n < params.MinAssets {
			continue // 小簇(如周末路过某地拍了几张)不足以成一趟旅行时刻。
		}
		segItems := items[seg.start : seg.end+1]
		from := segItems[0].takenAt
		to := segItems[len(segItems)-1].takenAt

		place := dominantPlace(segItems)
		title := "Trip"
		if place != "" {
			title = place + " Trip"
		}

		assets := make([]MomentAsset, len(segItems))
		for i, it := range segItems {
			// Score/Featured 先置零值,精选由 Task 3 的共用选优函数事后回填。
			assets[i] = MomentAsset{AssetID: it.id}
		}

		drafts = append(drafts, MomentDraft{
			Moment: Moment{
				ID:         TripMomentID(recipe.Key, from),
				RecipeKey:  recipe.Key,
				Title:      title,
				Subtitle:   tripSubtitle(from, to),
				Place:      place,
				TimeFrom:   from,
				TimeTo:     to,
				AssetCount: n,
			},
			Assets: assets,
		})
	}
	return drafts, nil
}

// loadTripCandidates 查询候选池:status='indexed'、非回收站(deleted_at IS
// NULL AND offline=0,与本库既有查询——见 embedder.go/faces.go 等——同一
// 判据)、排除文档(hasOcrExpr 取反,见 docscore.go:202)、排除
// is_live_photo_video(live photo 的 MOV 侧与其静态照片同一瞬间各自落一条
// asset_geo,不排除会在同一段内被双计,拉高计数/扭曲主城占比;与本库其它
// 15+ 处 geo JOIN 查询——places.go/persons.go/search.go/smartview.go
// 等——统一判据一致,而非参照 embedder/faces 这类非 geo 流水线),且必须
// 有 asset_geo(JOIN 天然过滤掉无 GPS 的资产),按拍摄时间升序返回。
// taken_at 相同时按 id 兜底排序,保证多轮调用切段结果确定、可复现。
func loadTripCandidates(ctx context.Context, db *sql.DB) ([]tripCandidate, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT a.id, a.taken_at, COALESCE(g.city,''), COALESCE(g.country,'')
		FROM assets a
		JOIN asset_geo g ON g.asset_id = a.id
		WHERE a.status='indexed' AND a.deleted_at IS NULL AND a.offline=0
		  AND a.is_live_photo_video=0
		  AND a.taken_at IS NOT NULL
		  AND NOT (`+hasOcrExpr+`)
		ORDER BY a.taken_at ASC, a.id ASC`)
	if err != nil {
		return nil, fmt.Errorf("moments: trip candidate query: %w", err)
	}
	defer rows.Close()

	var out []tripCandidate
	for rows.Next() {
		var it tripCandidate
		var ts sql.NullString
		if err := rows.Scan(&it.id, &ts, &it.city, &it.country); err != nil {
			return nil, fmt.Errorf("moments: scan trip candidate: %w", err)
		}
		t := parseSQLiteTime(ts)
		if t == nil {
			continue // taken_at 已在 WHERE 里限定非空,这里只是双重保险。
		}
		it.takenAt = *t
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("moments: iterate trip candidates: %w", err)
	}
	return out, nil
}

// splitByGap 把一个已按时间升序排好的序列,按相邻两点间隔 > gapDays 天切成
// 若干段;算法与 places.go:194-207 的 splitTrips 一致,区别是这里的输入是
// 跨城市合并后的全局序列,而不是单城市内的序列。
func splitByGap(times []time.Time, gapDays int) []tripSegment {
	if len(times) == 0 {
		return nil
	}
	gap := time.Duration(gapDays) * 24 * time.Hour
	segs := []tripSegment{{0, 0}}
	for i := 1; i < len(times); i++ {
		if times[i].Sub(times[i-1]) > gap {
			segs = append(segs, tripSegment{i, i})
		} else {
			segs[len(segs)-1].end = i
		}
	}
	return segs
}

// dominantPlace 返回段内的主城命名:出现频次最高的城市为主城;若存在第二
// 高频城市且其出现次数占段内资产总数超过 secondCityRatio,则返回
// "CityA & CityB"(A 为主城)。频次并列时按城市名字典序稳定排序,避免同
// 一份数据多次重算得到不同的命名顺序。空 city(reverse geocode 未产出地名)
// 不参与计数;若段内全部为空 city,返回空字符串(调用方回退 "Trip" 标题)。
func dominantPlace(items []tripCandidate) string {
	counts := map[string]int{}
	var order []string
	for _, it := range items {
		city := strings.TrimSpace(it.city)
		if city == "" {
			continue
		}
		if _, ok := counts[city]; !ok {
			order = append(order, city)
		}
		counts[city]++
	}
	if len(order) == 0 {
		return ""
	}
	sort.SliceStable(order, func(i, j int) bool {
		if counts[order[i]] != counts[order[j]] {
			return counts[order[i]] > counts[order[j]]
		}
		return order[i] < order[j]
	})
	top := order[0]
	if len(order) < 2 {
		return top
	}
	second := order[1]
	if float64(counts[second])/float64(len(items)) > secondCityRatio {
		return top + " & " + second
	}
	return top
}

// tripSubtitle 生成时刻卡片副标题:同月同年 "May 2011";跨月同年
// "May – Jun 2011"(en dash,两侧各一空格);跨年则两侧各带年份,如
// "Dec 2011 – Jan 2012"。
func tripSubtitle(from, to time.Time) string {
	if from.Year() == to.Year() && from.Month() == to.Month() {
		return from.Format("Jan 2006")
	}
	if from.Year() == to.Year() {
		return from.Format("Jan") + " – " + to.Format("Jan 2006")
	}
	return from.Format("Jan 2006") + " – " + to.Format("Jan 2006")
}
