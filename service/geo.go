package service

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/NimoTech/NimoOS-Photos/pkg/geo"
)

// GeoService reverse-geocodes assets and persists results into asset_geo.
type GeoService struct {
	db  *sql.DB
	gaz *geo.Gazetteer
}

// NewGeoService constructs a GeoService.
func NewGeoService(db *sql.DB, gaz *geo.Gazetteer) *GeoService {
	return &GeoService{db: db, gaz: gaz}
}

// GeocodeAsset reverse-geocodes a single asset (by its exif lat/lon) and
// upserts the result into asset_geo. No-op if the asset has no GPS.
func (s *GeoService) GeocodeAsset(assetID string) error {
	var lat, lon sql.NullFloat64
	err := s.db.QueryRow(
		`SELECT latitude, longitude FROM asset_exif WHERE asset_id=?`, assetID).
		Scan(&lat, &lon)
	if err == sql.ErrNoRows || !lat.Valid || !lon.Valid {
		return nil
	}
	if err != nil {
		return fmt.Errorf("GeocodeAsset read exif: %w", err)
	}
	if lat.Float64 == 0 && lon.Float64 == 0 {
		return nil
	}
	r, ok := s.gaz.ReverseGeocode(lat.Float64, lon.Float64)
	if !ok {
		return nil
	}
	_, err = s.db.Exec(`
INSERT INTO asset_geo(asset_id, city_id, city, country, region, admin1, lat, lon, geocoded_at)
VALUES(?,?,?,?,?,?,?,?,?)
ON CONFLICT(asset_id) DO UPDATE SET
  city_id=excluded.city_id, city=excluded.city, country=excluded.country,
  region=excluded.region, admin1=excluded.admin1, lat=excluded.lat,
  lon=excluded.lon, geocoded_at=excluded.geocoded_at`,
		assetID, r.CityID, r.City, r.Country, r.Region, r.Admin1,
		lat.Float64, lon.Float64, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("GeocodeAsset upsert: %w", err)
	}
	return nil
}
