package service_test

import (
	"context"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/stretchr/testify/require"
)

func TestPersonVisible(t *testing.T) {
	db := makeTestFaceDB(t)
	vec := make([]float32, 512)
	vec[0] = 1.0
	insertAssetFace(t, db, "a1", normalize(vec))
	require.NoError(t, service.NewFaceService(db).RunClustering(context.Background()))
	var pid string
	require.NoError(t, db.QueryRow(`SELECT id FROM persons`).Scan(&pid))

	ps := service.NewPersonService(db)
	require.NoError(t, ps.PersonVisible(pid))
	require.ErrorIs(t, ps.PersonVisible("no-such-id"), service.ErrNotFound)

	_, err := db.Exec(`UPDATE persons SET hidden=1 WHERE id=?`, pid)
	require.NoError(t, err)
	require.ErrorIs(t, ps.PersonVisible(pid), service.ErrNotFound)
}
