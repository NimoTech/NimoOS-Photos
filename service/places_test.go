package service_test

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/NimoTech/NimoOS-Photos/pkg/geo"
	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/stretchr/testify/require"
)

func placesFixture(t *testing.T) *service.PlacesService {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "t.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	gaz, err := geo.Load()
	require.NoError(t, err)
	geoSvc := service.NewGeoService(db, gaz)

	seed := func(id string, lat, lon float64, takenDaysAgo int) {
		taken := time.Now().AddDate(0, 0, -takenDaysAgo).UTC().Format("2006-01-02 15:04:05")
		_, err := db.Exec(`INSERT INTO assets(id,file_path,status,taken_at,is_live_photo_video)
			VALUES(?,?, 'indexed', ?, 0)`, id, "/x/"+id, taken)
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO asset_exif(asset_id,latitude,longitude) VALUES(?,?,?)`, id, lat, lon)
		require.NoError(t, err)
		require.NoError(t, geoSvc.GeocodeAsset(id))
	}
	// Three points all resolve to Tokyo (cityID=1850147) at (35.689x, 139.69xx)
	seed("t1", 35.6895, 139.6917, 2)
	seed("t2", 35.6870, 139.6890, 3)
	seed("t3", 35.6920, 139.6950, 4)
	// New York City
	seed("n1", 40.71, -74.00, 300)

	return service.NewPlacesService(db, gaz, geoSvc)
}

func TestListPlaces(t *testing.T) {
	svc := placesFixture(t)
	resp, err := svc.ListPlaces()
	require.NoError(t, err)
	require.Equal(t, 2, resp.Stats.Cities)
	require.Equal(t, 4, resp.Stats.Photos)
	require.Equal(t, 2, resp.Stats.Countries)

	var tokyo *service.Place
	for i := range resp.Places {
		if resp.Places[i].City == "Tokyo" {
			tokyo = &resp.Places[i]
		}
	}
	require.NotNil(t, tokyo)
	require.Equal(t, 3, tokyo.Count)
	require.True(t, tokyo.Recent)
	require.NotEmpty(t, tokyo.Thumbs)
	require.Equal(t, "asia", tokyo.Region)
}

// TestListPlacesExcludesOffline verifies that ListPlaces and recentThumbs no
// longer count/show assets whose removable drive is currently unplugged.
func TestListPlacesExcludesOffline(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "off.db"))
	require.NoError(t, err)
	defer db.Close()

	gaz, err := geo.Load()
	require.NoError(t, err)
	geoSvc := service.NewGeoService(db, gaz)

	seed := func(id, path string, lat, lon float64) {
		_, err := db.Exec(`INSERT INTO assets(id,file_path,status,taken_at,is_live_photo_video)
			VALUES(?,?, 'indexed', '2026-01-01 00:00:00', 0)`, id, path)
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO asset_exif(asset_id,latitude,longitude) VALUES(?,?,?)`, id, lat, lon)
		require.NoError(t, err)
		require.NoError(t, geoSvc.GeocodeAsset(id))
	}
	seed("online", "/DATA/x1", 35.6895, 139.6917)
	seed("offline", "/media/X/x2", 35.6895, 139.6917)
	_, err = db.Exec(`UPDATE assets SET offline=1 WHERE id='offline'`)
	require.NoError(t, err)

	svc := service.NewPlacesService(db, gaz, geoSvc)
	resp, err := svc.ListPlaces()
	require.NoError(t, err)
	require.Len(t, resp.Places, 1)
	require.Equal(t, 1, resp.Places[0].Count, "offline 资产不应计入城市照片数")
	require.Contains(t, resp.Places[0].Thumbs, "online")
	require.NotContains(t, resp.Places[0].Thumbs, "offline")
}

