package aesthetic

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadEmbedded(t *testing.T) {
	h, err := Load()
	require.NoError(t, err)
	require.Equal(t, 1152, h.InDim(), "内嵌头必须匹配 SigLIP2 1152 维")
	require.NotEmpty(t, h.Version())
	// 打一个随机向量应得到有限值(非 NaN/Inf)。
	vec := make([]float32, 1152)
	for i := range vec {
		vec[i] = float32(i%7) * 0.1
	}
	s := h.Score(vec)
	require.False(t, s != s, "score 不应为 NaN")
}
