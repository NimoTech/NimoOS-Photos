package service_test

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/common"
	"github.com/NimoTech/NimoOS-Photos/pkg/geo"
	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/stretchr/testify/require"
)

type mockTextML struct{}

func (m *mockTextML) CLIPTextEmbed(_ string) ([]float32, error) {
	v := make([]float32, common.CLIPDim)
	v[0] = 1.0
	return v, nil
}

func openSearchDB(t *testing.T) *service.SearchService {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "search.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	return service.NewSearchService(db, &mockTextML{})
}

func TestSmartSearch(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "s.db"))
	require.NoError(t, err)
	defer db.Close()

	db.Exec(`INSERT INTO assets(id, file_path, status, is_live_photo_video) VALUES('a1','/p/beach.jpg','indexed',0)`)
	db.Exec(`INSERT INTO asset_clip_idx(asset_id) VALUES('a1')`)
	var rowid int64
	db.QueryRow(`SELECT rowid FROM asset_clip_idx WHERE asset_id='a1'`).Scan(&rowid)

	vec := make([]float32, common.CLIPDim)
	vec[0] = 1.0
	db.Exec(`INSERT INTO clip_embeddings(rowid, embedding) VALUES(?,?)`, rowid, sqlite.SerializeFloat32(vec))

	svc := service.NewSearchService(db, &mockTextML{})
	results, err := svc.SmartSearch("beach", 10, service.SearchFilters{})
	require.NoError(t, err)
	require.NotEmpty(t, results)
	require.Equal(t, "a1", results[0].ID)
}

// TestSmartSearchIncludeOCR 验证 IncludeOCR 开启时 OCR 子串命中以 1.0 分置顶、
// 与 CLIP 结果按 ID 去重，且默认（关闭）行为不变。
func TestSmartSearchIncludeOCR(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "ocr.db"))
	require.NoError(t, err)
	defer db.Close()

	// a1: 只有 CLIP 向量；a2: 只有 OCR 文本；a3: 两者都有（去重场景）
	for i, id := range []string{"a1", "a2", "a3"} {
		db.Exec(`INSERT INTO assets(id,file_path,status,is_live_photo_video,taken_at)
			VALUES(?,?,'indexed',0,?)`, id, "/p/"+id+".jpg", fmt.Sprintf("2025-07-%02d 10:00:00", i+1))
	}
	for _, id := range []string{"a1", "a3"} {
		db.Exec(`INSERT INTO asset_clip_idx(asset_id) VALUES(?)`, id)
		var rowid int64
		db.QueryRow(`SELECT rowid FROM asset_clip_idx WHERE asset_id=?`, id).Scan(&rowid)
		vec := make([]float32, common.CLIPDim)
		vec[0] = 1.0
		db.Exec(`INSERT INTO clip_embeddings(rowid,embedding) VALUES(?,?)`, rowid, sqlite.SerializeFloat32(vec))
	}
	db.Exec(`INSERT INTO asset_ocr(asset_id,text) VALUES('a2','SUPERMART Receipt TOTAL $42.00')`)
	db.Exec(`INSERT INTO asset_ocr(asset_id,text) VALUES('a3','Invoice receipt 2024')`)

	svc := service.NewSearchService(db, &mockTextML{})

	// 默认关闭：纯 CLIP，OCR-only 的 a2 不出现
	results, err := svc.SmartSearch("receipt", 10, service.SearchFilters{})
	require.NoError(t, err)
	for _, a := range results {
		require.NotEqual(t, "a2", a.ID, "IncludeOCR=false 时不应返回 OCR-only 命中")
	}

	// 开启：a2、a3 以 1.0 分置顶（OCR 组内按拍摄时间倒序 → a3 在前），a1 仍在
	results, err = svc.SmartSearch("receipt", 10, service.SearchFilters{IncludeOCR: true})
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(results), 3)
	require.Equal(t, "a3", results[0].ID)
	require.Equal(t, "a2", results[1].ID)
	require.NotNil(t, results[0].MatchScore)
	require.InDelta(t, 1.0, *results[0].MatchScore, 1e-9)
	// OCR 命中带 matchedBy 标记（双路命中时保留 OCR 版本）；纯 CLIP 命中不带
	require.Equal(t, "ocr", results[0].MatchedBy)
	require.Equal(t, "ocr", results[1].MatchedBy)
	for _, a := range results {
		if a.ID == "a1" {
			require.Empty(t, a.MatchedBy)
		}
	}
	ids := map[string]int{}
	for _, a := range results {
		ids[a.ID]++
	}
	require.Equal(t, 1, ids["a3"], "双路命中必须去重")
	require.Equal(t, 1, ids["a1"])

	// 大小写不敏感
	results, err = svc.SmartSearch("RECEIPT", 10, service.SearchFilters{IncludeOCR: true})
	require.NoError(t, err)
	require.Equal(t, "a3", results[0].ID)
}

