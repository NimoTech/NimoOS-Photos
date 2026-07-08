package service

import "testing"

// semanticCutIndex 在按分数降序排列的「语义命中子序列」上计算最佳匹配层应保留的
// 条数（返回值 == len(scores) 表示不分层）。用例覆盖规格 §2 列出的五类场景：
// 教科书断崖 / 均匀分布不分层 / <3 条不分层 / 全高分不分层 / 断崖不显著仅相对阈值生效。
func TestSemanticCutIndex(t *testing.T) {
	cases := []struct {
		name   string
		scores []float64
		alpha  float64
		want   int
	}{
		{
			// "fish" 实测：4 条真命中 0.66~0.86，随后断崖到 0.13 的无关图。
			// 相对阈值 0.7×0.86=0.602 与断崖信号（最大分差 0.53 在 index3，
			// 远超 max(0.10, 2×mean)=0.292）在同一处一致触发。
			name:   "教科书断崖",
			scores: []float64{0.86, 0.80, 0.72, 0.66, 0.13, 0.13},
			alpha:  0.7,
			want:   4,
		},
		{
			name:   "均匀分布不分层",
			scores: []float64{0.50, 0.48, 0.46, 0.44, 0.42},
			alpha:  0.7,
			want:   5,
		},
		{
			name:   "少于3条不分层_2条",
			scores: []float64{0.90, 0.10},
			alpha:  0.7,
			want:   2,
		},
		{
			name:   "少于3条不分层_1条",
			scores: []float64{0.90},
			alpha:  0.7,
			want:   1,
		},
		{
			name:   "全高分不分层",
			scores: []float64{0.90, 0.88, 0.85, 0.83, 0.80},
			alpha:  0.7,
			want:   5,
		},
		{
			// 相邻分差恒定 0.05，远达不到 max(0.10, 2×mean=0.10) 的显著断崖门槛，
			// 但累计衰减在 index7 处越过相对阈值 0.7×1.00=0.70，仅相对阈值生效。
			name:   "断崖不显著仅相对阈值生效",
			scores: []float64{1.00, 0.95, 0.90, 0.85, 0.80, 0.75, 0.70, 0.65, 0.60},
			alpha:  0.7,
			want:   7,
		},
		{
			name:   "空切片不分层",
			scores: nil,
			alpha:  0.7,
			want:   0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := semanticCutIndex(c.scores, c.alpha)
			if got != c.want {
				t.Errorf("semanticCutIndex(%v, %v) = %d, want %d", c.scores, c.alpha, got, c.want)
			}
		})
	}
}

// searchCutAlpha 未配置时退回默认值 0.7（config 未初始化时，如本测试直接调用）。
func TestSearchCutAlphaDefault(t *testing.T) {
	if got := searchCutAlpha(); got != defaultSearchCutAlpha {
		t.Errorf("searchCutAlpha() = %v, want default %v", got, defaultSearchCutAlpha)
	}
}
