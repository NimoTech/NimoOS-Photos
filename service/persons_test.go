package service_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/google/uuid"
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

func mustFirstPersonID(t *testing.T, db *sql.DB) string {
	t.Helper()
	var id string
	require.NoError(t, db.QueryRow(`SELECT id FROM persons`).Scan(&id))
	return id
}

func TestGetPerson_HiddenReturnsNotFound(t *testing.T) {
	db := makeTestFaceDB(t)
	dim := 512
	a := make([]float32, dim); a[0] = 1.0
	insertAssetFace(t, db, "gh-a1", normalize(a))
	require.NoError(t, service.NewFaceService(db).RunClustering(context.Background()))

	id := mustFirstPersonID(t, db)
	_, err := db.Exec(`UPDATE persons SET hidden=1 WHERE id=?`, id)
	require.NoError(t, err)

	_, err = service.NewPersonService(db).GetPerson(id)
	require.ErrorIs(t, err, service.ErrNotFound)
}

func TestUpdatePerson_AndHideRestore(t *testing.T) {
	db := makeTestFaceDB(t)
	dim := 512
	a := make([]float32, dim); a[0] = 1.0
	insertAssetFace(t, db, "u-a1", normalize(a))
	require.NoError(t, service.NewFaceService(db).RunClustering(context.Background()))
	ps := service.NewPersonService(db)
	id := mustFirstPersonID(t, db)

	name := "Bob"
	fav := true
	rel := "family"
	require.NoError(t, ps.UpdatePerson(id, service.PersonPatch{Name: &name, Favorite: &fav, Relation: &rel}))
	p, _ := ps.GetPerson(id)
	require.Equal(t, "Bob", p.Name)
	require.True(t, p.Favorite)
	require.Equal(t, "family", p.Relation)

	require.NoError(t, ps.HidePerson(id))
	l, _ := ps.ListPersons()
	require.Len(t, l, 0)
	require.NoError(t, ps.RestorePerson(id))
	l2, _ := ps.ListPersons()
	require.Len(t, l2, 1)

	require.ErrorIs(t, ps.UpdatePerson("nope", service.PersonPatch{Name: &name}), service.ErrNotFound)
}

func insertFaceOnAsset(t *testing.T, db *sql.DB, assetID string, vec []float32) {
	t.Helper()
	fid := uuid.NewString()
	_, err := db.Exec(`INSERT INTO face_detections(id, asset_id, bbox, embedding) VALUES(?,?,?,?)`,
		fid, assetID, `{"x1":0,"y1":0,"x2":1,"y2":1}`, sqlite.SerializeFloat32(vec))
	require.NoError(t, err)
}

func TestPersonRelations(t *testing.T) {
	db := makeTestFaceDB(t)
	dim := 512
	a := make([]float32, dim); a[0] = 1.0
	b := make([]float32, dim); b[1] = 1.0
	// 同一张 asset 内放 A、B 两张脸 → 共现
	_, err := db.Exec(`INSERT INTO assets(id, file_path, status) VALUES('shared','/x/s.jpg','indexed')`)
	require.NoError(t, err)
	insertFaceOnAsset(t, db, "shared", normalize(a))
	insertFaceOnAsset(t, db, "shared", normalize(b))
	require.NoError(t, service.NewFaceService(db).RunClustering(context.Background()))

	ps := service.NewPersonService(db)
	list, _ := ps.ListPersons()
	require.Len(t, list, 2)
	rels, err := ps.PersonRelations(list[0].ID)
	require.NoError(t, err)
	require.Len(t, rels, 1)
	require.Equal(t, 1, rels[0].Count)

	rels2, err := ps.PersonRelations(list[1].ID)
	require.NoError(t, err)
	require.Len(t, rels2, 1)
	require.Equal(t, 1, rels2[0].Count)
}

func TestPersonPlaces(t *testing.T) {
	db := makeTestFaceDB(t)
	dim := 512
	a := make([]float32, dim); a[0] = 1.0
	insertAssetFace(t, db, "pp-a1", normalize(a))
	insertAssetFace(t, db, "pp-a2", normalize(a))
	_, err := db.Exec(`INSERT INTO asset_exif(asset_id, latitude, longitude) VALUES('pp-a1', 35.6, 139.6)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO asset_exif(asset_id, latitude, longitude) VALUES('pp-a2', 37.7, -122.4)`)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE assets SET taken_at='2026-01-15 12:00:00' WHERE id='pp-a1'`)
	require.NoError(t, err)
	require.NoError(t, service.NewFaceService(db).RunClustering(context.Background()))

	ps := service.NewPersonService(db)
	id := mustFirstPersonID(t, db)
	pts, err := ps.PersonPlaces(id)
	require.NoError(t, err)
	require.Len(t, pts, 2)
}

func TestMergeSuggestions_RespectRejections(t *testing.T) {
	db := makeTestFaceDB(t)
	dim := 512
	// 两个簇质心 cosine 距离落在 (dbscanEpsilon=0.6, suggestEpsilon=0.75) 带内
	a := make([]float32, dim); a[0] = 1.0
	c := make([]float32, dim); c[0] = 0.3; c[1] = 0.954 // cos≈0.3 → dist≈0.7
	insertAssetFace(t, db, "ms-a", normalize(a))
	insertAssetFace(t, db, "ms-c", normalize(c))
	require.NoError(t, service.NewFaceService(db).RunClustering(context.Background()))

	ps := service.NewPersonService(db)
	sugs, err := ps.MergeSuggestions()
	require.NoError(t, err)
	require.NotEmpty(t, sugs, "应至少一个建议落在距离带内")

	require.NoError(t, ps.RejectMerge(sugs[0].FromID, sugs[0].IntoID))
	sugs2, err := ps.MergeSuggestions()
	require.NoError(t, err)
	for _, s := range sugs2 {
		require.False(t,
			(s.FromID == sugs[0].FromID && s.IntoID == sugs[0].IntoID) ||
				(s.FromID == sugs[0].IntoID && s.IntoID == sugs[0].FromID),
			"被拒绝的配对不应再出现")
	}
}
