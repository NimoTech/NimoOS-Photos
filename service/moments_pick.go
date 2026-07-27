// 共用选优:trip/theme 两个引擎产出的候选成员集,靠这里统一的连拍去重 +
// 精选/封面挑选逻辑决定"哪些照片值得展示"——引擎本身只管"该不该属于这个
// 时刻",不管"该不该被推到前排"。
package service

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

// clipVecLoader 按 asset_id 返回 CLIP 图向量;ok=false 表示无向量(未嵌入,或
// ScenesEnabled 关闭)。PickFeaturedAndCover 对这类资产跳过连拍去重、直接放
// 入精选候选池(没有向量就无法判断"是不是同一张连拍",宁可不去重也不要
// 误删)。
type clipVecLoader func(assetID string) ([]float32, bool)

// RealClipVecLoader 是 clipVecLoader 的生产实现,包一层 readClipVector(见
// docverdict.go:14-24)。测试用 fake 注入,不走这个。
func RealClipVecLoader(db *sql.DB) clipVecLoader {
	return func(assetID string) ([]float32, bool) {
		v := readClipVector(db, assetID)
		return v, v != nil
	}
}

// burstWindowSeconds 是连拍分簇的时间窗:相邻两张照片拍摄时间间隔 <=
// 此值才可能属于同一连拍簇(簇内再用向量相似度判定是否真的是连拍)。
const burstWindowSeconds = 60

// burstCosineThreshold 是连拍去重的余弦相似度门槛:同一时间簇内两张照片的
// CLIP 向量余弦相似度 > 此值即判定为连拍(同一场景的连续快门),簇内只留
// 美学分最高者进入精选候选池。
const burstCosineThreshold = 0.95

// pickAssetInfo 是 PickFeaturedAndCover 计算过程中每个资产需要的最小信息。
type pickAssetInfo struct {
	takenAt      time.Time
	aesthetic    float64
	hasAesthetic bool
}

// PickFeaturedAndCover 是 trip/theme 引擎共用的选优:按拍摄时间 60s 相邻窗把
// assets 分簇 → 簇内按 CLIP 向量两两余弦相似度 > 0.95 判连拍(同一连拍组只
// 保留 aesthetic_score 最高者进精选候选池,其余仍是该时刻成员,只是不参与
// 精选/不作为封面候选)→ 候选池按 aesthetic_score 降序取前 maxFeatured 张为
// 精选,首张(分数最高)为封面。向量缺失的资产跳过去重步骤,直接进候选池。
//
// 注意:aesthetic_score 是探针头模型的输出,只在同一批候选内做相对排序
// (谁比谁"更值得展示"),不是绝对质量分——禁止拿它做跨批次/跨时刻的绝对
// 阈值判断(比如"低于 0.4 就不能当封面"这种规则是错的)。
func PickFeaturedAndCover(ctx context.Context, db *sql.DB, assets []MomentAsset, maxFeatured int, loadVec clipVecLoader) ([]string, string, error) {
	if len(assets) == 0 {
		return nil, "", nil
	}

	info, err := loadPickAssetInfo(ctx, db, assets)
	if err != nil {
		return nil, "", err
	}

	ordered := make([]string, len(assets))
	for i, a := range assets {
		ordered[i] = a.AssetID
	}
	// 按 taken_at 升序排列;没有 taken_at 的排到最后(SliceStable 维持它们
	// 相对原始顺序不变)——没有时间锚点就无法判断"是否相邻",在
	// clusterByTimeWindow 里各自单独成簇。
	sort.SliceStable(ordered, func(i, j int) bool {
		hi, hj := !info[ordered[i]].takenAt.IsZero(), !info[ordered[j]].takenAt.IsZero()
		if hi != hj {
			return hi
		}
		if hi && hj {
			return info[ordered[i]].takenAt.Before(info[ordered[j]].takenAt)
		}
		return false
	})

	var poolIDs []string
	for _, cluster := range clusterByTimeWindow(ordered, info) {
		for _, group := range groupByCosine(cluster, loadVec) {
			poolIDs = append(poolIDs, bestByAestheticInGroup(group, info))
		}
	}
	if len(poolIDs) == 0 {
		return nil, "", nil
	}

	// 候选池按 aesthetic_score 降序;同分按 id 排序保证结果确定、可复现。
	sort.SliceStable(poolIDs, func(i, j int) bool {
		si, sj := aestheticOf(info, poolIDs[i]), aestheticOf(info, poolIDs[j])
		if si != sj {
			return si > sj
		}
		return poolIDs[i] < poolIDs[j]
	})

	cover := poolIDs[0]
	featured := poolIDs
	if maxFeatured >= 0 && maxFeatured < len(featured) {
		featured = featured[:maxFeatured]
	}
	return featured, cover, nil
}