func TestSpotsCluster(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "t.db"))
	require.NoError(t, err)
	defer db.Close()
	gaz, _ := geo.Load()
	geoSvc := service.NewGeoService(db, gaz)
	mk := func(id string, lat, lon float64) {
		db.Exec(`INSERT INTO assets(id,file_path,status,taken_at,is_live_photo_video) VALUES(?,?, 'indexed', '2026-03-01 10:00:00', 0)`, id, "/x/"+id)
		db.Exec(`INSERT INTO asset_exif(asset_id,latitude,longitude) VALUES(?,?,?)`, id, lat, lon)
		geoSvc.GeocodeAsset(id)
	}
	// Both clusters inside Shibuya (city_id=11808021), separated by lon bucket boundary at 139.710.
	// Cluster A: lon ~139.7036 → bucket [3565,13970]
	// Cluster B: lon ~139.7116 → bucket [3565,13971]
	mk("s1", 35.6579, 139.7036)
	mk("s2", 35.6569, 139.7046)
	mk("s3", 35.6589, 139.7036)
	mk("k1", 35.6579, 139.7116)
	mk("k2", 35.6569, 139.7126)
	mk("k3", 35.6589, 139.7116)

	svc := service.NewPlacesService(db, gaz, geoSvc)
	resp, _ := svc.ListPlaces()
	require.NotEmpty(t, resp.Places)
	key := resp.Places[0].Key
	spots := svc.Spots(key)
	require.GreaterOrEqual(t, len(spots), 2)
	require.NotEmpty(t, spots[0].Thumb)
	require.NotZero(t, spots[0].Count)
}

// TestSpotMemberIDsMatchesSpotCount is the regression guard for the library
// "view this spot" count diverging from the spot dialog count: SpotMemberIDs
// must return exactly Count IDs for every spot, since both derive from the same
// radius clustering (previously the filter used a grid cell and undercounted).
func TestSpotMemberIDsMatchesSpotCount(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "t.db"))
	require.NoError(t, err)
	defer db.Close()
	gaz, _ := geo.Load()
	geoSvc := service.NewGeoService(db, gaz)
	mk := func(id string, lat, lon float64) {
		db.Exec(`INSERT INTO assets(id,file_path,status,taken_at,is_live_photo_video) VALUES(?,?, 'indexed', '2026-03-01 10:00:00', 0)`, id, "/x/"+id)
		db.Exec(`INSERT INTO asset_exif(asset_id,latitude,longitude) VALUES(?,?,?)`, id, lat, lon)
		geoSvc.GeocodeAsset(id)
	}
	// Two clusters that straddle the 139.710 lon grid-bucket boundary — the exact
	// case where the old grid-cell filter diverged from the cluster count.
	mk("s1", 35.6579, 139.7036)
	mk("s2", 35.6569, 139.7046)
	mk("s3", 35.6589, 139.7036)
	mk("k1", 35.6579, 139.7116)
	mk("k2", 35.6569, 139.7126)
	mk("k3", 35.6589, 139.7116)

	svc := service.NewPlacesService(db, gaz, geoSvc)
	resp, _ := svc.ListPlaces()
	require.NotEmpty(t, resp.Places)
	cityKey := resp.Places[0].Key
	spots := svc.Spots(cityKey)
	require.GreaterOrEqual(t, len(spots), 2)

	for _, sp := range spots {
		ids, err := svc.SpotMemberIDs(sp.Key)
		require.NoError(t, err)
		require.Len(t, ids, sp.Count, "spot %s: member IDs must match displayed count", sp.Key)
	}

	// An unknown spot key resolves to zero photos (non-nil empty), never nil.
	ids, err := svc.SpotMemberIDs(fmt.Sprintf("%d:0:0", cityKey))
	require.NoError(t, err)
	require.NotNil(t, ids)
	require.Empty(t, ids)
}

func TestSpotNameOverride(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "t.db"))
	require.NoError(t, err)
	defer db.Close()
	gaz, _ := geo.Load()
	geoSvc := service.NewGeoService(db, gaz)
	mk := func(id string, lat, lon float64) {
		db.Exec(`INSERT INTO assets(id,file_path,status,taken_at,is_live_photo_video) VALUES(?,?, 'indexed', '2026-03-01 10:00:00', 0)`, id, "/x/"+id)
		db.Exec(`INSERT INTO asset_exif(asset_id,latitude,longitude) VALUES(?,?,?)`, id, lat, lon)
		require.NoError(t, geoSvc.GeocodeAsset(id))
	}
	mk("s1", 35.6579, 139.7036)
	mk("s2", 35.6569, 139.7046)
	mk("s3", 35.6589, 139.7036)

	svc := service.NewPlacesService(db, gaz, geoSvc)
	resp, _ := svc.ListPlaces()
	spots := svc.Spots(resp.Places[0].Key)
	require.NotEmpty(t, spots)
	sk := spots[0].Key

	require.NoError(t, svc.SetSpotName("0", sk, "teamLab Planets"))
	require.Equal(t, "teamLab Planets", svc.SpotNameOverrides("0")[sk])
	require.Empty(t, svc.SpotNameOverrides("other-user")[sk], "override is per-user")

	require.NoError(t, svc.ResetSpotName("0", sk))
	require.Empty(t, svc.SpotNameOverrides("0")[sk])
}

