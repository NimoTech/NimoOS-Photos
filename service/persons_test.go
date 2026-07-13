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

func TestFaceThumbnailOfflineCoverReturnsNotFound(t *testing.T) {
	db := makeTestFaceDB(t)
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.jpg")
	writeTestJPEG(t, srcPath, 400, 300)
	_, err := db.Exec(`INSERT INTO assets(id, file_path, status, offline) VALUES('fo', ?, 'indexed', 1)`, srcPath)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO face_detections(id, asset_id, bbox, embedding) VALUES('face-o','fo',?,?)`,
		`{"x1":100,"y1":75,"x2":240,"y2":210}`, sqlite.SerializeFloat32(make([]float32, common.FaceDim)))
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO persons(id, name, cover_asset_id, cover_face_id) VALUES('po','','fo','face-o')`)
	require.NoError(t, err)

	ps := service.NewPersonService(db)
	_, err = ps.FaceThumbnail("po", filepath.Join(dir, "fo-cache"), "")
	require.ErrorIs(t, err, service.ErrNotFound)
}

// 回归锁定：offline=1 的视频资产即便本地已有 thumbDir/large.jpg 缓存，
// FaceThumbnail 也必须返回 ErrNotFound——"offline 一律隐藏/降级" 对视频源同样
// 成立，不因为缩略图缓存还在本地就绕过。SQL 里的 a.offline=0 过滤本就已经
// 挡住了这种情况，这条测试不改代码，只是把这个既有行为钉死防回归。
func TestFaceThumbnailOfflineVideoWithCachedThumbReturnsNotFound(t *testing.T) {
	db := makeTestFaceDB(t)
	dir := t.TempDir()

	thumbDir := filepath.Join(dir, "thumbs")
	assetID := "fov"
	largeDir := filepath.Join(thumbDir, assetID)
	require.NoError(t, os.MkdirAll(largeDir, 0o755))
	writeTestJPEG(t, filepath.Join(largeDir, "large.jpg"), 1280, 720)

	_, err := db.Exec(`INSERT INTO assets(id, file_path, mime_type, status, offline) VALUES(?, '/x.mp4', 'video/mp4', 'indexed', 1)`, assetID)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO asset_exif(asset_id, width, height) VALUES(?, 1920, 1080)`, assetID)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO face_detections(id, asset_id, bbox, embedding) VALUES('fov-face',?,?,?)`,
		assetID, `{"x1":480,"y1":270,"x2":960,"y2":810}`, sqlite.SerializeFloat32(make([]float32, common.FaceDim)))
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO persons(id, name, cover_asset_id, cover_face_id) VALUES('pov','','` + assetID + `','fov-face')`)
	require.NoError(t, err)

	ps := service.NewPersonService(db)
	_, err = ps.FaceThumbnail("pov", filepath.Join(dir, "face-thumbs"), thumbDir)
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

// ---------------------------------------------------------------------------
// 封面混合分 + hero 兜底
// ---------------------------------------------------------------------------

// setAssetAesthetic 更新指定 asset 的 aesthetic_score（nil 表示置 NULL）。
func setAssetAesthetic(t *testing.T, db *sql.DB, assetID string, score *float64) {
	t.Helper()
	var err error
	if score == nil {
		_, err = db.Exec(`UPDATE assets SET aesthetic_score=NULL WHERE id=?`, assetID)
	} else {
		_, err = db.Exec(`UPDATE assets SET aesthetic_score=? WHERE id=?`, *score, assetID)
	}
	require.NoError(t, err)
}

// setAssetExifSize 写入/更新 asset 的 EXIF 宽高（用于混合分的脸面积占比计算）。
func setAssetExifSize(t *testing.T, db *sql.DB, assetID string, w, h int) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO asset_exif(asset_id, width, height) VALUES(?,?,?)
ON CONFLICT(asset_id) DO UPDATE SET width=excluded.width, height=excluded.height`, assetID, w, h)
	require.NoError(t, err)
}

func setFaceBBox(t *testing.T, db *sql.DB, faceID, bboxJSON string) {
	t.Helper()
	_, err := db.Exec(`UPDATE face_detections SET bbox=? WHERE id=?`, bboxJSON, faceID)
	require.NoError(t, err)
}

func f64(v float64) *float64 { return &v }

