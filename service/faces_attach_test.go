package service_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/stretchr/testify/require"
)

func TestTimelineAndGetAssetCarryNamedFaces(t *testing.T) {
	db := makeTestFaceDB(t)
	vec := make([]float32, 512)
	vec[0] = 1.0
	insertAssetFace(t, db, "a1", normalize(vec))
	require.NoError(t, service.NewFaceService(db).RunClustering(context.Background()))
	_, err := db.Exec(`UPDATE persons SET name='Alice'`)
	require.NoError(t, err)

	svc := service.NewSearchService(db, nil)

	groups, err := svc.Timeline("u1")
	require.NoError(t, err)
	require.NotEmpty(t, groups)
	require.Equal(t, []string{"Alice"}, groups[0].Assets[0].Faces)

	a, err := svc.GetAsset("u1", "a1")
	require.NoError(t, err)
	require.Equal(t, []string{"Alice"}, a.Faces)

	pa, err := svc.PersonAssets(mustPersonID(t, db), 100, 0)
	require.NoError(t, err)
	require.Equal(t, []string{"Alice"}, pa[0].Faces)
}

func mustPersonID(t *testing.T, db *sql.DB) string {
	t.Helper()
	var pid string
	require.NoError(t, db.QueryRow(`SELECT id FROM persons`).Scan(&pid))
	return pid
}
