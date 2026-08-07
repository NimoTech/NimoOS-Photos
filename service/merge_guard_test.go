package service_test

import (
	"context"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/stretchr/testify/require"
)

func TestMergePersons_SelfMergeRejected(t *testing.T) {
	db := makeTestFaceDB(t)
	vec := make([]float32, 512)
	vec[0] = 1.0
	insertAssetFace(t, db, "a1", normalize(vec))
	require.NoError(t, service.NewFaceService(db).RunClustering(context.Background()))

	var pid string
	require.NoError(t, db.QueryRow(`SELECT id FROM persons`).Scan(&pid))
	_, err := db.Exec(`UPDATE persons SET name='Alice' WHERE id=?`, pid)
	require.NoError(t, err)

	err = service.NewSearchService(db, nil).MergePersons(pid, pid)
	require.Error(t, err)

	// Person must remain fully intact: row exists, faces still bound.
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM persons WHERE id=?`, pid).Scan(&n))
	require.Equal(t, 1, n)
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM face_person WHERE person_id=?`, pid).Scan(&n))
	require.Equal(t, 1, n)
}

func TestMergePersons_MissingPersonNotFound(t *testing.T) {
	db := makeTestFaceDB(t)
	vec := make([]float32, 512)
	vec[0] = 1.0
	insertAssetFace(t, db, "a1", normalize(vec))
	require.NoError(t, service.NewFaceService(db).RunClustering(context.Background()))
	var pid string
	require.NoError(t, db.QueryRow(`SELECT id FROM persons`).Scan(&pid))

	svc := service.NewSearchService(db, nil)
	require.ErrorIs(t, svc.MergePersons("no-such-id", pid), service.ErrNotFound)
	require.ErrorIs(t, svc.MergePersons(pid, "no-such-id"), service.ErrNotFound)
}