// TestPersonCoverHybridSelection 验证封面选取从"质心最近"改为"混合分最高"：
// 混合分 = clamp01((score-1)/9) × min(1, faceArea/imageArea)。
// 三张脸同属一个 person（f1/f2/f3embedding 互相靠近可聚为一簇）：
//
//	f1: asset 分 9.0，脸占比 0.01 → hybrid = 0.8889*0.01 ≈ 0.0089
//	f2: asset 分 7.0，脸占比 0.30 → hybrid = 0.6667*0.30 = 0.20  ← 应当选
//	f3: asset 分 NULL（无分）      → 不可比（-1）
//
// 把 f2 的 asset 分置 NULL 后重算：f2/f3 都不可比，只剩 f1 可比 → 应选 f1。
// 再把 f1 的 asset 分也置 NULL（全 NULL）→ 退回质心最近（原有行为）；
// 三个 embedding 特意构造为 f1 恰好与质心方向重合，距离为 0，验证退回后选中 f1。
func TestPersonCoverHybridSelection(t *testing.T) {
	db := makeTestFaceDB(t)
	dim := 512

	// f1: 沿 dim0 的单位向量。
	v1 := make([]float32, dim)
	v1[0] = 1.0
	// f2: dim0=1, dim1=0.3，归一化后与 f1 靠近但不重合。
	v2raw := make([]float32, dim)
	v2raw[0] = 1.0
	v2raw[1] = 0.3
	// f3: dim0=1, dim1=-0.3，归一化后与 f2 对称。
	v3raw := make([]float32, dim)
	v3raw[0] = 1.0
	v3raw[1] = -0.3

	f1 := insertAssetFace(t, db, "hy-a1", normalize(v1))
	f2 := insertAssetFace(t, db, "hy-a2", normalize(v2raw))
	f3 := insertAssetFace(t, db, "hy-a3", normalize(v3raw))
	require.NoError(t, service.NewFaceService(db).RunClustering(context.Background()))
	personID := mustFirstPersonID(t, db)

	// 三张 asset 统一 1000x1000 图幅。
	for _, aid := range []string{"hy-a1", "hy-a2", "hy-a3"} {
		setAssetExifSize(t, db, aid, 1000, 1000)
	}
	// f1: 脸占比 0.01（100x100=10000/1e6），分 9.0。
	setFaceBBox(t, db, f1, `{"x1":0,"y1":0,"x2":100,"y2":100}`)
	setAssetAesthetic(t, db, "hy-a1", f64(9.0))
	// f2: 脸占比 0.30（约 547.72^2=300000/1e6），分 7.0。
	setFaceBBox(t, db, f2, `{"x1":0,"y1":0,"x2":547.7226,"y2":547.7226}`)
	setAssetAesthetic(t, db, "hy-a2", f64(7.0))
	// f3: 脸占比 0.50，但 asset 分 NULL → 不可比。
	setFaceBBox(t, db, f3, `{"x1":0,"y1":0,"x2":707.1068,"y2":707.1068}`)
	setAssetAesthetic(t, db, "hy-a3", nil)

	ps := service.NewPersonService(db)

	// UnlockPersonCover 触发 recomputeOneCentroidTx；person 此时未锁，不受影响。
	newFace, err := ps.UnlockPersonCover(personID)
	require.NoError(t, err)
	require.Equal(t, f2, newFace, "混合分最高的 f2 应被选为封面")

	var coverAsset string
	require.NoError(t, db.QueryRow(`SELECT cover_asset_id FROM persons WHERE id=?`, personID).Scan(&coverAsset))
	require.Equal(t, "hy-a2", coverAsset)

	// 把 f2 的分置 NULL：f2/f3 都不可比，只剩 f1 可比 → 应选 f1。
	setAssetAesthetic(t, db, "hy-a2", nil)
	newFace, err = ps.UnlockPersonCover(personID)
	require.NoError(t, err)
	require.Equal(t, f1, newFace, "f2 不可比后应回退到唯一可比的 f1")

	// 全 NULL：退回质心最近。质心方向与 f1 完全重合（cosDist=0），应选 f1。
	setAssetAesthetic(t, db, "hy-a1", nil)
	newFace, err = ps.UnlockPersonCover(personID)
	require.NoError(t, err)
	require.Equal(t, f1, newFace, "全部不可比时应退回质心最近，此处质心与 f1 方向重合")
}

// TestPersonCoverLockedUnaffected 验证 cover_locked=1 且锁脸仍有效时，
// 混合分选优不得改写封面 —— 即便另一张脸的混合分明显更高。
func TestPersonCoverLockedUnaffected(t *testing.T) {
	db := makeTestFaceDB(t)
	dim := 512
	v := make([]float32, dim)
	v[0] = 1.0
	f1 := insertAssetFace(t, db, "lkh-a1", normalize(v))
	f2 := insertAssetFace(t, db, "lkh-a2", normalize(v))
	f3 := insertAssetFace(t, db, "lkh-a3", normalize(v)) // 用于触发一次与锁无关的 detach 重算
	require.NoError(t, service.NewFaceService(db).RunClustering(context.Background()))
	personID := mustFirstPersonID(t, db)

	for _, aid := range []string{"lkh-a1", "lkh-a2", "lkh-a3"} {
		setAssetExifSize(t, db, aid, 1000, 1000)
	}
	// f1（将被锁定）混合分很低：小脸 + 低分。
	setFaceBBox(t, db, f1, `{"x1":0,"y1":0,"x2":50,"y2":50}`)
	setAssetAesthetic(t, db, "lkh-a1", f64(2.0))
	// f2 混合分远高于 f1：大脸 + 满分。
	setFaceBBox(t, db, f2, `{"x1":0,"y1":0,"x2":900,"y2":900}`)
	setAssetAesthetic(t, db, "lkh-a2", f64(10.0))
	_ = f3

	ps := service.NewPersonService(db)
	// 锁定封面到 f1 所在的 asset。
	lockedFaceID, err := ps.SetPersonCover(personID, "lkh-a1")
	require.NoError(t, err)
	require.Equal(t, f1, lockedFaceID)

	// 触发一次与锁无关的 recompute（detach 一个不相关的 asset），不应改写锁定的封面。
	_, err = ps.DetachAssetsFromPerson(personID, []string{"lkh-a3"})
	require.NoError(t, err)

	var coverAsset, coverFace string
	var locked int
	require.NoError(t, db.QueryRow(
		`SELECT COALESCE(cover_asset_id,''), COALESCE(cover_face_id,''), cover_locked FROM persons WHERE id=?`, personID,
	).Scan(&coverAsset, &coverFace, &locked))
	require.Equal(t, "lkh-a1", coverAsset, "锁定封面不应被混合分更高的 f2 改写")
	require.Equal(t, f1, coverFace)
	require.Equal(t, 1, locked)
}