func TestTimeline(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "tl.db"))
	require.NoError(t, err)
	defer db.Close()

	db.Exec(`INSERT INTO assets(id,file_path,status,taken_at,is_live_photo_video) VALUES('a1','/p1.jpg','indexed','2025-07-15 10:00:00',0)`)
	db.Exec(`INSERT INTO assets(id,file_path,status,taken_at,is_live_photo_video) VALUES('v1','/p1.mov','indexed','2025-07-15 10:00:00',1)`)

	svc := service.NewSearchService(db, nil)
	groups, err := svc.Timeline("default")
	require.NoError(t, err)
	require.Len(t, groups, 1)
	require.Equal(t, 2025, groups[0].Year)
	require.Equal(t, 7, groups[0].Month)
	require.Len(t, groups[0].Assets, 1, "live photo video must be hidden")
}

// TestTimelineEnrichesPlaceName verifies Timeline/ListAssets surface a city-level
// PlaceName ("City, Country") from asset_geo, so the client filters by city rather
// than falling back to a coordinate-derived country.
func TestTimelineEnrichesPlaceName(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "place.db"))
	require.NoError(t, err)
	defer db.Close()

	db.Exec(`INSERT INTO assets(id,file_path,status,taken_at,is_live_photo_video) VALUES('a1','/p1.jpg','indexed','2025-07-15 10:00:00',0)`)
	db.Exec(`INSERT INTO assets(id,file_path,status,taken_at,is_live_photo_video) VALUES('a2','/p2.jpg','indexed','2025-07-15 11:00:00',0)`)
	// a1 is geocoded to Tokyo, Japan; a2 has no geo row → PlaceName stays empty.
	db.Exec(`INSERT INTO asset_geo(asset_id,city_id,city,country,region,lat,lon) VALUES('a1',1850147,'Tokyo','Japan','asia',35.68,139.69)`)

	svc := service.NewSearchService(db, nil)
	groups, err := svc.Timeline("default")
	require.NoError(t, err)
	got := map[string]string{}
	for _, g := range groups {
		for _, a := range g.Assets {
			got[a.ID] = a.PlaceName
		}
	}
	require.Equal(t, "Tokyo, Japan", got["a1"])
	require.Equal(t, "", got["a2"])

	assets, err := svc.ListAssets("default", 10, 0)
	require.NoError(t, err)
	for _, a := range assets {
		if a.ID == "a1" {
			require.Equal(t, "Tokyo, Japan", a.PlaceName)
		}
	}
}

func TestListAssets(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "la.db"))
	require.NoError(t, err)
	defer db.Close()

	for i := 0; i < 3; i++ {
		db.Exec(fmt.Sprintf(`INSERT INTO assets(id,file_path,status,is_live_photo_video) VALUES('id%d','/p%d.jpg','indexed',0)`, i, i))
	}

	svc := service.NewSearchService(db, nil)
	assets, err := svc.ListAssets("default", 10, 0)
	require.NoError(t, err)
	require.Len(t, assets, 3)
}

func TestGetAssetReturnsImageMetadata(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "img.db"))
	require.NoError(t, err)
	defer db.Close()

	_, err = db.Exec(`INSERT INTO assets(id, file_path, status, mime_type) VALUES('img1','/tmp/x.jpg','indexed','image/jpeg')`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO asset_exif(asset_id, width, height, iso, aperture, make, focal_length, orientation)
		VALUES('img1', 4000, 3000, 800, 1.8, 'Apple', 35.0, 1)`)
	require.NoError(t, err)

	svc := service.NewSearchService(db, nil)
	a, err := svc.GetAsset("default", "img1")
	require.NoError(t, err)
	require.Equal(t, 4000, a.Width)
	require.Equal(t, 3000, a.Height)
	require.Equal(t, 800, a.ISO)
	require.InDelta(t, 1.8, a.Aperture, 1e-6)
	require.Equal(t, "Apple", a.Make)
	require.InDelta(t, 35.0, a.FocalLength, 1e-6)
	require.Equal(t, 1, a.Orientation)
}

func TestGetAssetReturnsVideoMetadata(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "vid.db"))
	require.NoError(t, err)
	defer db.Close()

	_, err = db.Exec(`INSERT INTO assets(id, file_path, status, mime_type) VALUES('vid1','/tmp/x.mp4','indexed','video/mp4')`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO asset_exif(asset_id, width, height, video_codec, audio_codec, frame_rate, bit_rate, rotation, latitude, longitude)
		VALUES('vid1', 1920, 1080, 'h264', 'aac', 29.97, 12000000, 90, 39.9, 116.4)`)
	require.NoError(t, err)

	svc := service.NewSearchService(db, nil)
	a, err := svc.GetAsset("default", "vid1")
	require.NoError(t, err)
	require.Equal(t, 1920, a.Width)
	require.Equal(t, 1080, a.Height)
	require.Equal(t, "h264", a.VideoCodec)
	require.Equal(t, "aac", a.AudioCodec)
	require.InDelta(t, 29.97, a.FrameRate, 1e-3)
	require.Equal(t, int64(12000000), a.BitRate)
	require.Equal(t, 90, a.Rotation)
	require.InDelta(t, 39.9, a.Latitude, 1e-6)
	require.InDelta(t, 116.4, a.Longitude, 1e-6)
}

