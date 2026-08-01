package sqlite

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestClipDimMatches(t *testing.T) {
	require.True(t, clipDimMatches(`CREATE VIRTUAL TABLE clip_embeddings USING vec0(embedding float[1152])`, 1152))
	// Substring trap: float[1152] contains a prefix like "float[115"; exact matching must reject this
	require.False(t, clipDimMatches(`... float[1152] ...`, 115))
	require.False(t, clipDimMatches(`... float[512] ...`, 1152))
	require.False(t, clipDimMatches(`no dim here`, 1152))
}
