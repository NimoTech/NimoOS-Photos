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
	// Give the two assets different GPS (different coarse-grained cell), asset_exif rows
	_, err := db.Exec(`INSERT INTO asset_exif(asset_id, latitude, longitude) VALUES('pl-a1', 35.6, 139.6)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO asset_exif(asset_id, latitude, longitude) VALUES('pl-a2', 37.7, -122.4)`)
	require.NoError(t, err)

	require.NoError(t, service.NewFaceService(db).RunClustering(context.Background()))
	list, err := service.NewPersonService(db).ListPersons()
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, 2, list[0].PlacesCount)

	// After soft-deleting a2, places should drop to 1
	_, err = db.Exec(`UPDATE assets SET deleted_at='2026-05-01 00:00:00' WHERE id='pl-a2'`)
	require.NoError(t, err)
	list2, err := service.NewPersonService(db).ListPersons()
	require.NoError(t, err)
	require.Len(t, list2, 1)
	require.Equal(t, 1, list2[0].PlacesCount)
}

func TestFacesIndexedUpTo(t *testing.T) {
	db := makeTestFaceDB(t)
	// Empty database
	ps := service.NewPersonService(db)
	ts, err := ps.FacesIndexedUpTo()
	require.NoError(t, err)
	require.Nil(t, ts)

	// Has a face: insert asset + indexed_at + face
	_, err = db.Exec(`INSERT INTO assets(id, file_path, status, indexed_at) VALUES('fi-a', '/x/fi-a.jpg', 'indexed', '2026-05-01 12:00:00')`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO face_detections(id, asset_id, bbox, embedding) VALUES('fi-f', 'fi-a', '{}', X'00000000')`)
	require.NoError(t, err)

	ts2, err := ps.FacesIndexedUpTo()
	require.NoError(t, err)
	require.NotNil(t, ts2)
	require.Contains(t, *ts2, "2026")
}

// A named person must outrank a higher-count unnamed cluster in ListPersons.
// This matters even though the People page splits named/unnamed into separate
// sections client-side: PhotosPersonDetail's merge-target picker reads the
// unfiltered list directly, so the raw backend order is user-visible there.
func TestListPersons_NamedOutranksHigherCountUnnamed(t *testing.T) {
	db := makeTestFaceDB(t)
	dim := 512
	v1 := make([]float32, dim)
	v1[0] = 1.0
	v2 := make([]float32, dim)
	v2[1] = 1.0
	v3 := make([]float32, dim)
	v3[2] = 1.0

	f1 := insertAssetFace(t, db, "no-a1", normalize(v1))
	f2 := insertAssetFace(t, db, "no-a2", normalize(v2)) // second photo -> unnamed cluster cnt=2
	f3 := insertAssetFace(t, db, "no-a3", normalize(v3)) // sole photo -> named person cnt=1

	_, err := db.Exec(`INSERT INTO persons(id, name) VALUES('unnamed-cluster', ''), ('bob', 'Bob')`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO face_person(face_id, person_id) VALUES(?, 'unnamed-cluster'), (?, 'unnamed-cluster'), (?, 'bob')`,
		f1, f2, f3)
	require.NoError(t, err)

	list, err := service.NewPersonService(db).ListPersons()
	require.NoError(t, err)
	require.Len(t, list, 2)
	require.Equal(t, "bob", list[0].ID, "named person should rank first despite lower count")
	require.Equal(t, 1, list[0].Count)
	require.Equal(t, "unnamed-cluster", list[1].ID)
	require.Equal(t, 2, list[1].Count)
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
	// Put faces A and B on the same asset → co-occurrence
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

	// At least one point should carry TakenAt (pp-a1 has taken_at)
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

	// Both persons are named
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
	// bbox is in absolute pixel coordinates on the ML input image (a face box on a 400×300 test image).
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

	// Second call hits the cache: same path and no rebuild (unchanged mtime means cache hit)
	stat1, _ := os.Stat(out)
	out2, err := ps.FaceThumbnail("pp", cacheDir, "")
	require.NoError(t, err)
	require.Equal(t, out, out2)
	stat2, _ := os.Stat(out2)
	require.Equal(t, stat1.ModTime(), stat2.ModTime())

	// A nonexistent person returns ErrNotFound
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

// Regression lock: even when a video asset with offline=1 already has a local
// thumbDir/large.jpg cache, FaceThumbnail must still return ErrNotFound —
// "offline always hides/degrades" holds just the same for video sources, and
// isn't bypassed just because a thumbnail cache still sits locally. The
// a.offline=0 filter in the SQL already blocks this case; this test doesn't
// change any code, it just pins down this existing behavior against
// regression.
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

// Video source: bbox is based on the keyframe (asset_exif W/H = 1920×1080),
// the thumb large.jpg is 1280×720. FaceThumbnail must scale the bbox by the
// thumb/exif ratio, otherwise the coordinates blow up.
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
	// The two clusters' centroid cosine distance falls within the (dbscanEpsilon=0.6, suggestEpsilon=0.75) band
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
	require.NotEmpty(t, sugs, "at least one suggestion should fall within the distance band")

	require.NoError(t, ps.RejectMerge(sugs[0].FromID, sugs[0].IntoID))
	sugs2, err := ps.MergeSuggestions()
	require.NoError(t, err)
	for _, s := range sugs2 {
		require.False(t,
			(s.FromID == sugs[0].FromID && s.IntoID == sugs[0].IntoID) ||
				(s.FromID == sugs[0].IntoID && s.IntoID == sugs[0].FromID),
			"a rejected pair must not reappear")
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

	// face_person rows should decrease
	var bound int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM face_person WHERE person_id=?`, personID).Scan(&bound))
	require.Equal(t, 1, bound)

	// The removed faces should have excluded=1
	var excl int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM face_detections WHERE excluded=1`).Scan(&excl))
	require.Equal(t, 2, excl)

	// Re-run clustering; excluded faces must not be clustered back into this person
	require.NoError(t, service.NewFaceService(db).RunClustering(context.Background()))
	list2, _ := ps.ListPersons()
	for _, p := range list2 {
		require.LessOrEqual(t, p.Count, 1, "excluded faces must not be re-clustered into any person")
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
	require.Equal(t, 0, n, "an automatic person should be deleted once its last face is removed")
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

	// Give this person a name to make it anchored
	_, err := db.Exec(`UPDATE persons SET name='Alice' WHERE id=?`, personID)
	require.NoError(t, err)

	ps := service.NewPersonService(db)
	_, err = ps.DetachAssetsFromPerson(personID, []string{"dk-a1"})
	require.NoError(t, err)

	// An anchored person should be kept, but cover/centroid should be cleared
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

	// into should now have 2 faces, and confidence should have been recomputed.
	// Before merging, each person had 1 face, so a single-face confidence=1.0.
	// After merging, the two orthogonal vectors' centroid cosine similarity ≈ 0.707, which must be < 1.0.
	// If it wasn't recomputed, confidence would still be the old 1.0, failing the assertion.
	var cnt int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM face_person WHERE person_id=?`, into).Scan(&cnt))
	require.Equal(t, 2, cnt)
	var conf float64
	require.NoError(t, db.QueryRow(`SELECT confidence FROM persons WHERE id=?`, into).Scan(&conf))
	require.Greater(t, conf, 0.0)
	require.Less(t, conf, 1.0, "confidence should have been recomputed (should be <1.0 after merging two orthogonal faces); if it's still 1.0, it wasn't recomputed")
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
	require.Equal(t, 1, p.Count, "a face on an offline asset must not count toward the person's appearance count")

	list, err := ps.ListPersons()
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, 1, list[0].Count, "ListPersons's count likewise must not count offline assets")
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
	require.Empty(t, p.CoverAssetID, "an offline cover should collapse to empty (no hero to fall back to)")

	list, err := ps.ListPersons()
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Empty(t, list[0].CoverAssetID, "ListPersons should likewise fall back to empty")

	// Now set a hero on the still-online asset — cover must fall back to it.
	heroVal := "cov-a2"
	require.NoError(t, ps.UpdatePerson(personID, service.PersonPatch{HeroAssetID: &heroVal}))

	p, err = ps.GetPerson(personID)
	require.NoError(t, err)
	require.Equal(t, "cov-a2", p.CoverAssetID, "an offline cover should fall back to a valid hero")

	list, err = ps.ListPersons()
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, "cov-a2", list[0].CoverAssetID, "ListPersons should likewise fall back to hero")
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
// Cover hybrid score + hero fallback
// ---------------------------------------------------------------------------

// setAssetAesthetic updates the given asset's aesthetic_score (nil means set to NULL).
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

// setAssetExifSize writes/updates an asset's EXIF width/height (used for the hybrid score's face-area-ratio calculation).
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

// TestPersonCoverHybridSelection verifies that cover selection changed from
// "nearest centroid" to "highest hybrid score":
// hybrid = clamp01((score-1)/9) × min(1, faceArea/imageArea).
// Three faces belong to the same person (f1/f2/f3 embeddings are close
// enough to cluster together):
//
//	f1: asset score 9.0, face ratio 0.01 → hybrid = 0.8889*0.01 ≈ 0.0089
//	f2: asset score 7.0, face ratio 0.30 → hybrid = 0.6667*0.30 = 0.20  ← should be selected
//	f3: asset score NULL (no score)      → incomparable (-1)
//
// After setting f2's asset score to NULL and recomputing: f2/f3 are both
// incomparable, leaving only f1 comparable → f1 should be selected.
// After also setting f1's asset score to NULL (all NULL) → falls back to
// nearest centroid (the original behavior); the three embeddings are
// deliberately constructed so f1 exactly coincides with the centroid
// direction, distance 0, verifying that f1 is selected after the fallback.
func TestPersonCoverHybridSelection(t *testing.T) {
	db := makeTestFaceDB(t)
	dim := 512

	// f1: unit vector along dim0.
	v1 := make([]float32, dim)
	v1[0] = 1.0
	// f2: dim0=1, dim1=0.3; after normalization it's close to f1 but not coincident.
	v2raw := make([]float32, dim)
	v2raw[0] = 1.0
	v2raw[1] = 0.3
	// f3: dim0=1, dim1=-0.3; after normalization it's symmetric to f2.
	v3raw := make([]float32, dim)
	v3raw[0] = 1.0
	v3raw[1] = -0.3

	f1 := insertAssetFace(t, db, "hy-a1", normalize(v1))
	f2 := insertAssetFace(t, db, "hy-a2", normalize(v2raw))
	f3 := insertAssetFace(t, db, "hy-a3", normalize(v3raw))
	require.NoError(t, service.NewFaceService(db).RunClustering(context.Background()))
	personID := mustFirstPersonID(t, db)

	// All three assets use a uniform 1000x1000 frame.
	for _, aid := range []string{"hy-a1", "hy-a2", "hy-a3"} {
		setAssetExifSize(t, db, aid, 1000, 1000)
	}
	// f1: face ratio 0.01 (100x100=10000/1e6), score 9.0.
	setFaceBBox(t, db, f1, `{"x1":0,"y1":0,"x2":100,"y2":100}`)
	setAssetAesthetic(t, db, "hy-a1", f64(9.0))
	// f2: face ratio 0.30 (approx 547.72^2=300000/1e6), score 7.0.
	setFaceBBox(t, db, f2, `{"x1":0,"y1":0,"x2":547.7226,"y2":547.7226}`)
	setAssetAesthetic(t, db, "hy-a2", f64(7.0))
	// f3: face ratio 0.50, but asset score NULL → incomparable.
	setFaceBBox(t, db, f3, `{"x1":0,"y1":0,"x2":707.1068,"y2":707.1068}`)
	setAssetAesthetic(t, db, "hy-a3", nil)

	ps := service.NewPersonService(db)

	// UnlockPersonCover triggers recomputeOneCentroidTx; the person isn't locked at this point, so it's unaffected.
	newFace, err := ps.UnlockPersonCover(personID)
	require.NoError(t, err)
	require.Equal(t, f2, newFace, "f2, with the highest hybrid score, should be selected as the cover")

	var coverAsset string
	require.NoError(t, db.QueryRow(`SELECT cover_asset_id FROM persons WHERE id=?`, personID).Scan(&coverAsset))
	require.Equal(t, "hy-a2", coverAsset)

	// Set f2's score to NULL: f2/f3 are both incomparable, leaving only f1 comparable → f1 should be selected.
	setAssetAesthetic(t, db, "hy-a2", nil)
	newFace, err = ps.UnlockPersonCover(personID)
	require.NoError(t, err)
	require.Equal(t, f1, newFace, "once f2 is incomparable it should fall back to the only comparable face, f1")

	// All NULL: falls back to nearest centroid. The centroid direction exactly coincides with f1 (cosDist=0), so f1 should be selected.
	setAssetAesthetic(t, db, "hy-a1", nil)
	newFace, err = ps.UnlockPersonCover(personID)
	require.NoError(t, err)
	require.Equal(t, f1, newFace, "when all faces are incomparable it should fall back to nearest centroid, and here the centroid coincides with f1's direction")
}

// TestPersonCoverLockedUnaffected verifies that when cover_locked=1 and the
// locked face is still valid, hybrid-score selection must not overwrite the
// cover — even if another face has a clearly higher hybrid score.
func TestPersonCoverLockedUnaffected(t *testing.T) {
	db := makeTestFaceDB(t)
	dim := 512
	v := make([]float32, dim)
	v[0] = 1.0
	f1 := insertAssetFace(t, db, "lkh-a1", normalize(v))
	f2 := insertAssetFace(t, db, "lkh-a2", normalize(v))
	f3 := insertAssetFace(t, db, "lkh-a3", normalize(v)) // used to trigger a recompute via detach unrelated to the lock
	require.NoError(t, service.NewFaceService(db).RunClustering(context.Background()))
	personID := mustFirstPersonID(t, db)

	for _, aid := range []string{"lkh-a1", "lkh-a2", "lkh-a3"} {
		setAssetExifSize(t, db, aid, 1000, 1000)
	}
	// f1 (to be locked) has a very low hybrid score: small face + low score.
	setFaceBBox(t, db, f1, `{"x1":0,"y1":0,"x2":50,"y2":50}`)
	setAssetAesthetic(t, db, "lkh-a1", f64(2.0))
	// f2's hybrid score is far higher than f1's: large face + perfect score.
	setFaceBBox(t, db, f2, `{"x1":0,"y1":0,"x2":900,"y2":900}`)
	setAssetAesthetic(t, db, "lkh-a2", f64(10.0))
	_ = f3

	ps := service.NewPersonService(db)
	// Lock the cover to the asset containing f1.
	lockedFaceID, err := ps.SetPersonCover(personID, "lkh-a1")
	require.NoError(t, err)
	require.Equal(t, f1, lockedFaceID)

	// Trigger a recompute unrelated to the lock (detach an unrelated asset); the locked cover must not be overwritten.
	_, err = ps.DetachAssetsFromPerson(personID, []string{"lkh-a3"})
	require.NoError(t, err)

	var coverAsset, coverFace string
	var locked int
	require.NoError(t, db.QueryRow(
		`SELECT COALESCE(cover_asset_id,''), COALESCE(cover_face_id,''), cover_locked FROM persons WHERE id=?`, personID,
	).Scan(&coverAsset, &coverFace, &locked))
	require.Equal(t, "lkh-a1", coverAsset, "the locked cover must not be overwritten by f2's higher hybrid score")
	require.Equal(t, f1, coverFace)
	require.Equal(t, 1, locked)
}

// TestPersonHeroAestheticFallback verifies that when hero hasn't been
// manually set, it falls back to the highest-scoring photo of this person;
// once manually set, it stays unchanged (even if another photo scores
// higher).
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

	// hero not set: should fall back to hf-a2, which has the highest score.
	p, err := ps.GetPerson(personID)
	require.NoError(t, err)
	require.Equal(t, "hf-a2", p.HeroAssetID, "when hero isn't set it should fall back to the highest-scoring photo")

	list, err := ps.ListPersons()
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, "hf-a2", list[0].HeroAssetID, "ListPersons's hero fallback should likewise take effect")

	// Manually set hero to the lower-scoring hf-a1: it should stay unchanged, not be overwritten by the fallback.
	heroVal := "hf-a1"
	require.NoError(t, ps.UpdatePerson(personID, service.PersonPatch{HeroAssetID: &heroVal}))

	p2, err := ps.GetPerson(personID)
	require.NoError(t, err)
	require.Equal(t, "hf-a1", p2.HeroAssetID, "a manually set hero must not be overwritten by the aesthetic fallback")

	list2, err := ps.ListPersons()
	require.NoError(t, err)
	require.Len(t, list2, 1)
	require.Equal(t, "hf-a1", list2[0].HeroAssetID)
}

// TestRecomputeOneCentroid_AllFacesIncomparable_FallbackPicksValidFace is a
// regression test: when the main cover-selection path (hybrid score) is
// entirely incomparable, it falls back to the nearest-centroid fallback
// loop. Constructs two completely opposite unit-vector faces — so the
// centroid degenerates to the zero vector, and cosDist(v, centroid) always
// returns 1.0 for the zero vector — verifying that the fallback loop still
// lands on a valid face index and doesn't panic (before the fix, best
// defaulted to -1, and in the theoretical edge case where the loop body
// never updates best even once, it would access faceIDs/assetIDs
// out-of-bounds at -1).
func TestRecomputeOneCentroid_AllFacesIncomparable_FallbackPicksValidFace(t *testing.T) {
	db := makeTestFaceDB(t)
	dim := 512

	// f1: unit vector along dim0.
	v1 := make([]float32, dim)
	v1[0] = 1.0
	// f2: unit vector completely opposite to f1; their centroid degenerates to the zero vector.
	v2 := make([]float32, dim)
	v2[0] = -1.0

	f1 := insertAssetFace(t, db, "rc-a1", v1)
	f2 := insertAssetFace(t, db, "rc-a2", v2)

	// Manually build the person + membership, skipping RunClustering: the two
	// faces have cosDist=2.0, far beyond the DBSCAN epsilon, so they would
	// naturally never cluster into the same person — here we build the "all
	// incomparable under one person" scenario directly via SQL.
	personID := "rc-person"
	_, err := db.Exec(`INSERT INTO persons(id, name) VALUES(?, '')`, personID)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO face_person(face_id, person_id) VALUES(?, ?), (?, ?)`,
		f1, personID, f2, personID)
	require.NoError(t, err)

	// Both faces have no aesthetic score/EXIF size → hybridCoverScore returns
	// -1 for both, all incomparable, triggering the centroid fallback loop.
	ps := service.NewPersonService(db)
	newFace, err := ps.UnlockPersonCover(personID)
	require.NoError(t, err, "an all-incomparable scenario must not panic or error")
	require.Contains(t, []string{f1, f2}, newFace, "should land on one of the member faces, not an out-of-bounds index")

	var coverAsset string
	require.NoError(t, db.QueryRow(`SELECT cover_asset_id FROM persons WHERE id=?`, personID).Scan(&coverAsset))
	require.Contains(t, []string{"rc-a1", "rc-a2"}, coverAsset)
}
