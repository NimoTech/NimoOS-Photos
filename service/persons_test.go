package service_test

import (
	"context"
	"database/sql"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/common"
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

// TestPersonPlaces_PlaceName verifies that PersonPlaces enriches each GPS point
// with the correct human-readable place name derived from asset_geo.
func TestPersonPlaces_PlaceName(t *testing.T) {
	db := makeTestFaceDB(t)
	dim := 512
	a := make([]float32, dim)
	a[0] = 1.0

	// Three assets for the same person, each with a different geo scenario:
	//   pg-a1: city + country → "Tokyo, Japan"
	//   pg-a2: city only     → "Paris"
	//   pg-a3: no geo row    → ""
	insertAssetFace(t, db, "pg-a1", normalize(a))
	insertAssetFace(t, db, "pg-a2", normalize(a))
	insertAssetFace(t, db, "pg-a3", normalize(a))

	_, err := db.Exec(`INSERT INTO asset_exif(asset_id, latitude, longitude) VALUES('pg-a1', 35.6, 139.6)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO asset_exif(asset_id, latitude, longitude) VALUES('pg-a2', 48.8, 2.3)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO asset_exif(asset_id, latitude, longitude) VALUES('pg-a3', 51.5, -0.1)`)
	require.NoError(t, err)

	// Insert asset_geo rows for pg-a1 (city+country) and pg-a2 (city only).
	_, err = db.Exec(`INSERT INTO asset_geo(asset_id, city, country) VALUES('pg-a1', 'Tokyo', 'Japan')`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO asset_geo(asset_id, city, country) VALUES('pg-a2', 'Paris', '')`)
	require.NoError(t, err)
	// pg-a3 intentionally has no asset_geo row.

	require.NoError(t, service.NewFaceService(db).RunClustering(context.Background()))

	ps := service.NewPersonService(db)
	id := mustFirstPersonID(t, db)
	pts, err := ps.PersonPlaces(id)
	require.NoError(t, err)
	require.Len(t, pts, 3)

	// Build a map from lat→PlaceName for deterministic lookup.
	byLat := map[float64]string{}
	for _, p := range pts {
		byLat[p.Latitude] = p.PlaceName
	}

	require.Equal(t, "Tokyo, Japan", byLat[35.6], "city+country should produce 'City, Country'")
	require.Equal(t, "Paris", byLat[48.8], "city-only should produce just the city name")
	require.Equal(t, "", byLat[51.5], "no geo row should produce empty string")
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
	// bbox 是基于 ML 输入图的绝对像素坐标（400×300 测试图上的人脸框）。
	_, err = db.Exec(`INSERT INTO face_detections(id, asset_id, bbox, embedding) VALUES('face1','fa',?,?)`,
		`{"x1":100,"y1":75,"x2":240,"y2":210}`, sqlite.SerializeFloat32(make([]float32, common.FaceDim)))
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO persons(id, name, cover_asset_id, cover_face_id) VALUES('pp','','fa','face1')`)
	require.NoError(t, err)

	ps := service.NewPersonService(db)
	cacheDir := filepath.Join(dir, "face-thumbs")
	out, err := ps.FaceThumbnail("pp", cacheDir, "")
	require.NoError(t, err)
	st, err := os.Stat(out)
	require.NoError(t, err)
	require.Greater(t, st.Size(), int64(0))

	// 第二次命中缓存：路径相同且不重建（mtime 不变即缓存命中）
	stat1, _ := os.Stat(out)
	out2, err := ps.FaceThumbnail("pp", cacheDir, "")
	require.NoError(t, err)
	require.Equal(t, out, out2)
	stat2, _ := os.Stat(out2)
	require.Equal(t, stat1.ModTime(), stat2.ModTime())

	// 不存在 person 返回 ErrNotFound
	_, err = ps.FaceThumbnail("no-such", cacheDir, "")
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
		`{"x1":100,"y1":75,"x2":240,"y2":210}`, sqlite.SerializeFloat32(make([]float32, common.FaceDim)))
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO persons(id, name, cover_asset_id, cover_face_id, hidden) VALUES('ph','','fh','face-h',1)`)
	require.NoError(t, err)

	ps := service.NewPersonService(db)
	_, err = ps.FaceThumbnail("ph", filepath.Join(dir, "fh-cache"), "")
	require.ErrorIs(t, err, service.ErrNotFound)
}

// 视频源：bbox 基于关键帧（asset_exif W/H = 1920×1080），thumb large.jpg 是 1280×720。
// FaceThumbnail 必须按 thumb/exif 比例缩放 bbox，否则坐标爆掉。
func TestFaceThumbnail_VideoScalesBBoxToThumb(t *testing.T) {
	db := makeTestFaceDB(t)
	dir := t.TempDir()

	thumbDir := filepath.Join(dir, "thumbs")
	assetID := "fv"
	largeDir := filepath.Join(thumbDir, assetID)
	require.NoError(t, os.MkdirAll(largeDir, 0o755))
	writeTestJPEG(t, filepath.Join(largeDir, "large.jpg"), 1280, 720)

	_, err := db.Exec(`INSERT INTO assets(id, file_path, mime_type, status) VALUES(?, '/x.mp4', 'video/mp4', 'indexed')`, assetID)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO asset_exif(asset_id, width, height) VALUES(?, 1920, 1080)`, assetID)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO face_detections(id, asset_id, bbox, embedding) VALUES('fv-face',?,?,?)`,
		assetID, `{"x1":480,"y1":270,"x2":960,"y2":810}`, sqlite.SerializeFloat32(make([]float32, common.FaceDim)))
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO persons(id, name, cover_asset_id, cover_face_id) VALUES('pv','','` + assetID + `','fv-face')`)
	require.NoError(t, err)

	ps := service.NewPersonService(db)
	out, err := ps.FaceThumbnail("pv", filepath.Join(dir, "face-thumbs"), thumbDir)
	require.NoError(t, err)
	st, err := os.Stat(out)
	require.NoError(t, err)
	require.Greater(t, st.Size(), int64(0))
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

