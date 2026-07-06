package service_test

import (
	"path/filepath"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/stretchr/testify/require"
)

func TestAlbumSummary(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	svc := service.NewAlbumService(db)

	album, err := svc.Create("Untitled")
	require.NoError(t, err)

	_, err = db.Exec(`INSERT INTO assets(id, file_path, mime_type, original_name, taken_at, status, deleted_at, is_live_photo_video) VALUES
		('p1','/g/p1.heic','image/heic','IMG_0001.HEIC','2024-04-02 10:00:00','indexed',NULL,0),
		('p2','/g/p2.jpg','image/jpeg','IMG_0002.jpg','2024-04-05 11:00:00','indexed',NULL,0),
		('p3','/g/p3.jpg','image/jpeg','IMG_0003.jpg','2024-04-09 12:00:00','indexed',NULL,0),
		('v1','/g/v1.mp4','video/mp4','MOV_0001.mp4','2024-04-06 09:00:00','indexed',NULL,0),
		('tr','/g/tr.jpg','image/jpeg','TRASH.jpg','2024-04-07 09:00:00','indexed','2024-05-01 00:00:00',0),
		('lv','/g/lv.mov','video/quicktime','IMG_0001.mov','2024-04-02 10:00:00','indexed',NULL,1)`)
	require.NoError(t, err)
	for _, id := range []string{"p1", "p2", "p3", "v1", "tr", "lv"} {
		require.NoError(t, svc.AddAsset(album.ID, id))
	}
	_, err = db.Exec(`INSERT INTO asset_geo(asset_id, city, country) VALUES
		('p1','Tokyo','Japan'), ('p2','Tokyo','Japan'),
		('tr','Paris','France')`) // trashed asset's place must NOT count
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO persons(id, name) VALUES ('per1','Alice'), ('per2','')`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO face_detections(id, asset_id, bbox, embedding, excluded) VALUES
		('f1','p1','[]',x'00',0),
		('f2','p2','[]',x'00',0),
		('f3','p3','[]',x'00',1)`) // excluded face must NOT count
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO face_person(face_id, person_id) VALUES
		('f1','per1'), ('f2','per2'), ('f3','per1')`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO asset_ocr(asset_id, text) VALUES ('p2','新干线指定席 东京→大阪')`)
	require.NoError(t, err)

	sum, err := svc.Summary(album.ID)
	require.NoError(t, err)

	require.Equal(t, 4, sum.AssetCount) // p1 p2 p3 v1 (trash and live companion video excluded)
	require.Equal(t, 3, sum.PhotoCount)
	require.Equal(t, 1, sum.VideoCount)
	require.Equal(t, "2024-04-02", sum.DateStart)
	require.Equal(t, "2024-04-09", sum.DateEnd)
	require.Equal(t, []service.AlbumPlaceCount{{City: "Tokyo", Country: "Japan", Count: 2}}, sum.TopPlaces)
	require.Equal(t, []service.AlbumPersonCount{{Name: "Alice", Count: 1}}, sum.TopPersons) // unnamed per2 and excluded f3 must not appear
	require.Equal(t, []string{"新干线指定席 东京→大阪"}, sum.OCRSamples)
	require.Len(t, sum.SampleFilenames, 4)
	require.Equal(t, "IMG_0001.HEIC", sum.SampleFilenames[0])         // ascending taken_at order
	require.Equal(t, []string{"p1", "p2", "p3"}, sum.CoverCandidates) // photos only, time-spread
}

// TestAlbumSummaryExcludesOffline verifies the AI agent's album summary
// (counts, top places/persons, OCR samples, filenames, cover candidates)
// stops counting/surfacing an asset whose removable drive is unplugged
// (offline=1) — otherwise CoverCandidates could pick a broken thumbnail.
func TestAlbumSummaryExcludesOffline(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	svc := service.NewAlbumService(db)

	album, err := svc.Create("Offline")
	require.NoError(t, err)

	_, err = db.Exec(`INSERT INTO assets(id, file_path, mime_type, original_name, taken_at, status, is_live_photo_video) VALUES
		('online','/g/online.jpg','image/jpeg','ONLINE.jpg','2024-04-02 10:00:00','indexed',0),
		('offline','/media/X/offline.jpg','image/jpeg','OFFLINE.jpg','2024-04-05 11:00:00','indexed',0)`)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE assets SET offline=1 WHERE id='offline'`)
	require.NoError(t, err)
	require.NoError(t, svc.AddAsset(album.ID, "online"))
	require.NoError(t, svc.AddAsset(album.ID, "offline"))
	_, err = db.Exec(`INSERT INTO asset_geo(asset_id, city, country) VALUES ('offline','Paris','France')`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO asset_ocr(asset_id, text) VALUES ('offline','should not appear')`)
	require.NoError(t, err)

	sum, err := svc.Summary(album.ID)
	require.NoError(t, err)
	require.Equal(t, 1, sum.AssetCount, "offline 资产不应计入相册摘要总数")
	require.Equal(t, 1, sum.PhotoCount)
	require.Empty(t, sum.TopPlaces, "offline 资产的地点不应出现在摘要里")
	require.Empty(t, sum.OCRSamples, "offline 资产的 OCR 文本不应出现在摘要里")
	require.Equal(t, []string{"online"}, sum.CoverCandidates, "封面候选不应包含 offline 资产")
}

func TestAlbumSummaryEmptyAlbum(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	svc := service.NewAlbumService(db)

	album, err := svc.Create("Empty")
	require.NoError(t, err)

	sum, err := svc.Summary(album.ID)
	require.NoError(t, err)
	require.Equal(t, 0, sum.AssetCount)
	// empty slices, not nil: JSON must encode as [] not null
	require.NotNil(t, sum.TopPlaces)
	require.NotNil(t, sum.TopPersons)
	require.NotNil(t, sum.OCRSamples)
	require.NotNil(t, sum.SampleFilenames)
	require.NotNil(t, sum.CoverCandidates)
}

func TestAlbumSummaryNotFound(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	svc := service.NewAlbumService(db)

	_, err = svc.Summary("nope")
	require.ErrorIs(t, err, service.ErrNotFound)
}
