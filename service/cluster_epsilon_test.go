package service_test

import (
	"context"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/pkg/config"
	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/stretchr/testify/require"
)

// Two faces at cosine distance ~0.52: legacy eps 0.6 chains them into one
// person; the new default 0.48 must keep them apart. Proves the epsilon is
// config-driven end to end.
func TestClusterEpsilonConfigDriven(t *testing.T) {
	build := func(eps float64) int {
		db := makeTestFaceDB(t)
		a := make([]float32, 512)
		a[0] = 1.0
		b := make([]float32, 512)
		b[0] = 0.48
		b[1] = 0.877 // cosθ=0.48 → dist≈0.52
		insertAssetFace(t, db, "fa", normalize(a))
		insertAssetFace(t, db, "fb", normalize(b))

		old := config.Cfg
		config.Cfg = &config.Config{ClusterEpsilon: eps, FacesEnabled: true}
		defer func() { config.Cfg = old }()

		require.NoError(t, service.NewFaceService(db).RunClustering(context.Background()))
		var n int
		require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM persons`).Scan(&n))
		return n
	}
	require.Equal(t, 1, build(0.6), "legacy epsilon must chain the pair")
	require.Equal(t, 2, build(0.48), "new default epsilon must keep them apart")
}