func TestDetachAssetsFromPerson_MarksExcludedAndUnbinds(t *testing.T) {
	db := makeTestFaceDB(t)
	dim := 512
	a := make([]float32, dim)
	a[0] = 1.0
	insertAssetFace(t, db, "d-a1", normalize(a))
	insertAssetFace(t, db, "d-a2", normalize(a))
	insertAssetFace(t, db, "d-a3", normalize(a))
	require.NoError(t, service.NewFaceService(db).RunClustering(context.Background()))
	list, _ := service.NewPersonService(db).ListPersons()
	require.Len(t, list, 1)
	personID := list[0].ID
	require.Equal(t, 3, list[0].Count)

	ps := service.NewPersonService(db)
	removed, err := ps.DetachAssetsFromPerson(personID, []string{"d-a1", "d-a2"})
	require.NoError(t, err)
	require.Equal(t, 2, removed)

	// face_person 行减少
	var bound int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM face_person WHERE person_id=?`, personID).Scan(&bound))
	require.Equal(t, 1, bound)

	// 被移除的脸 excluded=1
	var excl int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM face_detections WHERE excluded=1`).Scan(&excl))
	require.Equal(t, 2, excl)

	// 重跑聚类，excluded 脸不会再被聚回该 person
	require.NoError(t, service.NewFaceService(db).RunClustering(context.Background()))
	list2, _ := ps.ListPersons()
	for _, p := range list2 {
		require.LessOrEqual(t, p.Count, 1, "excluded faces 不应再被聚回任何 person")
	}
}

