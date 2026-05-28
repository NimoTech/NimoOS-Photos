package service_test

import (
	"context"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/stretchr/testify/require"
)

func TestListPersons_ExcludesHiddenAndCounts(t *testing.T) {
	db := makeTestFaceDB(t)
	dim := 512
	a := make([]float32, dim)
	a[0] = 1.0
	insertAssetFace(t, db, "p-a1", normalize(a))
	insertAssetFace(t, db, "p-a2", normalize(a)) // same person, two photos

	require.NoError(t, service.NewFaceService(db).RunClustering(context.Background()))

	ps := service.NewPersonService(db)
	list, err := ps.ListPersons()
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, 2, list[0].Count)

	// hidden person should disappear from list
	_, err = db.Exec(`UPDATE persons SET hidden=1 WHERE id=?`, list[0].ID)
	require.NoError(t, err)
	list2, err := ps.ListPersons()
	require.NoError(t, err)
	require.Len(t, list2, 0)
}

func TestFacesIndexedUpTo(t *testing.T) {
	db := makeTestFaceDB(t)
	// 空库
	ps := service.NewPersonService(db)
	ts, err := ps.FacesIndexedUpTo()
	require.NoError(t, err)
	require.Nil(t, ts)

	// 有脸：插入 asset + indexed_at + face
	_, err = db.Exec(`INSERT INTO assets(id, file_path, status, indexed_at) VALUES('fi-a', '/x/fi-a.jpg', 'indexed', '2026-05-01 12:00:00')`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO face_detections(id, asset_id, bbox, embedding) VALUES('fi-f', 'fi-a', '{}', X'00000000')`)
	require.NoError(t, err)

	ts2, err := ps.FacesIndexedUpTo()
	require.NoError(t, err)
	require.NotNil(t, ts2)
	require.Contains(t, *ts2, "2026")
}
