package service_test

import (
	"math/rand"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/stretchr/testify/require"
)

// The parallel neighbor-list DBSCAN must produce byte-identical labels to a
// reference serial run across random inputs and epsilons.
func TestDBSCANParallelMatchesSerial(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	for trial := 0; trial < 5; trial++ {
		n := 200 + trial*100
		vecs := make([][]float32, n)
		for i := range vecs {
			v := make([]float32, 32)
			for j := range v {
				v[j] = rng.Float32()*2 - 1
			}
			vecs[i] = v
		}
		for _, eps := range []float64{0.2, 0.48, 0.7} {
			want := service.DBSCAN(vecs, eps, 1) // reference implementation
			got := service.DBSCANWithProgress(vecs, eps, 1, func(done, n int) {})
			require.Equal(t, want, got, "trial=%d eps=%v", trial, eps)
		}
	}
}