func TestDetachAssetsFromPerson_AutomaticEmptyPersonDeleted(t *testing.T) {
	db := makeTestFaceDB(t)
	dim := 512
	a := make([]float32, dim)
	a[0] = 1.0
	insertAssetFace(t, db, "de-a1", normalize(a))
	require.NoError(t, service.NewFaceService(db).RunClustering(context.Background()))
	list, _ := service.NewPersonService(db).ListPersons()
	require.Len(t, list, 1)
	personID := list[0].ID

	ps := service.NewPersonService(db)
	_, err := ps.DetachAssetsFromPerson(personID, []string{"de-a1"})
	require.NoError(t, err)

	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM persons WHERE id=?`, personID).Scan(&n))
	require.Equal(t, 0, n, "自动 person 移出最后一张脸后应被删除")
}

func TestDetachAssetsFromPerson_AnchoredEmptyPersonKept(t *testing.T) {
	db := makeTestFaceDB(t)
	dim := 512
	a := make([]float32, dim)
	a[0] = 1.0
	insertAssetFace(t, db, "dk-a1", normalize(a))
	require.NoError(t, service.NewFaceService(db).RunClustering(context.Background()))
	list, _ := service.NewPersonService(db).ListPersons()
	require.Len(t, list, 1)
	personID := list[0].ID

	// 给该 person 取个名变成锚定
	_, err := db.Exec(`UPDATE persons SET name='Alice' WHERE id=?`, personID)
	require.NoError(t, err)

	ps := service.NewPersonService(db)
	_, err = ps.DetachAssetsFromPerson(personID, []string{"dk-a1"})
	require.NoError(t, err)

	// 锚定 person 应被保留，但 cover/centroid 清空
	var name string
	var coverFace sql.NullString
	require.NoError(t, db.QueryRow(`SELECT name, cover_face_id FROM persons WHERE id=?`, personID).Scan(&name, &coverFace))
	require.Equal(t, "Alice", name)
	require.False(t, coverFace.Valid)
}

func TestDetachAssetsFromPerson_EmptyAssetsNoOp(t *testing.T) {
	db := makeTestFaceDB(t)
	dim := 512
	a := make([]float32, dim)
	a[0] = 1.0
	insertAssetFace(t, db, "dn-a1", normalize(a))
	require.NoError(t, service.NewFaceService(db).RunClustering(context.Background()))
	list, _ := service.NewPersonService(db).ListPersons()
	personID := list[0].ID

	ps := service.NewPersonService(db)
	removed, err := ps.DetachAssetsFromPerson(personID, nil)
	require.NoError(t, err)
	require.Equal(t, 0, removed)
}

func TestDetachAssetsFromPerson_NotFound(t *testing.T) {
	db := makeTestFaceDB(t)
	ps := service.NewPersonService(db)
	_, err := ps.DetachAssetsFromPerson("nonexistent", []string{"x"})
	require.ErrorIs(t, err, service.ErrNotFound)
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

// ---------------------------------------------------------------------------
// PurgePerson tests
// ---------------------------------------------------------------------------

func TestPurgePerson_DeletesPersonAndExcludesFaces(t *testing.T) {
	db := makeTestFaceDB(t)
	dim := 512
	a := make([]float32, dim)
	a[0] = 1.0
	// Two faces on two different assets grouped into one person.
	insertAssetFace(t, db, "pur-a1", normalize(a))
	insertAssetFace(t, db, "pur-a2", normalize(a))
	require.NoError(t, service.NewFaceService(db).RunClustering(context.Background()))

	list, _ := service.NewPersonService(db).ListPersons()
	require.Len(t, list, 1)
	personID := list[0].ID

	ps := service.NewPersonService(db)
	require.NoError(t, ps.PurgePerson(personID))

	// persons row must be gone.
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM persons WHERE id=?`, personID).Scan(&n))
	require.Equal(t, 0, n, "persons row must be deleted after purge")

	// face_person binding must be gone.
	var fp int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM face_person WHERE person_id=?`, personID).Scan(&fp))
	require.Equal(t, 0, fp, "face_person rows must be deleted after purge")

	// All face_detections that belonged to this person must be excluded=1.
	var notExcluded int
	require.NoError(t, db.QueryRow(`
		SELECT COUNT(*) FROM face_detections
		WHERE asset_id IN ('pur-a1','pur-a2') AND excluded=0`).Scan(&notExcluded))
	require.Equal(t, 0, notExcluded, "all face_detections for purged person must be excluded")

	// Assets must be untouched.
	var assets int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM assets WHERE id IN ('pur-a1','pur-a2')`).Scan(&assets))
	require.Equal(t, 2, assets, "assets must not be affected by purge")
}

