package aesthetic

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadEmbedded(t *testing.T) {
	h, err := Load()
	require.NoError(t, err)
	require.Equal(t, 1152, h.InDim(), "embedded head must match SigLIP2's 1152 dimensions")
	require.NotEmpty(t, h.Version())
	// Scoring a random vector should yield a finite value (not NaN/Inf).
	vec := make([]float32, 1152)
	for i := range vec {
		vec[i] = float32(i%7) * 0.1
	}
	s := h.Score(vec)
	require.False(t, s != s, "score should not be NaN")
}
