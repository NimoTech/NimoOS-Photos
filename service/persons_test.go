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

func TestListPersons_PlacesCount(t *testing.T) {
	db := makeTestFaceDB(t)
	dim := 512
	a := make([]float32, dim); a[0] = 1.0
	insertAssetFace(t, db, "pl-a1", normalize(a))
	insertAssetFace(t, db, "pl-a2", normalize(a))
	// 给两张 asset 不同 GPS（粗粒度 cell 不同），asset_exif 行
	_, err := db.Exec(`INSERT INTO asset_exif(asset_id, latitude, longitude) VALUES('pl-a1', 35.6, 139.6)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO asset_exif(asset_id, latitude, longitude) VALUES('pl-a2', 37.7, -122.4)`)
	require.NoError(t, err)

	require.NoError(t, service.NewFaceService(db).RunClustering(context.Background()))
	list, err := service.NewPersonService(db).ListPersons()
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, 2, list[0].PlacesCount)

	// 软删 a2 后 places 应减为 1
	_, err = db.Exec(`UPDATE assets SET deleted_at='2026-05-01 00:00:00' WHERE id='pl-a2'`)
	require.NoError(t, err)
	list2, err := service.NewPersonService(db).ListPersons()
	require.NoError(t, err)
	require.Len(t, list2, 1)
	require.Equal(t, 1, list2[0].PlacesCount)
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

func TestGetPerson_Stats(t *testing.T) {
	db := makeTestFaceDB(t)
	dim := 512
	a := make([]float32, dim); a[0] = 1.0
	insertAssetFace(t, db, "g-a1", normalize(a))
	require.NoError(t, service.NewFaceService(db).RunClustering(context.Background()))

	ps := service.NewPersonService(db)
	list, _ := ps.ListPersons()
	p, err := ps.GetPerson(list[0].ID)
	require.NoError(t, err)
	require.Equal(t, 1, p.Count)

	_, err = ps.GetPerson("no-such")
	require.ErrorIs(t, err, service.ErrNotFound)
}