func TestPurgePerson_AnchoredPersonCanBePurged(t *testing.T) {
	db := makeTestFaceDB(t)
	dim := 512
	a := make([]float32, dim)
	a[0] = 1.0
	insertAssetFace(t, db, "anc-a1", normalize(a))
	require.NoError(t, service.NewFaceService(db).RunClustering(context.Background()))

	personID := mustFirstPersonID(t, db)

	// Anchor the person with a name and mark as favorite.
	_, err := db.Exec(`UPDATE persons SET name='Alice', favorite=1 WHERE id=?`, personID)
	require.NoError(t, err)

	ps := service.NewPersonService(db)
	// purge is an explicit user action — anchoring must not protect against it.
	require.NoError(t, ps.PurgePerson(personID))

	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM persons WHERE id=?`, personID).Scan(&n))
	require.Equal(t, 0, n, "anchored person must still be purged")
}

func TestPurgePerson_NotFound(t *testing.T) {
	db := makeTestFaceDB(t)
	ps := service.NewPersonService(db)
	err := ps.PurgePerson("nonexistent-id")
	require.ErrorIs(t, err, service.ErrNotFound)
}

func TestPurgePerson_RecluterDoesNotResurrect(t *testing.T) {
	db := makeTestFaceDB(t)
	dim := 512
	a := make([]float32, dim)
	a[0] = 1.0
	insertAssetFace(t, db, "res-a1", normalize(a))
	insertAssetFace(t, db, "res-a2", normalize(a))
	require.NoError(t, service.NewFaceService(db).RunClustering(context.Background()))

	list, _ := service.NewPersonService(db).ListPersons()
	require.Len(t, list, 1)
	personID := list[0].ID

	ps := service.NewPersonService(db)
	require.NoError(t, ps.PurgePerson(personID))

	// Re-run clustering: excluded faces must not form a new person.
	require.NoError(t, service.NewFaceService(db).RunClustering(context.Background()))
	list2, _ := ps.ListPersons()
	for _, p := range list2 {
		require.NotEqual(t, personID, p.ID, "purged person must not be resurrected")
	}
	// All faces remain excluded — they must not be bound to any new person.
	var bound int
	require.NoError(t, db.QueryRow(`
		SELECT COUNT(*) FROM face_person fp
		JOIN face_detections fd ON fd.id=fp.face_id
		WHERE fd.asset_id IN ('res-a1','res-a2')`).Scan(&bound))
	require.Equal(t, 0, bound, "excluded faces must not be re-bound after recluster")
}

// ---------------------------------------------------------------------------
// SetPersonCover / UnlockPersonCover tests
// ---------------------------------------------------------------------------

// insertFaceWithBBox inserts a face detection with the given bbox and returns its ID.
func insertFaceWithBBox(t *testing.T, db *sql.DB, assetID, faceID, bbox string, vec []float32) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO face_detections(id, asset_id, bbox, embedding) VALUES(?,?,?,?)`,
		faceID, assetID, bbox, sqlite.SerializeFloat32(vec))
	require.NoError(t, err)
}