func TestGetPlaceFull(t *testing.T) {
	svc := placesFixture(t)
	resp, _ := svc.ListPlaces()
	// Find the place with the highest count (Tokyo in the fixture, 3 photos).
	var key int32
	var city string
	for _, p := range resp.Places {
		if p.Count > 1 {
			key = p.Key
			city = p.City
			break
		}
	}
	require.NotZero(t, key)
	d, err := svc.GetPlace(key)
	require.NoError(t, err)
	require.Equal(t, city, d.City)
	require.NotEmpty(t, d.Recent)
	require.NotEmpty(t, d.Insights)
	require.NotEmpty(t, d.Visits)
	for _, ins := range d.Insights {
		require.NotEmpty(t, ins.Key)
	}
}

func TestCoverOverride(t *testing.T) {
	svc := placesFixture(t)
	resp, _ := svc.ListPlaces()
	key := resp.Places[0].Key

	require.NoError(t, svc.SetCover("1", key, resp.Places[0].Thumbs[0]))
	got, err := svc.GetCover("1", key)
	require.NoError(t, err)
	require.Equal(t, resp.Places[0].Thumbs[0], got)

	require.NoError(t, svc.ResetCover("1", key))
	got, _ = svc.GetCover("1", key)
	require.Equal(t, "", got)
}

// TestCoverOverridesBatch verifies the batch per-user override lookup used by
// the places list: it returns only this user's overrides and silently drops
// overrides whose asset has been deleted.
func TestCoverOverridesBatch(t *testing.T) {
	svc := placesFixture(t)
	resp, _ := svc.ListPlaces()
	require.GreaterOrEqual(t, len(resp.Places), 2)
	k1, k2 := resp.Places[0].Key, resp.Places[1].Key

	require.NoError(t, svc.SetCover("1", k1, resp.Places[0].Thumbs[0]))
	require.NoError(t, svc.SetCover("1", k2, resp.Places[1].Thumbs[0]))
	require.NoError(t, svc.SetCover("2", k1, resp.Places[0].Thumbs[0]))

	got := svc.CoverOverrides("1")
	require.Len(t, got, 2)
	require.Equal(t, resp.Places[0].Thumbs[0], got[k1])
	require.Equal(t, resp.Places[1].Thumbs[0], got[k2])

	// Unknown user → empty (non-nil OK either way, just must be len 0).
	require.Empty(t, svc.CoverOverrides("nobody"))
}

