package sqlite

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestClipDimMatches(t *testing.T) {
	require.True(t, clipDimMatches(`CREATE VIRTUAL TABLE clip_embeddings USING vec0(embedding float[1152])`, 1152))
	// 子串陷阱:float[1152] 包含 "float[115" 之类前缀;精确匹配必须拒绝
	require.False(t, clipDimMatches(`... float[1152] ...`, 115))
	require.False(t, clipDimMatches(`... float[512] ...`, 1152))
	require.False(t, clipDimMatches(`no dim here`, 1152))
}
