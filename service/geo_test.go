package service_test

import (
	"path/filepath"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/pkg/geo"
	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/stretchr/testify/require"
)

func TestGeocodeAssetWritesRow(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "t.db")
	db, err := sqlite.Open(dbPath)
	require.NoError(t, err)
	defer db.Close()

	_, err = db.Exec(`INSERT INTO assets(id, file_path, status) VALUES('a1','/x/a.jpg','indexed')`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO asset_exif(asset_id, latitude, longitude) VALUES('a1', 35.68, 139.76)`)
	require.NoError(t, err)

	gaz, err := geo.Load()
	require.NoError(t, err)
	svc := service.NewGeoService(db, gaz)

	require.NoError(t, svc.GeocodeAsset("a1"))

	var city, country, region string
	var cityID int
	require.NoError(t, db.QueryRow(
		`SELECT city_id, city, country, region FROM asset_geo WHERE asset_id='a1'`).
		Scan(&cityID, &city, &country, &region))
	require.Equal(t, "Japan", country)
	require.Equal(t, "asia", region)
	require.NotZero(t, cityID)
}

func TestBackfillPending(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "t.db"))
	require.NoError(t, err)
	defer db.Close()

	for _, a := range []struct {
		id       string
		lat, lon float64
	}{{"a1", 35.68, 139.76}, {"a2", 40.71, -74.0}} {
		_, err = db.Exec(`INSERT INTO assets(id, file_path, status) VALUES(?,?, 'indexed')`, a.id, "/x/"+a.id)
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO asset_exif(asset_id, latitude, longitude) VALUES(?,?,?)`, a.id, a.lat, a.lon)
		require.NoError(t, err)
	}

	gaz, _ := geo.Load()
	svc := service.NewGeoService(db, gaz)

	n, err := svc.BackfillPending(10)
	require.NoError(t, err)
	require.Equal(t, 2, n)

	n, err = svc.BackfillPending(10)
	require.NoError(t, err)
	require.Equal(t, 0, n)
}
