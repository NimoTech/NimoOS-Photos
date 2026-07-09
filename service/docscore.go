package service

import (
	"math"

	"github.com/NimoTech/NimoOS-Photos/pkg/config"
)

// OCR「文档」分类混合判据的纯计算部分。密度候选闸(coverage/line_count)之上,
// 用 CLIP 零样本语义边际 + 行几何规整度做加权否决,消灭「文字密集的照片被误判
// 成 OCR」。规格:docs/superpowers/specs/2026-07-09-ocr-doc-classify-design.md。
//
// 提示词写死为代码常量(算法实现细节,不进用户配置);其文本嵌入按
// common.MLModelGen 缓存在 clip_text_cache 表(见 docverdict.go)。
var docPrompts = []string{
	"a scan of a document",
	"a photo of a receipt",
	"a screenshot of a phone or computer screen",
	"a page of a book with text",
	"a whiteboard with handwriting",
	"an identity card or a paper form",
}

var photoPrompts = []string{
	"a photo of a restaurant menu",
	"a photo of a storefront with signs",
	"a poster on a wall",
	"a street scene with signs and billboards",
	"a natural photograph of people or scenery",
}

// 权重/阈值默认值(photos.conf 可覆盖)。初始值为经验值,现网 91 图校准后
// 如有调整须同步更新这里与配置注释。
const (
	defaultDocWSem       = 0.65
	defaultDocWGeo       = 0.35
	defaultDocScoreFloor = 0.5
	// SigLIP2 图文余弦本身量级很小(强相关 ~0.09-0.13,见 scan.go 标定注释),
	// 文档组-照片组的边际更小,[-0.01, 0.05] 是线性归一的初始标定区间。
	defaultDocSemFloor = -0.01
	defaultDocSemCeil  = 0.05
)

func docWSem() float64 {
	if config.Cfg != nil && config.Cfg.DocWSem != 0 {
		return config.Cfg.DocWSem
	}
	return defaultDocWSem
}

func docWGeo() float64 {
	if config.Cfg != nil && config.Cfg.DocWGeo != 0 {
		return config.Cfg.DocWGeo
	}
	return defaultDocWGeo
}

func docScoreFloor() float64 {
	if config.Cfg != nil && config.Cfg.DocScoreFloor != 0 {
		return config.Cfg.DocScoreFloor
	}
	return defaultDocScoreFloor
}

func docSemFloor() float64 {
	if config.Cfg != nil && config.Cfg.DocSemFloor != 0 {
		return config.Cfg.DocSemFloor
	}
	return defaultDocSemFloor
}

func docSemCeil() float64 {
	if config.Cfg != nil && config.Cfg.DocSemCeil != 0 {
		return config.Cfg.DocSemCeil
	}
	return defaultDocSemCeil
}

// docSemMargin 返回图向量对「文档组」与「照片组」提示词的最大余弦相似度之差。
// 正 = 语义上更像平面文字载体;负 = 更像自然照片。空组返回 0(中性)。
func docSemMargin(imgVec []float32, docVecs, photoVecs [][]float32) float64 {
	maxSim := func(vecs [][]float32) (float64, bool) {
		best, ok := -2.0, false
		for _, v := range vecs {
			if len(v) != len(imgVec) {
				continue
			}
			// cosDist 返回余弦距离(调用方惯例 1-cosDist=相似度,见 faces.go
			// ClusterConfidence);实现前请以 faces.go 实读为准。
			sim := 1.0 - cosDist(imgVec, v)
			if sim > best {
				best, ok = sim, true
			}
		}
		return best, ok
	}
	d, okD := maxSim(docVecs)
	p, okP := maxSim(photoVecs)
	if !okD || !okP {
		return 0
	}
	return d - p
}

// docGeoScore 从保留行的归一化四角坐标(TL,TR,BR,BL 顺序)计算版面规整度 [0,1]:
// 水平度(上边缘角度)、等高性(行高 CV)、对齐性(左缘/中心 std 取小)三特征均值。
// 行数 < 3 时统计不可靠,返回 0.5(中性,不投票)。
// 注意:在归一化坐标系下计算,非方图会使角度值有畸变,但水平行(dy≈0)在任何
// 宽高比下角度都≈0,判别方向不受影响。
func docGeoScore(boxes [][]float64) float64 {
	type line struct{ angle, height, left, center float64 }
	lines := make([]line, 0, len(boxes))
	for _, b := range boxes {
		if len(b) != 8 {
			continue
		}
		topDx, topDy := b[2]-b[0], b[3]-b[1]
		if topDx == 0 && topDy == 0 {
			continue
		}
		angle := math.Abs(math.Atan2(topDy, topDx)) * 180 / math.Pi
		if angle > 90 { // 归到 [0,90]
			angle = 180 - angle
		}
		hL := math.Hypot(b[6]-b[0], b[7]-b[1]) // TL→BL
		hR := math.Hypot(b[4]-b[2], b[5]-b[3]) // TR→BR
		h := (hL + hR) / 2
		if h <= 0 {
			continue
		}
		lines = append(lines, line{
			angle:  angle,
			height: h,
			left:   math.Min(b[0], b[6]),
			center: (b[0] + b[2] + b[4] + b[6]) / 4,
		})
	}
	if len(lines) < 3 {
		return 0.5
	}

	mean := func(get func(line) float64) float64 {
		s := 0.0
		for _, l := range lines {
			s += get(l)
		}
		return s / float64(len(lines))
	}
	std := func(get func(line) float64, m float64) float64 {
		s := 0.0
		for _, l := range lines {
			d := get(l) - m
			s += d * d
		}
		return math.Sqrt(s / float64(len(lines)))
	}
	clamp01 := func(x float64) float64 { return math.Max(0, math.Min(1, x)) }

	// 水平度:平均角度 0° → 1 分;>=15° → 0 分。
	horiz := clamp01(1 - mean(func(l line) float64 { return l.angle })/15)
	// 等高性:行高变异系数 0 → 1 分;>=0.75 → 0 分。
	mh := mean(func(l line) float64 { return l.height })
	uniform := 0.0
	if mh > 0 {
		uniform = clamp01(1 - (std(func(l line) float64 { return l.height }, mh)/mh)/0.75)
	}
	// 对齐性:左缘与中心的 std 取小(兼容左对齐文档与居中收据),0 → 1 分;>=0.15 → 0 分。
	ml := mean(func(l line) float64 { return l.left })
	mc := mean(func(l line) float64 { return l.center })
	align := clamp01(1 - math.Min(
		std(func(l line) float64 { return l.left }, ml),
		std(func(l line) float64 { return l.center }, mc))/0.15)

	return (horiz + uniform + align) / 3
}

// docVerdict 把语义边际线性归一后与几何规整度加权,过 docScoreFloor 判为文档。
// 只在密度候选闸(coverage/line_count)已通过的资产上调用——本函数不重复判密度。
func docVerdict(semMargin, geo float64) bool {
	floor, ceil := docSemFloor(), docSemCeil()
	sem := 0.5
	if ceil > floor {
		sem = math.Max(0, math.Min(1, (semMargin-floor)/(ceil-floor)))
	}
	// 1e-9 容差:抵消浮点除法/加权在临界值(如中性档正好等于 floor)上的舍入误差,
	// 不改变判定语义——真正明确低于 floor 的分数差距远超此容差。
	return docWSem()*sem+docWGeo()*geo >= docScoreFloor()-1e-9
}