func TestSetPersonCover_SuccessSelectsLargestBBox(t *testing.T) {
	db := makeTestFaceDB(t)
	dim := 512
	v := make([]float32, dim)
	v[0] = 1.0
	// Two faces on the same asset with different bbox sizes.
	insertAssetFace(t, db, "sc-a1", normalize(v)) // small bbox (inserted via insertAssetFace, bbox={})
	// Insert a second face on sc-a1 with a larger explicit bbox.
	insertFaceWithBBox(t, db, "sc-a1", "sc-face-big",
		`{"x1":0,"y1":0,"x2":200,"y2":200}`, normalize(v))
	// Insert a third face on a different asset to give the person >1 asset.
	insertAssetFace(t, db, "sc-a2", normalize(v))

	require.NoError(t, service.NewFaceService(db).RunClustering(context.Background()))
	personID := mustFirstPersonID(t, db)

	ps := service.NewPersonService(db)
	gotFaceID, err := ps.SetPersonCover(personID, "sc-a1")
	require.NoError(t, err)
	// The face with the bigger bbox (sc-face-big) should be chosen.
	require.Equal(t, "sc-face-big", gotFaceID)

	// Verify DB state.
	var coverFace, coverAsset string
	var locked int
	require.NoError(t, db.QueryRow(
		`SELECT COALESCE(cover_face_id,''), COALESCE(cover_asset_id,''), cover_locked FROM persons WHERE id=?`, personID,
	).Scan(&coverFace, &coverAsset, &locked))
	require.Equal(t, "sc-face-big", coverFace)
	require.Equal(t, "sc-a1", coverAsset)
	require.Equal(t, 1, locked, "cover_locked must be 1 after SetPersonCover")
}

func TestSetPersonCover_NoFaceOnAsset_ReturnsNotFound(t *testing.T) {
	db := makeTestFaceDB(t)
	dim := 512
	v := make([]float32, dim)
	v[0] = 1.0
	insertAssetFace(t, db, "scnf-a1", normalize(v))
	require.NoError(t, service.NewFaceService(db).RunClustering(context.Background()))
	personID := mustFirstPersonID(t, db)

	ps := service.NewPersonService(db)
	// Try to set cover using an asset that has no face for this person.
	_, err := ps.SetPersonCover(personID, "nonexistent-asset")
	require.ErrorIs(t, err, service.ErrNotFound)
}

func TestSetPersonCover_PersonNotFound_ReturnsNotFound(t *testing.T) {
	db := makeTestFaceDB(t)
	ps := service.NewPersonService(db)
	_, err := ps.SetPersonCover("no-person", "some-asset")
	require.ErrorIs(t, err, service.ErrNotFound)
}

func TestRecomputeOneCentroid_LockedCoverNotOverwritten(t *testing.T) {
	// After SetPersonCover, a recompute triggered by (e.g.) DetachAssetsFromPerson
	// must NOT overwrite the locked cover face.
	db := makeTestFaceDB(t)
	dim := 512
	v := make([]float32, dim)
	v[0] = 1.0
	insertAssetFace(t, db, "lk-a1", normalize(v))
	insertAssetFace(t, db, "lk-a2", normalize(v))
	insertAssetFace(t, db, "lk-a3", normalize(v))
	require.NoError(t, service.NewFaceService(db).RunClustering(context.Background()))
	personID := mustFirstPersonID(t, db)

	ps := service.NewPersonService(db)
	// Lock cover to lk-a1.
	_, err := ps.SetPersonCover(personID, "lk-a1")
	require.NoError(t, err)

	// Detach lk-a3; this triggers recomputeOneCentroidTx internally.
	_, err = ps.DetachAssetsFromPerson(personID, []string{"lk-a3"})
	require.NoError(t, err)

	// Cover should still be on lk-a1.
	var coverAsset string
	var locked int
	require.NoError(t, db.QueryRow(
		`SELECT COALESCE(cover_asset_id,''), cover_locked FROM persons WHERE id=?`, personID,
	).Scan(&coverAsset, &locked))
	require.Equal(t, "lk-a1", coverAsset, "locked cover must survive recompute")
	require.Equal(t, 1, locked)
}

