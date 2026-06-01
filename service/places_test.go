package service_test

import (
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
