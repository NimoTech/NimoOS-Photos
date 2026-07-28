// 共用选优:trip/theme 两个引擎产出的候选成员集,靠这里统一的连拍去重 +
// 精选/封面挑选逻辑决定"哪些照片值得展示"——引擎本身只管"该不该属于这个
// 时刻",不管"该不该被推到前排"。
package service

import (
	"context"
	"database/sql"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"math"
	"os"
	"path/filepath"
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

// coverImageLoader 按 asset_id 返回封面亮度闸要检查的缩略图;ok=false 表示读
// 不到(缩略图缺失/解码失败)——pickCover 对这种候选跳过亮度闸、直接采用
// (宁可放行一张没法验证的图,也不要因为读不到缩略图就让整个 recipe 没有
// 封面)。
type coverImageLoader func(assetID string) (image.Image, bool)

// RealCoverImageLoader 是 coverImageLoader 的生产实现,读
// <thumbDir>/<assetID>/small.jpg(与 pkg/thumb.Generate 落盘路径同款惯例)并
// 解码为 image.Image。文件不存在/解码失败按 ok=false 处理。
func RealCoverImageLoader(thumbDir string) coverImageLoader {
	return func(assetID string) (image.Image, bool) {
		f, err := os.Open(filepath.Join(thumbDir, assetID, "small.jpg"))
		if err != nil {
			return nil, false
		}
		defer f.Close()
		img, err := jpeg.Decode(f)
		if err != nil {
			return nil, false
		}
		return img, true
	}
}

// 封面亮度/对比硬闸阈值——探针美学头选出的封面偶发过暗/过曝/灰雾低对比
// (真机截图实证),这是等 AVA 对齐头上线前的过渡补丁,阈值不进 recipe。
// 换 AVA 头后,pickCover 及以下几个亮度闸相关函数可整段退役。
const (
	coverMinMeanLuma = 0.12 // 灰度均值低于此值判过暗
	coverMaxMeanLuma = 0.92 // 灰度均值高于此值判过曝
	coverMinStdDev   = 0.05 // 灰度标准差低于此值判灰雾/低对比
)

// passesCoverBrightnessGate 计算 img 的灰度均值/标准差(取值域 [0,1]),判断是
// 否适合当封面:过暗、过曝、或灰雾低对比都不通过。
func passesCoverBrightnessGate(img image.Image) bool {
	mean, stddev := grayscaleMeanStdDev(img)
	if mean < coverMinMeanLuma || mean > coverMaxMeanLuma {
		return false
	}
	if stddev < coverMinStdDev {
		return false
	}
	return true
}

// grayscaleMeanStdDev 遍历 img 全部像素转灰度,返回均值与标准差(都归一化到
// [0,1])。缩略图是 small.jpg(250px 宽),像素量小,不需要额外降采样。
func grayscaleMeanStdDev(img image.Image) (mean, stddev float64) {
	bounds := img.Bounds()
	var sum, sumSq, n float64
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			g := color.GrayModel.Convert(img.At(x, y)).(color.Gray).Y
			v := float64(g) / 255.0
			sum += v
			sumSq += v * v
			n++
		}
	}
	if n == 0 {
		return 0, 0
	}
	mean = sum / n
	variance := sumSq/n - mean*mean
	if variance < 0 {
		variance = 0 // 浮点误差兜底
	}
	return mean, math.Sqrt(variance)
}

// pickCover 从 featured(已按 aesthetic_score 排好、且已按 maxFeatured 截断的
// 精选成员)中挑封面:按 MomentAsset.Score 降序、同分再按 aesthetic_score 降
// 序重排候选顺序(score 对 theme/pet 时刻是 CLIP 主题相似分,让 Your Beagle
// 的封面挑到"最像狗"的那张;trip 时刻 score 恒为 0,自然退化为纯美学序,即
// 现状行为)。逐候选过亮度/对比闸,第一个通过者当封面;loadCover 读不到
// (ok=false)的候选直接采用,不受闸约束;全部被拒(或没有可用 loader)则回退
// 到 featured[0](aesthetic 序下的最高分),不因闸失败导致"没有封面"。
func pickCover(featured []string, assets []MomentAsset, info map[string]pickAssetInfo, loadCover coverImageLoader) string {
	if len(featured) == 0 {
		return ""
	}
	if loadCover == nil {
		return featured[0]
	}

	scoreByID := make(map[string]float64, len(assets))
	for _, a := range assets {
		scoreByID[a.AssetID] = a.Score
	}

	ordered := append([]string(nil), featured...)
	sort.SliceStable(ordered, func(i, j int) bool {
		si, sj := scoreByID[ordered[i]], scoreByID[ordered[j]]
		if si != sj {
			return si > sj
		}
		ai, aj := aestheticOf(info, ordered[i]), aestheticOf(info, ordered[j])
		if ai != aj {
			return ai > aj
		}
		return ordered[i] < ordered[j]
	})

	for _, id := range ordered {
		img, ok := loadCover(id)
		if !ok {
			return id
		}
		if passesCoverBrightnessGate(img) {
			return id
		}
	}
	return featured[0]
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
// 精选。封面则从精选候选中另按 MomentAsset.Score(CLIP 主题相似分,trip 时刻
// 恒为 0)降序 + aesthetic_score 降序重排后,过 pickCover 的亮度/对比闸挑选
// (过渡补丁,见 pickCover 注释)。向量缺失的资产跳过去重步骤,直接进候选池。
//
// 注意:aesthetic_score 是探针头模型的输出,只在同一批候选内做相对排序
// (谁比谁"更值得展示"),不是绝对质量分——禁止拿它做跨批次/跨时刻的绝对
// 阈值判断(比如"低于 0.4 就不能当封面"这种规则是错的)。
func PickFeaturedAndCover(ctx context.Context, db *sql.DB, assets []MomentAsset, maxFeatured int, loadVec clipVecLoader, loadCover coverImageLoader) ([]string, string, error) {
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

	featured := poolIDs
	if maxFeatured >= 0 && maxFeatured < len(featured) {
		featured = featured[:maxFeatured]
	}
	cover := pickCover(featured, assets, info, loadCover)
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