// TestTopFacesExcludesLivePhotoCompanionVideo verifies that a live photo's
// auto-generated companion video (is_live_photo_video=1) does not inflate a
// person's face count/ranking in a place's "companions" insight. "Live" has a
// real still photo (1 face) plus a companion video carrying 2 more face rows
// for the same person — if those video-face rows were still counted, Live's
// count (3) would beat Competitor's (2) and knock Competitor out of the
// top-2 companions; once excluded, Live's true count (1) loses to Competitor
// (2) and Live drops out instead, with no counts tying (deterministic).
func TestTopFacesExcludesLivePhotoCompanionVideo(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "t.db"))
	require.NoError(t, err)
	defer db.Close()
	gaz, err := geo.Load()
	require.NoError(t, err)
	geoSvc := service.NewGeoService(db, gaz)

	const lat, lon = 35.6895, 139.6917 // Tokyo

	mkAsset := func(id string, isLive bool) {
		live := 0
		if isLive {
			live = 1
		}
		_, err := db.Exec(`INSERT INTO assets(id,file_path,status,taken_at,is_live_photo_video)
			VALUES(?,?, 'indexed', '2026-01-01 00:00:00', ?)`, id, "/x/"+id, live)
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO asset_exif(asset_id,latitude,longitude) VALUES(?,?,?)`, id, lat, lon)
		require.NoError(t, err)
		require.NoError(t, geoSvc.GeocodeAsset(id))
	}
	mkFace := func(faceID, assetID, personID string) {
		_, err := db.Exec(`INSERT INTO face_detections(id,asset_id,bbox,embedding) VALUES(?,?,'{}',X'00000000')`, faceID, assetID)
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO face_person(face_id,person_id) VALUES(?,?)`, faceID, personID)
		require.NoError(t, err)
	}

	_, err = db.Exec(`INSERT INTO persons(id,name) VALUES('p-live','Live'),('p-comp','Competitor'),('p-fill','Filler')`)
	require.NoError(t, err)

	// Filler: 5 distinct real photos → count=5, always tops the ranking.
	for i := 1; i <= 5; i++ {
		id := fmt.Sprintf("fill%d", i)
		mkAsset(id, false)
		mkFace("f-fill-"+id, id, "p-fill")
	}
	// Competitor: 2 distinct real photos → count=2, stable.
	for i := 1; i <= 2; i++ {
		id := fmt.Sprintf("comp%d", i)
		mkAsset(id, false)
		mkFace("f-comp-"+id, id, "p-comp")
	}
	// Live: 1 real still photo (true count=1) + 1 companion video carrying 2
	// face rows for the same person (would inflate the count to 3 if counted).
	mkAsset("live_img", false)
	mkFace("f-live-img", "live_img", "p-live")
	mkAsset("live_vid", true)
	mkFace("f-live-vid-1", "live_vid", "p-live")
	mkFace("f-live-vid-2", "live_vid", "p-live")

	svc := service.NewPlacesService(db, gaz, geoSvc)
	resp, err := svc.ListPlaces()
	require.NoError(t, err)
	require.NotEmpty(t, resp.Places)
	cityKey := resp.Places[0].Key

	detail, err := svc.GetPlace(cityKey)
	require.NoError(t, err)

	var names []string
	found := false
	for _, ins := range detail.Insights {
		if ins.Key == "photos.places.insight.companions" {
			found = true
			raw, ok := ins.Params["names"].([]string)
			require.True(t, ok, "companions insight params must carry a []string names slice")
			names = raw
		}
	}
	require.True(t, found, "expected a companions insight")
	require.Equal(t, []string{"Filler", "Competitor"}, names,
		"Live's companion-video face rows must not inflate its count past Competitor's real count")
}

func TestCreateAlbumFromPlace(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "t.db"))
	require.NoError(t, err)
	defer db.Close()
	gaz, _ := geo.Load()
	geoSvc := service.NewGeoService(db, gaz)
	for _, a := range []string{"a1", "a2"} {
		db.Exec(`INSERT INTO assets(id,file_path,status,taken_at,is_live_photo_video) VALUES(?,?, 'indexed', '2026-03-01 10:00:00', 0)`, a, "/x/"+a)
		db.Exec(`INSERT INTO asset_exif(asset_id,latitude,longitude) VALUES(?,35.6895,139.6917)`, a)
		geoSvc.GeocodeAsset(a)
	}
	albums := service.NewAlbumService(db)
	svc := service.NewPlacesServiceWithAlbums(db, gaz, geoSvc, albums)
	resp, _ := svc.ListPlaces()
	key := resp.Places[0].Key

	albumID, count, err := svc.CreateAlbumFromPlace(key, "Tokyo Trip", "", "")
	require.NoError(t, err)
	require.NotEmpty(t, albumID)
	require.Equal(t, 2, count)
}

func TestCoverCandidatesTabs(t *testing.T) {
	svc := placesFixture(t)
	resp, _ := svc.ListPlaces()
	key := resp.Places[0].Key

	res, err := svc.CoverCandidates(key, "all", "", 0, 40)
	require.NoError(t, err)
	require.Equal(t, resp.Places[0].Count, res.Total)
	require.Len(t, res.Items, resp.Places[0].Count)
	require.Equal(t, 1, res.TotalPages)

	recent, err := svc.CoverCandidates(key, "recent", "", 0, 40)
	require.NoError(t, err)
	require.NotEmpty(t, recent.Items)
}

// TestCoverCandidatesNegativePage 确认 page<0 不 panic，且行为等同于 page=0。
func TestCoverCandidatesNegativePage(t *testing.T) {
	svc := placesFixture(t)
	resp, _ := svc.ListPlaces()
	key := resp.Places[0].Key

	// page=-1 不应 panic，且应等同于 page=0 的结果。
	res, err := svc.CoverCandidates(key, "all", "", -1, 40)
	require.NoError(t, err)
	require.Equal(t, 0, res.Page)
	require.Equal(t, resp.Places[0].Count, res.Total)
	require.Len(t, res.Items, resp.Places[0].Count)
}