// clusterByTimeWindow 把已按 taken_at 升序排好(无 taken_at 的排最后)的
// ordered 序列,按相邻两点间隔 <= burstWindowSeconds 链式分簇:算法与
// moments_trip.go 的 splitByGap 同构,只是这里的窗口单位是秒、判据是"够近就
// 并入同簇"而不是"够远就切段"。没有 taken_at 的资产各自单独成簇——没有时间
// 锚点,无法判断它与任何人相邻。
func clusterByTimeWindow(ordered []string, info map[string]pickAssetInfo) [][]string {
	var clusters [][]string
	var cur []string
	var prev time.Time
	window := time.Duration(burstWindowSeconds) * time.Second

	flush := func() {
		if len(cur) > 0 {
			clusters = append(clusters, cur)
			cur = nil
		}
	}

	for _, id := range ordered {
		t := info[id].takenAt
		if t.IsZero() {
			flush()
			clusters = append(clusters, []string{id})
			continue
		}
		if len(cur) > 0 && t.Sub(prev) <= window {
			cur = append(cur, id)
		} else {
			flush()
			cur = []string{id}
		}
		prev = t
	}
	flush()
	return clusters
}

// groupByCosine 在一个时间簇内,用并查集把"两两余弦相似度 > 阈值"的资产合并
// 成连拍组(允许链式传递:A~B、B~C 都命中即使 A~C 未命中,A/B/C 仍归一组—
// —这与"连拍"的直觉一致:一串连续快门里首尾两张构图可能已经有明显差异)。
// 没有向量的资产各自单独成组(跳过去重,直接进池)。
func groupByCosine(cluster []string, loadVec clipVecLoader) [][]string {
	if len(cluster) <= 1 {
		groups := make([][]string, len(cluster))
		for i, id := range cluster {
			groups[i] = []string{id}
		}
		return groups
	}

	type item struct {
		id  string
		vec []float32
	}
	var vecItems []item
	var groups [][]string
	for _, id := range cluster {
		v, ok := loadVec(id)
		if !ok {
			groups = append(groups, []string{id})
			continue
		}
		vecItems = append(vecItems, item{id, v})
	}

	n := len(vecItems)
	parent := make([]int, n)
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(x int) int {
		for parent[x] != x {
			parent[x] = parent[parent[x]]
			x = parent[x]
		}
		return x
	}
	union := func(a, b int) {
		ra, rb := find(a), find(b)
		if ra != rb {
			parent[ra] = rb
		}
	}

	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			sim := 1 - cosDist(vecItems[i].vec, vecItems[j].vec)
			if sim > burstCosineThreshold {
				union(i, j)
			}
		}
	}

	byRoot := map[int][]string{}
	var order []int
	for i := 0; i < n; i++ {
		r := find(i)
		if _, ok := byRoot[r]; !ok {
			order = append(order, r)
		}
		byRoot[r] = append(byRoot[r], vecItems[i].id)
	}
	for _, r := range order {
		groups = append(groups, byRoot[r])
	}
	return groups
}

// bestByAestheticInGroup 返回组内 aesthetic_score 最高者;没有美学分的资产
// 视为最低(-1),分数并列按 id 排序保证确定性。
func bestByAestheticInGroup(group []string, info map[string]pickAssetInfo) string {
	best := group[0]
	bestScore := aestheticOf(info, best)
	for _, id := range group[1:] {
		s := aestheticOf(info, id)
		if s > bestScore || (s == bestScore && id < best) {
			best, bestScore = id, s
		}
	}
	return best
}

// aestheticOf 返回资产的 aesthetic_score;没打过分(NULL)的视为 -1,天然排
// 在任何已打分资产之后。
func aestheticOf(info map[string]pickAssetInfo, id string) float64 {
	v := info[id]
	if !v.hasAesthetic {
		return -1
	}
	return v.aesthetic
}

// loadPickAssetInfo 批量查询 assets.taken_at/aesthetic_score(chunk 到 500 一
// 批,避免 SQLite 变量上限,与 places.go bestByAesthetic 同款写法)。
func loadPickAssetInfo(ctx context.Context, db *sql.DB, assets []MomentAsset) (map[string]pickAssetInfo, error) {
	out := make(map[string]pickAssetInfo, len(assets))
	ids := make([]string, len(assets))
	for i, a := range assets {
		ids[i] = a.AssetID
	}

	const chunkSize = 500
	for start := 0; start < len(ids); start += chunkSize {
		end := start + chunkSize
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[start:end]

		ph := strings.Repeat("?,", len(chunk)-1) + "?"
		args := make([]interface{}, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}

		rows, err := db.QueryContext(ctx, `
			SELECT id, taken_at, aesthetic_score FROM assets WHERE id IN (`+ph+`)`, args...)
		if err != nil {
			return nil, fmt.Errorf("moments: pick load asset info: %w", err)
		}
		for rows.Next() {
			var id string
			var ts sql.NullString
			var score sql.NullFloat64
			if err := rows.Scan(&id, &ts, &score); err != nil {
				rows.Close()
				return nil, fmt.Errorf("moments: scan pick asset info: %w", err)
			}
			var info pickAssetInfo
			if t := parseSQLiteTime(ts); t != nil {
				info.takenAt = *t
			}
			if score.Valid {
				info.aesthetic = score.Float64
				info.hasAesthetic = true
			}
			out[id] = info
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("moments: iterate pick asset info: %w", err)
		}
		rows.Close()
	}
	return out, nil
}