func TestRecomputeOneCentroid_LockedCoverFaceDetached_LockCleared(t *testing.T) {
	// When the locked cover face is itself detached, the lock should be cleared
	// and cover reselected automatically.
	db := makeTestFaceDB(t)
	dim := 512
	v := make([]float32, dim)
	v[0] = 1.0
	insertAssetFace(t, db, "lkd-a1", normalize(v))
	insertAssetFace(t, db, "lkd-a2", normalize(v))
	require.NoError(t, service.NewFaceService(db).RunClustering(context.Background()))
	personID := mustFirstPersonID(t, db)

	ps := service.NewPersonService(db)
	// Lock cover to lkd-a1.
	_, err := ps.SetPersonCover(personID, "lkd-a1")
	require.NoError(t, err)

	// Detach the locked asset (lkd-a1); lock must be cleared and cover reselected.
	_, err = ps.DetachAssetsFromPerson(personID, []string{"lkd-a1"})
	require.NoError(t, err)

	var coverAsset string
	var locked int
	require.NoError(t, db.QueryRow(
		`SELECT COALESCE(cover_asset_id,''), cover_locked FROM persons WHERE id=?`, personID,
	).Scan(&coverAsset, &locked))
	require.Equal(t, 0, locked, "lock must be cleared when locked face is detached")
	// The remaining face (lkd-a2) should now be the cover.
	require.Equal(t, "lkd-a2", coverAsset)
}

func TestUnlockPersonCover_RecomputesAndReturnsNewFaceID(t *testing.T) {
	db := makeTestFaceDB(t)
	dim := 512
	v := make([]float32, dim)
	v[0] = 1.0
	insertAssetFace(t, db, "ul-a1", normalize(v))
	insertAssetFace(t, db, "ul-a2", normalize(v))
	require.NoError(t, service.NewFaceService(db).RunClustering(context.Background()))
	personID := mustFirstPersonID(t, db)

	ps := service.NewPersonService(db)
	// Lock to ul-a1.
	_, err := ps.SetPersonCover(personID, "ul-a1")
	require.NoError(t, err)

	// Unlock: should clear the lock and recompute.
	newFaceID, err := ps.UnlockPersonCover(personID)
	require.NoError(t, err)
	require.NotEmpty(t, newFaceID)

	var locked int
	require.NoError(t, db.QueryRow(`SELECT cover_locked FROM persons WHERE id=?`, personID).Scan(&locked))
	require.Equal(t, 0, locked, "lock must be 0 after UnlockPersonCover")
}

func TestUnlockPersonCover_PersonNotFound(t *testing.T) {
	db := makeTestFaceDB(t)
	ps := service.NewPersonService(db)
	_, err := ps.UnlockPersonCover("no-such")
	require.ErrorIs(t, err, service.ErrNotFound)
}

// ---------------------------------------------------------------------------
// heroAssetId patch tests
// ---------------------------------------------------------------------------

func TestUpdatePerson_HeroAssetID_SetAndClear(t *testing.T) {
	db := makeTestFaceDB(t)
	dim := 512
	v := make([]float32, dim)
	v[0] = 1.0
	insertAssetFace(t, db, "ha-a1", normalize(v))
	require.NoError(t, service.NewFaceService(db).RunClustering(context.Background()))
	personID := mustFirstPersonID(t, db)

	ps := service.NewPersonService(db)

	// Set hero to ha-a1 (has a face for this person).
	heroVal := "ha-a1"
	require.NoError(t, ps.UpdatePerson(personID, service.PersonPatch{HeroAssetID: &heroVal}))

	// GetPerson and ListPersons must return heroAssetId.
	p, err := ps.GetPerson(personID)
	require.NoError(t, err)
	require.Equal(t, "ha-a1", p.HeroAssetID)

	list, err := ps.ListPersons()
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, "ha-a1", list[0].HeroAssetID)

	// Clear hero with empty string.
	empty := ""
	require.NoError(t, ps.UpdatePerson(personID, service.PersonPatch{HeroAssetID: &empty}))
	p2, err := ps.GetPerson(personID)
	require.NoError(t, err)
	require.Empty(t, p2.HeroAssetID)
}

func TestUpdatePerson_HeroAssetID_NoFaceValidationFails(t *testing.T) {
	db := makeTestFaceDB(t)
	dim := 512
	v := make([]float32, dim)
	v[0] = 1.0
	insertAssetFace(t, db, "hav-a1", normalize(v))
	require.NoError(t, service.NewFaceService(db).RunClustering(context.Background()))
	personID := mustFirstPersonID(t, db)

	ps := service.NewPersonService(db)
	// Try to set hero to an asset that has no face for this person.
	badAsset := "nonexistent-asset"
	err := ps.UpdatePerson(personID, service.PersonPatch{HeroAssetID: &badAsset})
	require.ErrorIs(t, err, service.ErrNotFound)
}