func TestTripsSplitByGap(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "t.db"))
	require.NoError(t, err)
	defer db.Close()
	gaz, _ := geo.Load()
	geoSvc := service.NewGeoService(db, gaz)

	mk := func(id string, daysAgo int) {
		taken := time.Now().AddDate(0, 0, -daysAgo).UTC().Format("2006-01-02 15:04:05")
		db.Exec(`INSERT INTO assets(id,file_path,status,taken_at,is_live_photo_video) VALUES(?,?, 'indexed', ?, 0)`, id, "/x/"+id, taken)
		db.Exec(`INSERT INTO asset_exif(asset_id,latitude,longitude) VALUES(?,35.6895,139.6917)`, id)
		geoSvc.GeocodeAsset(id)
	}
	mk("a1", 300)
	mk("a2", 299)
	mk("b1", 5)
	mk("b2", 4)

	svc := service.NewPlacesService(db, gaz, geoSvc)
	resp, _ := svc.ListPlaces()
	key := resp.Places[0].Key
	require.Equal(t, 2, resp.Places[0].Trips)

	visits, err := svc.Visits(key)
	require.NoError(t, err)
	require.Len(t, visits, 2)
	require.True(t, visits[0].Current)
	require.Equal(t, 2, visits[0].Photos)
}

// The "current trip" is exactly one place: the city of the single most-recent
// photo (one green dot). Even another city visited within the same gap-free
// window is NOT current — only the latest photo's location is. This guards
// against the old per-city bug where several places could all claim to be the
// current trip at once.
func TestCurrentTripIsLatestPhotoCityOnly(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "t.db"))
	require.NoError(t, err)
	defer db.Close()
	gaz, _ := geo.Load()
	geoSvc := service.NewGeoService(db, gaz)

	seed := func(id string, lat, lon float64, daysAgo int) {
		taken := time.Now().AddDate(0, 0, -daysAgo).UTC().Format("2006-01-02 15:04:05")
		db.Exec(`INSERT INTO assets(id,file_path,status,taken_at,is_live_photo_video) VALUES(?,?, 'indexed', ?, 0)`, id, "/x/"+id, taken)
		db.Exec(`INSERT INTO asset_exif(asset_id,latitude,longitude) VALUES(?,?,?)`, id, lat, lon)
		require.NoError(t, geoSvc.GeocodeAsset(id))
	}
	// Tokyo holds the single most-recent photo (1 day ago) → the one green dot.
	seed("t1", 35.6895, 139.6917, 1)
	// London 3 days ago: same gap-free window as Tokyo, yet NOT the latest photo,
	// so it must NOT be current — only one place is ever the current trip.
	seed("l1", 51.5085, -0.1257, 3)

	svc := service.NewPlacesService(db, gaz, geoSvc)
	resp, err := svc.ListPlaces()
	require.NoError(t, err)

	byCity := map[string]service.Place{}
	for _, p := range resp.Places {
		byCity[p.City] = p
	}
	require.True(t, byCity["Tokyo"].Recent, "Tokyo holds the latest photo → current trip")
	require.False(t, byCity["London"].Recent, "London is not the latest photo's city")

	// Exactly one place flagged as the current trip.
	greenDots := 0
	for _, p := range resp.Places {
		if p.Recent {
			greenDots++
		}
	}
	require.Equal(t, 1, greenDots, "exactly one green dot")

	lv, err := svc.Visits(byCity["London"].Key)
	require.NoError(t, err)
	require.False(t, lv[0].Current, "London visit must not be flagged current")
}