// TestPersonHeroAestheticFallback 验证 hero 未手动设置时回退为该人物照片中
// 美学分最高者；已手动设置时保持不变（即便其他照片分更高）。
func TestPersonHeroAestheticFallback(t *testing.T) {
	db := makeTestFaceDB(t)
	dim := 512
	v := make([]float32, dim)
	v[0] = 1.0
	insertAssetFace(t, db, "hf-a1", normalize(v))
	insertAssetFace(t, db, "hf-a2", normalize(v))
	require.NoError(t, service.NewFaceService(db).RunClustering(context.Background()))
	personID := mustFirstPersonID(t, db)

	setAssetAesthetic(t, db, "hf-a1", f64(5.0))
	setAssetAesthetic(t, db, "hf-a2", f64(9.0))

	ps := service.NewPersonService(db)

	// 未设置 hero：应回退为分数最高的 hf-a2。
	p, err := ps.GetPerson(personID)
	require.NoError(t, err)
	require.Equal(t, "hf-a2", p.HeroAssetID, "hero 未设置时应回退到美学分最高的照片")

	list, err := ps.ListPersons()
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, "hf-a2", list[0].HeroAssetID, "ListPersons 的 hero 兜底同样应生效")

	// 手动设置 hero 为分数较低的 hf-a1：应保持不变，不被兜底覆盖。
	heroVal := "hf-a1"
	require.NoError(t, ps.UpdatePerson(personID, service.PersonPatch{HeroAssetID: &heroVal}))

	p2, err := ps.GetPerson(personID)
	require.NoError(t, err)
	require.Equal(t, "hf-a1", p2.HeroAssetID, "已手动设置的 hero 不应被美学兜底覆盖")

	list2, err := ps.ListPersons()
	require.NoError(t, err)
	require.Len(t, list2, 1)
	require.Equal(t, "hf-a1", list2[0].HeroAssetID)
}

// TestRecomputeOneCentroid_AllFacesIncomparable_FallbackPicksValidFace 是一个回归测试:
// 封面选优主路径(混合分)全不可比时,会退回质心最近的兜底循环。构造两张完全反向的
// 单位向量脸——质心因此退化为零向量,cosDist(v, centroid) 对零向量恒返回 1.0——
// 验证兜底循环最终仍落在合法脸索引上而不 panic(修复前 best 初值为 -1,理论边界下
// 若循环体一次都不更新 best,会以 -1 越界访问 faceIDs/assetIDs)。
func TestRecomputeOneCentroid_AllFacesIncomparable_FallbackPicksValidFace(t *testing.T) {
	db := makeTestFaceDB(t)
	dim := 512

	// f1: 沿 dim0 的单位向量。
	v1 := make([]float32, dim)
	v1[0] = 1.0
	// f2: 与 f1 完全反向的单位向量;二者质心退化为零向量。
	v2 := make([]float32, dim)
	v2[0] = -1.0

	f1 := insertAssetFace(t, db, "rc-a1", v1)
	f2 := insertAssetFace(t, db, "rc-a2", v2)

	// 手动建人物 + 成员关系,跳过 RunClustering:两张脸 cosDist=2.0,远超 DBSCAN
	// epsilon,天然不会被聚成同一人物,这里直接用 SQL 拼出「同一人物下全不可比」场景。
	personID := "rc-person"
	_, err := db.Exec(`INSERT INTO persons(id, name) VALUES(?, '')`, personID)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO face_person(face_id, person_id) VALUES(?, ?), (?, ?)`,
		f1, personID, f2, personID)
	require.NoError(t, err)

	// 两张脸均无美学分/EXIF 尺寸 → hybridCoverScore 全部返回 -1,全不可比,
	// 触发质心兜底循环。
	ps := service.NewPersonService(db)
	newFace, err := ps.UnlockPersonCover(personID)
	require.NoError(t, err, "全不可比场景下不应 panic 或报错")
	require.Contains(t, []string{f1, f2}, newFace, "应落在成员脸之一,而非越界索引")

	var coverAsset string
	require.NoError(t, db.QueryRow(`SELECT cover_asset_id FROM persons WHERE id=?`, personID).Scan(&coverAsset))
	require.Contains(t, []string{"rc-a1", "rc-a2"}, coverAsset)
}