func TestUpdatePerson_HeroAssetID_SoftDeletedAssetReturnsEmpty(t *testing.T) {
	// If the hero asset is soft-deleted, GetPerson/ListPersons must return empty heroAssetId.
	db := makeTestFaceDB(t)
	dim := 512
	v := make([]float32, dim)
	v[0] = 1.0
	insertAssetFace(t, db, "hsd-a1", normalize(v))
	require.NoError(t, service.NewFaceService(db).RunClustering(context.Background()))
	personID := mustFirstPersonID(t, db)

	ps := service.NewPersonService(db)
	heroVal := "hsd-a1"
	require.NoError(t, ps.UpdatePerson(personID, service.PersonPatch{HeroAssetID: &heroVal}))

	// Soft-delete the asset.
	_, err := db.Exec(`UPDATE assets SET deleted_at='2026-01-01 00:00:00' WHERE id='hsd-a1'`)
	require.NoError(t, err)

	p, err := ps.GetPerson(personID)
	require.NoError(t, err)
	require.Empty(t, p.HeroAssetID, "soft-deleted hero asset must not be returned")

	list, err := ps.ListPersons()
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Empty(t, list[0].HeroAssetID, "soft-deleted hero asset must not appear in list")
}

// TestListPersons_ExcludesOfflineAssetsFromCounts verifies that a person's
// count/first-last-seen stop counting a face whose asset is on a currently
// unplugged removable drive (offline=1), matching every other list surface.
func TestListPersons_ExcludesOfflineAssetsFromCounts(t *testing.T) {
	db := makeTestFaceDB(t)
	dim := 512
	v := make([]float32, dim)
	v[0] = 1.0
	insertAssetFace(t, db, "cnt-online", normalize(v))
	insertAssetFace(t, db, "cnt-offline", normalize(v))
	require.NoError(t, service.NewFaceService(db).RunClustering(context.Background()))
	personID := mustFirstPersonID(t, db)

	_, err := db.Exec(`UPDATE assets SET offline=1 WHERE id='cnt-offline'`)
	require.NoError(t, err)

	ps := service.NewPersonService(db)
	p, err := ps.GetPerson(personID)
	require.NoError(t, err)
	require.Equal(t, 1, p.Count, "offline 资产的脸不应计入 person 出现次数")

	list, err := ps.ListPersons()
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, 1, list[0].Count, "ListPersons 的 count 同样不应计入 offline 资产")
}

// TestPersonCoverFallsBackToHeroWhenOffline verifies the fix for the
// cover_asset_id/hero_asset_id inconsistency: cover is now validated the same
// way as hero (deleted_at IS NULL AND offline=0). When the pinned cover asset
// goes offline, CoverAssetID falls back to a valid hero, or empty if there is
// none — it never returns a dangling reference to an unreachable asset.
func TestPersonCoverFallsBackToHeroWhenOffline(t *testing.T) {
	db := makeTestFaceDB(t)
	dim := 512
	v := make([]float32, dim)
	v[0] = 1.0
	insertAssetFace(t, db, "cov-a1", normalize(v))
	insertAssetFace(t, db, "cov-a2", normalize(v))
	require.NoError(t, service.NewFaceService(db).RunClustering(context.Background()))
	personID := mustFirstPersonID(t, db)

	ps := service.NewPersonService(db)
	_, err := ps.SetPersonCover(personID, "cov-a1")
	require.NoError(t, err)

	// Sanity: cover resolves to the pinned asset while it is online.
	p, err := ps.GetPerson(personID)
	require.NoError(t, err)
	require.Equal(t, "cov-a1", p.CoverAssetID)

	// The cover's drive goes offline.
	_, err = db.Exec(`UPDATE assets SET offline=1 WHERE id='cov-a1'`)
	require.NoError(t, err)

	// No hero set yet: cover must fall back to empty, not the offline asset.
	p, err = ps.GetPerson(personID)
	require.NoError(t, err)
	require.Empty(t, p.CoverAssetID, "offline cover 应失效为空（无 hero 可回退）")

	list, err := ps.ListPersons()
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Empty(t, list[0].CoverAssetID, "ListPersons 同样应回退为空")

	// Now set a hero on the still-online asset — cover must fall back to it.
	heroVal := "cov-a2"
	require.NoError(t, ps.UpdatePerson(personID, service.PersonPatch{HeroAssetID: &heroVal}))

	p, err = ps.GetPerson(personID)
	require.NoError(t, err)
	require.Equal(t, "cov-a2", p.CoverAssetID, "offline cover 应回退到有效的 hero")

	list, err = ps.ListPersons()
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, "cov-a2", list[0].CoverAssetID, "ListPersons 同样应回退到 hero")
}

