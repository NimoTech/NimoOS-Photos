package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// 造一个规整文档版面:n 行水平、等高、左对齐、等行距。
func regularBoxes(n int) [][]float64 {
	out := make([][]float64, 0, n)
	for i := 0; i < n; i++ {
		y := 0.1 + float64(i)*0.06
		out = append(out, []float64{0.1, y, 0.8, y, 0.8, y + 0.04, 0.1, y + 0.04})
	}
	return out
}

// 造散乱街景文字:角度、行高、水平位置全不一致。
func scatteredBoxes() [][]float64 {
	return [][]float64{
		{0.05, 0.10, 0.40, 0.18, 0.39, 0.28, 0.04, 0.20}, // 斜排大字
		{0.55, 0.50, 0.70, 0.50, 0.70, 0.53, 0.55, 0.53}, // 水平小字
		{0.20, 0.70, 0.60, 0.62, 0.61, 0.74, 0.21, 0.82}, // 反向斜排
		{0.80, 0.30, 0.95, 0.30, 0.95, 0.42, 0.80, 0.42}, // 高瘦块
	}
}

func TestDocGeoScore(t *testing.T) {
	reg := docGeoScore(regularBoxes(10))
	scat := docGeoScore(scatteredBoxes())
	require.Greater(t, reg, 0.8, "规整版面应得高分, got %v", reg)
	require.Less(t, scat, 0.5, "散乱文字应得低分, got %v", scat)
	require.Greater(t, reg, scat+0.3, "两者区分度要足够")

	require.InDelta(t, 0.5, docGeoScore(regularBoxes(2)), 1e-9, "<3 行返回中性 0.5")
	require.InDelta(t, 0.5, docGeoScore(nil), 1e-9)
	// 非法框(长度非 8)被跳过;全非法等价于 0 行 → 中性
	require.InDelta(t, 0.5, docGeoScore([][]float64{{0.1, 0.2}}), 1e-9)
}

func TestDocSemMargin(t *testing.T) {
	e1 := make([]float32, 4)
	e1[0] = 1
	e2 := make([]float32, 4)
	e2[1] = 1
	mix := []float32{0.9, 0.1, 0, 0} // 靠近 e1

	// 图像贴近文档向量 → 正边际;贴近照片向量 → 负边际
	require.Greater(t, docSemMargin(mix, [][]float32{e1}, [][]float32{e2}), 0.0)
	require.Less(t, docSemMargin(mix, [][]float32{e2}, [][]float32{e1}), 0.0)
	// 多提示词取各组 max
	require.Greater(t,
		docSemMargin(mix, [][]float32{e2, e1}, [][]float32{e2}), 0.0)
	// 空组安全:返回 0(中性)
	require.InDelta(t, 0.0, docSemMargin(mix, nil, nil), 1e-9)
}

func TestDocVerdict(t *testing.T) {
	// 默认配置(config.Cfg 为 nil 时用默认值):wSem=0.65 wGeo=0.35 floor=0.5
	// semFloor=-0.01 semCeil=0.05 → margin 0.05 归一为 1.0
	require.True(t, docVerdict(0.05, 1.0), "语义强文档 + 几何规整 → 文档")
	require.False(t, docVerdict(-0.05, 0.2), "语义强照片 + 几何散乱 → 否决")
	// 语义中性(0.02 归一 0.5)+ 几何中性 0.5 → 加权 0.5,>=floor 通过
	require.True(t, docVerdict(0.02, 0.5))
	// 语义明确照片(归一 0)拉不回:0.65*0 + 0.35*0.5 = 0.175 < 0.5
	require.False(t, docVerdict(-0.01, 0.5))
}

// TestHasOcrExprTriState 验证共享判据片段的三态语义(经 ListAssets 真实查询):
// is_doc=1 → hasOcr;is_doc=0(密度过但被否决)→ 非 hasOcr;
// is_doc NULL → 回退旧密度判据。
func TestHasOcrExprTriState(t *testing.T) {
	db := makeTestDB(t)
	s := NewSearchService(db, nil)

	mk := func(id string, coverage float64, lines int, isDoc any) {
		_, err := db.Exec(`INSERT INTO assets(id,file_path,mime_type,status) VALUES(?,?, 'image/jpeg','indexed')`, id, "/p/"+id+".jpg")
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO asset_ocr(asset_id,text,coverage,line_count,is_doc) VALUES(?,'x',?,?,?)`, id, coverage, lines, isDoc)
		require.NoError(t, err)
	}
	mk("verdict1", 0.1, 20, 1)      // 判文档
	mk("vetoed", 0.1, 20, 0)        // 密度过但被语义否决
	mk("legacyDoc", 0.1, 20, nil)   // 未算 → 旧判据:过
	mk("legacyPhoto", 0.01, 2, nil) // 未算 → 旧判据:不过

	assets, err := s.ListAssets("", 100, 0)
	require.NoError(t, err)
	got := map[string]bool{}
	for _, a := range assets {
		got[a.ID] = a.HasOCR
	}
	require.True(t, got["verdict1"])
	require.False(t, got["vetoed"], "被否决的不进 OCR 类——本功能的核心目标")
	require.True(t, got["legacyDoc"], "未算回退旧密度判据")
	require.False(t, got["legacyPhoto"])
}