func TestGetAssetWithoutExifRow(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "bare.db"))
	require.NoError(t, err)
	defer db.Close()

	_, err = db.Exec(`INSERT INTO assets(id, file_path, status) VALUES('bare','/tmp/y.jpg','indexed')`)
	require.NoError(t, err)

	svc := service.NewSearchService(db, nil)
	a, err := svc.GetAsset("default", "bare")
	require.NoError(t, err)
	require.Equal(t, "bare", a.ID)
	require.Equal(t, 0, a.Width)
}

// TestListAssetsByPlaceKey 验证 place_key 过滤只返回该城市的照片。
// TestListAssetsBySpotKey 验证 spot_key 精确过滤到网格单元内的照片。
func TestListAssetsByPlaceAndSpotKey(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "geo_filter.db"))
	require.NoError(t, err)
	defer db.Close()

	gaz, err := geo.Load()
	require.NoError(t, err)
	geoSvc := service.NewGeoService(db, gaz)

	// seed 辅助：插入 asset + exif，并反向地理编码写入 asset_geo
	seed := func(id string, lat, lon float64) {
		_, err := db.Exec(
			`INSERT INTO assets(id,file_path,status,taken_at,is_live_photo_video)
			 VALUES(?,?,'indexed','2026-01-01 00:00:00',0)`,
			id, "/x/"+id,
		)
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO asset_exif(asset_id,latitude,longitude) VALUES(?,?,?)`, id, lat, lon)
		require.NoError(t, err)
		require.NoError(t, geoSvc.GeocodeAsset(id))
	}

	// 东京 2 张：同网格坐标 (35.6895, 139.6917)
	seed("tok1", 35.6895, 139.6917)
	seed("tok2", 35.6895, 139.6917)
	// 纽约 1 张
	seed("nyc1", 40.71, -74.00)

	// 取东京的 city_id（通过 PlacesService.ListPlaces 查 City=="Tokyo"）
	placesSvc := service.NewPlacesService(db, gaz, geoSvc)
	resp, err := placesSvc.ListPlaces()
	require.NoError(t, err)

	var tokyoCityID int32
	for _, p := range resp.Places {
		if p.City == "Tokyo" {
			tokyoCityID = p.Key
			break
		}
	}
	require.NotZero(t, tokyoCityID, "Tokyo must appear in ListPlaces")

	searchSvc := service.NewSearchService(db, nil)

	// ── 测试 place_key 过滤 ─────────────────────────────────
	assets, err := searchSvc.ListAssets("default", 50, 0,
		service.AssetFilter{PlaceKey: tokyoCityID})
	require.NoError(t, err)
	require.Len(t, assets, 2, "place_key filter must return only Tokyo photos")
	for _, a := range assets {
		require.Contains(t, []string{"tok1", "tok2"}, a.ID)
	}

	// ── 测试 spot_key 过滤 ──────────────────────────────────
	// spot_key 格式：cityID:int(lat/0.01):int(lon/0.01)
	tokLat, tokLon := 35.6895, 139.6917
	gx := int(tokLat / 0.01) // 3568
	gy := int(tokLon / 0.01) // 13969
	spotKey := fmt.Sprintf("%d:%d:%d", tokyoCityID, gx, gy)

	assets, err = searchSvc.ListAssets("default", 50, 0,
		service.AssetFilter{SpotKey: spotKey})
	require.NoError(t, err)
	require.Len(t, assets, 2, "spot_key filter must return only the 2 Tokyo photos in that grid cell")
	for _, a := range assets {
		require.Contains(t, []string{"tok1", "tok2"}, a.ID)
	}

	// ── 纽约 place_key 过滤 ─────────────────────────────────
	var nycCityID int32
	for _, p := range resp.Places {
		if p.City != "Tokyo" {
			nycCityID = p.Key
			break
		}
	}
	require.NotZero(t, nycCityID)
	assets, err = searchSvc.ListAssets("default", 50, 0,
		service.AssetFilter{PlaceKey: nycCityID})
	require.NoError(t, err)
	require.Len(t, assets, 1)
	require.Equal(t, "nyc1", assets[0].ID)
}
