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
