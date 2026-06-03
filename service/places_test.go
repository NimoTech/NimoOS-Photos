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
