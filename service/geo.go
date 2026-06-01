package service

import (
	"context"
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

// BackfillPending geocodes up to `limit` assets that have GPS but no
// asset_geo row yet. Returns the number processed.
func (s *GeoService) BackfillPending(limit int) (int, error) {
	rows, err := s.db.Query(`
SELECT e.asset_id FROM asset_exif e
JOIN assets a ON a.id=e.asset_id AND a.deleted_at IS NULL
LEFT JOIN asset_geo g ON g.asset_id=e.asset_id
WHERE g.asset_id IS NULL
  AND e.latitude IS NOT NULL AND e.longitude IS NOT NULL
  AND NOT (e.latitude=0 AND e.longitude=0)
LIMIT ?`, limit)
	if err != nil {
		return 0, fmt.Errorf("BackfillPending query: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	n := 0
	for _, id := range ids {
		if err := s.GeocodeAsset(id); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// StartScheduler runs BackfillPending in batches in the background until the
// context is cancelled, then polls periodically for newly-indexed assets.
// Mirrors FaceService.StartScheduler.
func (s *GeoService) StartScheduler(ctx context.Context) {
	go func() {
		for {
			n, err := s.BackfillPending(500)
			if err != nil {
				return
			}
			if n == 0 {
				break
			}
			select {
			case <-ctx.Done():
				return
			default:
			}
		}
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				for {
					n, err := s.BackfillPending(500)
					if err != nil || n == 0 {
						break
					}
				}
			}
		}
	}()
}
