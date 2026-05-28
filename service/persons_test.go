package service_test

import (
	"context"
	"database/sql"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
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
	a := make([]float32, dim)
	a[0] = 1.0
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
	a := make([]float32, dim)
	a[0] = 1.0
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
	a := make([]float32, dim)
	a[0] = 1.0
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
	a := make([]float32, dim)
	a[0] = 1.0
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
	a := make([]float32, dim)
	a[0] = 1.0
	b := make([]float32, dim)
	b[1] = 1.0
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
	a := make([]float32, dim)
	a[0] = 1.0
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

	// 至少一个点带 TakenAt（pp-a1 有 taken_at）
	hasTime := false
	for _, p := range pts {
		if p.TakenAt != nil {
			hasTime = true
		}
	}
	require.True(t, hasTime)
}

func TestMergeSuggestions_StableOrder(t *testing.T) {
	db := makeTestFaceDB(t)
	dim := 512
	a := make([]float32, dim)
	a[0] = 1.0
	c := make([]float32, dim)
	c[0] = 0.3
	c[1] = 0.954
	insertAssetFace(t, db, "so-a", normalize(a))
	insertAssetFace(t, db, "so-c", normalize(c))
	require.NoError(t, service.NewFaceService(db).RunClustering(context.Background()))

	// 两个 person 都命名
	rows, err := db.Query(`SELECT id FROM persons ORDER BY rowid`)
	require.NoError(t, err)
	var ids []string
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		ids = append(ids, id)
	}
	rows.Close()
	require.Len(t, ids, 2)
	_, err = db.Exec(`UPDATE persons SET name='Alice' WHERE id=?`, ids[0])
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE persons SET name='Bob' WHERE id=?`, ids[1])
	require.NoError(t, err)

	ps := service.NewPersonService(db)
	r1, err := ps.MergeSuggestions()
	require.NoError(t, err)
	r2, err := ps.MergeSuggestions()
	require.NoError(t, err)
	require.Equal(t, len(r1), len(r2))
	require.Greater(t, len(r1), 0)
	require.Equal(t, r1[0].FromID, r2[0].FromID)
	require.Equal(t, r1[0].IntoID, r2[0].IntoID)
}

func writeTestJPEG(t *testing.T, path string, w, h int) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	f, err := os.Create(path)
	require.NoError(t, err)
	defer f.Close()
	require.NoError(t, jpeg.Encode(f, img, nil))
}

func TestFaceThumbnail_CropsAndCaches(t *testing.T) {
	db := makeTestFaceDB(t)
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.jpg")
	writeTestJPEG(t, srcPath, 400, 300)
	_, err := db.Exec(`INSERT INTO assets(id, file_path, status) VALUES('fa', ?, 'indexed')`, srcPath)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO face_detections(id, asset_id, bbox, embedding) VALUES('face1','fa',?,?)`,
		`{"x1":0.25,"y1":0.25,"x2":0.6,"y2":0.7}`, sqlite.SerializeFloat32(make([]float32, 512)))
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO persons(id, name, cover_asset_id, cover_face_id) VALUES('pp','','fa','face1')`)
	require.NoError(t, err)

	ps := service.NewPersonService(db)
	cacheDir := filepath.Join(dir, "face-thumbs")
	out, err := ps.FaceThumbnail("pp", cacheDir)
	require.NoError(t, err)
	st, err := os.Stat(out)
	require.NoError(t, err)
	require.Greater(t, st.Size(), int64(0))

	// 第二次命中缓存：路径相同且不重建（mtime 不变即缓存命中）
	stat1, _ := os.Stat(out)
	out2, err := ps.FaceThumbnail("pp", cacheDir)
	require.NoError(t, err)
	require.Equal(t, out, out2)
	stat2, _ := os.Stat(out2)
	require.Equal(t, stat1.ModTime(), stat2.ModTime())

	// 不存在 person 返回 ErrNotFound
	_, err = ps.FaceThumbnail("no-such", cacheDir)
	require.ErrorIs(t, err, service.ErrNotFound)
}

func TestFaceThumbnail_HiddenReturnsNotFound(t *testing.T) {
	db := makeTestFaceDB(t)
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.jpg")
	writeTestJPEG(t, srcPath, 400, 300)
	_, err := db.Exec(`INSERT INTO assets(id, file_path, status) VALUES('fh', ?, 'indexed')`, srcPath)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO face_detections(id, asset_id, bbox, embedding) VALUES('face-h','fh',?,?)`,
		`{"x1":0.25,"y1":0.25,"x2":0.6,"y2":0.7}`, sqlite.SerializeFloat32(make([]float32, 512)))
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO persons(id, name, cover_asset_id, cover_face_id, hidden) VALUES('ph','','fh','face-h',1)`)
	require.NoError(t, err)

	ps := service.NewPersonService(db)
	_, err = ps.FaceThumbnail("ph", filepath.Join(dir, "fh-cache"))
	require.ErrorIs(t, err, service.ErrNotFound)
}

func TestMergeSuggestions_RespectRejections(t *testing.T) {
	db := makeTestFaceDB(t)
	dim := 512
	// 两个簇质心 cosine 距离落在 (dbscanEpsilon=0.6, suggestEpsilon=0.75) 带内
	a := make([]float32, dim)
	a[0] = 1.0
	c := make([]float32, dim)
	c[0] = 0.3
	c[1] = 0.954 // cos≈0.3 → dist≈0.7
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

func TestMergePersons_RecomputesCentroid(t *testing.T) {
	db := makeTestFaceDB(t)
	dim := 512
	a := make([]float32, dim)
	a[0] = 1.0
	b := make([]float32, dim)
	b[1] = 1.0
	insertAssetFace(t, db, "mp-a", normalize(a))
	insertAssetFace(t, db, "mp-b", normalize(b))
	require.NoError(t, service.NewFaceService(db).RunClustering(context.Background()))
	list, _ := service.NewPersonService(db).ListPersons()
	require.Len(t, list, 2)

	from, into := list[0].ID, list[1].ID
	require.NoError(t, service.NewSearchService(db, nil).MergePersons(from, into))

	// into 名下应有 2 张脸，且 confidence 已重算。
	// 合并前每个 person 各有 1 张脸，单脸 confidence=1.0。
	// 合并后两个正交向量的质心余弦相似度 ≈ 0.707，必然 < 1.0。
	// 若未重算则 confidence 仍为旧值 1.0，断言失败。
	var cnt int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM face_person WHERE person_id=?`, into).Scan(&cnt))
	require.Equal(t, 2, cnt)
	var conf float64
	require.NoError(t, db.QueryRow(`SELECT confidence FROM persons WHERE id=?`, into).Scan(&conf))
	require.Greater(t, conf, 0.0)
	require.Less(t, conf, 1.0, "confidence 应已重算（两个正交脸合并后 <1.0），若仍为 1.0 说明未重算")
}