func TestDetachAssetsFromPerson_ClearsHeroWhenDetached(t *testing.T) {
	db := makeTestFaceDB(t)
	dim := 512
	v := make([]float32, dim)
	v[0] = 1.0
	insertAssetFace(t, db, "dh-a1", normalize(v))
	insertAssetFace(t, db, "dh-a2", normalize(v))
	require.NoError(t, service.NewFaceService(db).RunClustering(context.Background()))
	personID := mustFirstPersonID(t, db)

	ps := service.NewPersonService(db)
	heroVal := "dh-a1"
	require.NoError(t, ps.UpdatePerson(personID, service.PersonPatch{HeroAssetID: &heroVal}))

	// Detach dh-a1; hero_asset_id must be cleared.
	_, err := ps.DetachAssetsFromPerson(personID, []string{"dh-a1"})
	require.NoError(t, err)

	var heroAsset sql.NullString
	require.NoError(t, db.QueryRow(`SELECT hero_asset_id FROM persons WHERE id=?`, personID).Scan(&heroAsset))
	require.False(t, heroAsset.Valid || heroAsset.String != "", "hero_asset_id must be cleared when its asset is detached")
}

func TestMergePersons_LockedCoverPreserved(t *testing.T) {
	// After merging, into's locked cover must not be overwritten by recompute.
	db := makeTestFaceDB(t)
	dim := 512
	a := make([]float32, dim)
	a[0] = 1.0
	b := make([]float32, dim)
	b[1] = 1.0
	insertAssetFace(t, db, "mlk-a", normalize(a))
	insertAssetFace(t, db, "mlk-b", normalize(b))
	require.NoError(t, service.NewFaceService(db).RunClustering(context.Background()))

	rows, err := db.Query(`SELECT id FROM persons ORDER BY rowid`)
	require.NoError(t, err)
	var pids []string
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		pids = append(pids, id)
	}
	rows.Close()
	require.Len(t, pids, 2)

	from, into := pids[0], pids[1]

	ps := service.NewPersonService(db)

	// Find which asset belongs to into before merging.
	var intoCoverAsset string
	require.NoError(t, db.QueryRow(`SELECT COALESCE(cover_asset_id,'') FROM persons WHERE id=?`, into).Scan(&intoCoverAsset))
	require.NotEmpty(t, intoCoverAsset)

	// Lock the cover for into.
	_, err = ps.SetPersonCover(into, intoCoverAsset)
	require.NoError(t, err)

	// Merge from → into.
	require.NoError(t, service.NewSearchService(db, nil).MergePersons(from, into))

	// into's cover_asset_id must still be intoCoverAsset (lock preserved).
	var coverAsset string
	var locked int
	require.NoError(t, db.QueryRow(
		`SELECT COALESCE(cover_asset_id,''), cover_locked FROM persons WHERE id=?`, into,
	).Scan(&coverAsset, &locked))
	require.Equal(t, intoCoverAsset, coverAsset, "locked cover must be preserved after merge")
	require.Equal(t, 1, locked, "cover_locked must remain 1 after merge")
}