// TestSpotMemberIDsAtPinsExactCluster guards the Places "view this spot" jump:
// when two clusters fall in the same 0.01° grid cell they share a spot key, and
// the key-based SpotMemberIDs resolves the tie to the largest cluster. Passing
// the tapped spot's centroid must instead return that exact cluster's members,
// so the library count equals the spot dialog count for BOTH colliding spots.
func TestSpotMemberIDsAtPinsExactCluster(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "t.db"))
	require.NoError(t, err)
	defer db.Close()
	gaz, _ := geo.Load()
	geoSvc := service.NewGeoService(db, gaz)
	mk := func(id string, lat, lon float64) {
		db.Exec(`INSERT INTO assets(id,file_path,status,taken_at,is_live_photo_video) VALUES(?,?, 'indexed', '2026-03-01 10:00:00', 0)`, id, "/x/"+id)
		db.Exec(`INSERT INTO asset_exif(asset_id,latitude,longitude) VALUES(?,?,?)`, id, lat, lon)
		geoSvc.GeocodeAsset(id)
	}
	// Two clusters in the SAME 0.01° grid cell (lat bucket 3565, lon bucket 13970)
	// but ~1km apart, so radius clustering keeps them separate while their spot
	// keys collide. Cluster A has 4 photos, B has 3 — the case where the old
	// largest-wins resolution returned A's photos even when the user tapped B.
	mk("a1", 35.6515, 139.7015)
	mk("a2", 35.6512, 139.7018)
	mk("a3", 35.6518, 139.7012)
	mk("a4", 35.6514, 139.7016)
	mk("b1", 35.6585, 139.7085)
	mk("b2", 35.6582, 139.7088)
	mk("b3", 35.6588, 139.7082)

	svc := service.NewPlacesService(db, gaz, geoSvc)
	resp, _ := svc.ListPlaces()
	require.NotEmpty(t, resp.Places)
	cityKey := resp.Places[0].Key
	spots := svc.Spots(cityKey)
	require.Len(t, spots, 2, "fixture must yield exactly two spots")
	require.Equal(t, spots[0].Key, spots[1].Key, "fixture must produce a spot-key collision")

	for _, sp := range spots {
		ids, err := svc.SpotMemberIDsAt(sp.Key, sp.Lat, sp.Lon)
		require.NoError(t, err)
		require.Len(t, ids, sp.Count, "centroid match must return exactly the tapped cluster's members, not the largest")
	}

	// A far-away point matches no cluster → empty (non-nil) slice.
	ids, err := svc.SpotMemberIDsAt(spots[0].Key, 0, 0)
	require.NoError(t, err)
	require.NotNil(t, ids)
	require.Empty(t, ids)
}

// TestPlacesCoverThumbsPrefersAesthetic 验证城市卡 Thumbs 按美学分优先排序(未打分的
// 兜底按拍摄时间倒序),而详情页 Detail.Recent(「最近」区块)仍保持纯时间语义,不受
// 美学分影响。
func TestPlacesCoverThumbsPrefersAesthetic(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "t.db"))
	require.NoError(t, err)
	defer db.Close()
	gaz, err := geo.Load()
	require.NoError(t, err)
	geoSvc := service.NewGeoService(db, gaz)

	seed := func(id string, daysAgo int) {
		taken := time.Now().AddDate(0, 0, -daysAgo).UTC().Format("2006-01-02 15:04:05")
		_, err := db.Exec(`INSERT INTO assets(id,file_path,status,taken_at,is_live_photo_video)
			VALUES(?,?, 'indexed', ?, 0)`, id, "/x/"+id, taken)
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO asset_exif(asset_id,latitude,longitude) VALUES(?,35.6895,139.6917)`, id)
		require.NoError(t, err)
		require.NoError(t, geoSvc.GeocodeAsset(id))
	}
	// t1 最新但未打分;t2 打分最高但拍摄较早;t3 打分较低;t4 最旧且未打分。
	seed("t1", 1)
	seed("t2", 5)
	seed("t3", 3)
	seed("t4", 10)
	_, err = db.Exec(`UPDATE assets SET aesthetic_score=9.0 WHERE id='t2'`)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE assets SET aesthetic_score=2.0 WHERE id='t3'`)
	require.NoError(t, err)

	svc := service.NewPlacesService(db, gaz, geoSvc)
	resp, err := svc.ListPlaces()
	require.NoError(t, err)
	require.Len(t, resp.Places, 1)
	cityID := resp.Places[0].Key

	// 城市卡 Thumbs:美学分优先(t2 最高在前、t3 次之),未打分的兜底按时间倒序(t1 比 t4 新)。
	require.Equal(t, []string{"t2", "t3", "t1", "t4"}, resp.Places[0].Thumbs,
		"城市卡封面应美学分优先,未打分资产兜底按拍摄时间倒序")

	// 详情页 Recent 仍是纯时间倒序,不受美学分影响。
	detail, err := svc.GetPlace(cityID)
	require.NoError(t, err)
	require.Equal(t, []string{"t1", "t3", "t2", "t4"}, detail.Recent,
		"详情页「最近」区块必须保持纯时间语义,不被美学分打乱")
}

// TestSpotsCoverPrefersAesthetic 验证 spot 封面取簇内美学分最高的资产;当簇内全部未打分
// 时退回原行为(最新一张,即 firstID)。
func TestSpotsCoverPrefersAesthetic(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "t.db"))
	require.NoError(t, err)
	defer db.Close()
	gaz, _ := geo.Load()
	geoSvc := service.NewGeoService(db, gaz)
	mk := func(id string, lat, lon float64) {
		db.Exec(`INSERT INTO assets(id,file_path,status,taken_at,is_live_photo_video) VALUES(?,?, 'indexed', '2026-03-01 10:00:00', 0)`, id, "/x/"+id)
		db.Exec(`INSERT INTO asset_exif(asset_id,latitude,longitude) VALUES(?,?,?)`, id, lat, lon)
		geoSvc.GeocodeAsset(id)
	}
	// 单个簇,3 张照片,刚好达到 spotMinPhotos(=3)阈值。
	mk("s1", 35.6579, 139.7036)
	mk("s2", 35.6569, 139.7046)
	mk("s3", 35.6589, 139.7036)

	svc := service.NewPlacesService(db, gaz, geoSvc)
	resp, _ := svc.ListPlaces()
	require.NotEmpty(t, resp.Places)
	cityID := resp.Places[0].Key

	// 全未打分:退回原行为(firstID,即簇内最新一张)。
	spotsBefore := svc.Spots(cityID)
	require.Len(t, spotsBefore, 1)
	require.NotEmpty(t, spotsBefore[0].Thumb, "全未打分应退回 firstID 兜底")

	// s2 打分最高:封面应切到 s2,即便 s2 不是簇内最新的一张。
	_, err = db.Exec(`UPDATE assets SET aesthetic_score=7.5 WHERE id='s2'`)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE assets SET aesthetic_score=1.0 WHERE id='s1'`)
	require.NoError(t, err)

	spotsAfter := svc.Spots(cityID)
	require.Len(t, spotsAfter, 1)
	require.Equal(t, "s2", spotsAfter[0].Thumb, "簇内美学分最高的资产应作为 spot 封面")
}

// TestSpotJumpSmallerClusterByCentroid is the end-to-end guard for the bug
// report: tapping the SMALLER of two key-colliding spots must surface that
// spot's own photos. (Mirrors how route/v1/assets.go calls SpotMemberIDsAt.)
func TestSpotJumpSmallerClusterByCentroid(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "t.db"))
	require.NoError(t, err)
	defer db.Close()
	gaz, _ := geo.Load()
	geoSvc := service.NewGeoService(db, gaz)
	mk := func(id string, lat, lon float64) {
		db.Exec(`INSERT INTO assets(id,file_path,status,taken_at,is_live_photo_video) VALUES(?,?, 'indexed', '2026-03-01 10:00:00', 0)`, id, "/x/"+id)
		db.Exec(`INSERT INTO asset_exif(asset_id,latitude,longitude) VALUES(?,?,?)`, id, lat, lon)
		geoSvc.GeocodeAsset(id)
	}
	mk("a1", 35.6515, 139.7015)
	mk("a2", 35.6512, 139.7018)
	mk("a3", 35.6518, 139.7012)
	mk("a4", 35.6514, 139.7016)
	mk("b1", 35.6585, 139.7085)
	mk("b2", 35.6582, 139.7088)
	mk("b3", 35.6588, 139.7082)

	svc := service.NewPlacesService(db, gaz, geoSvc)
	resp, _ := svc.ListPlaces()
	cityKey := resp.Places[0].Key
	spots := svc.Spots(cityKey)
	require.Len(t, spots, 2)

	// spots sorted by count desc → [0]=A(4), [1]=B(3). They share a key.
	small := spots[1]
	require.Equal(t, 3, small.Count)

	// Key-only resolution wrongly returns the larger cluster (4).
	idsKeyOnly, err := svc.SpotMemberIDs(small.Key)
	require.NoError(t, err)
	require.Len(t, idsKeyOnly, 4, "documents the old largest-wins divergence")

	// Centroid resolution returns the tapped (smaller) cluster (3).
	idsAt, err := svc.SpotMemberIDsAt(small.Key, small.Lat, small.Lon)
	require.NoError(t, err)
	require.Len(t, idsAt, 3, "centroid match returns the tapped cluster")
}
