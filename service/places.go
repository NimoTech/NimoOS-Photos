package service

import (
	"database/sql"
	"fmt"
	"sort"
	"time"

	"github.com/NimoTech/NimoOS-Photos/pkg/geo"
)

// PlacesService aggregates geocoded assets into places, spots, trips and insights.
type PlacesService struct {
	db  *sql.DB
	gaz *geo.Gazetteer
	geo *GeoService
}

// NewPlacesService constructs a PlacesService.
func NewPlacesService(db *sql.DB, gaz *geo.Gazetteer, geoSvc *GeoService) *PlacesService {
	return &PlacesService{db: db, gaz: gaz, geo: geoSvc}
}

const recentDays = 30

// ListPlaces returns all places with aggregated stats.
func (s *PlacesService) ListPlaces() (PlacesResponse, error) {
	rows, err := s.db.Query(`
SELECT g.city_id, g.city, g.country, g.region, COUNT(*) AS cnt, MAX(a.taken_at) AS last_taken
FROM asset_geo g
JOIN assets a ON a.id=g.asset_id AND a.deleted_at IS NULL AND a.is_live_photo_video=0
WHERE g.city_id IS NOT NULL
GROUP BY g.city_id
ORDER BY cnt DESC`)
	if err != nil {
		return PlacesResponse{}, fmt.Errorf("ListPlaces: %w", err)
	}
	defer rows.Close()

	var resp PlacesResponse
	regionCounts := map[string]int{}
	countries := map[string]struct{}{}
	maxTrips := 0
	type tmp struct {
		p Place
	}
	var tmps []tmp

	for rows.Next() {
		var cityID int32
		var city, country, region string
		var cnt int
		var lastTaken sql.NullString
		if err := rows.Scan(&cityID, &city, &country, &region, &cnt, &lastTaken); err != nil {
			return PlacesResponse{}, fmt.Errorf("ListPlaces scan: %w", err)
		}
		lat, lon := 0.0, 0.0
		if c, ok := s.gaz.CityByID(cityID); ok {
			lat, lon = c.Lat, c.Lon
		}
		p := Place{
			Key: cityID, Region: region, Country: country, City: city,
			Lat: lat, Lon: lon, Count: cnt,
		}
		lt := parseSQLiteTime(lastTaken)
		if lt != nil {
			p.Last = lt.Format("Jan 2, 2006")
			p.Recent = time.Since(*lt) <= recentDays*24*time.Hour
		}
		p.Trips = s.tripCount(cityID)
		if p.Trips > maxTrips {
			maxTrips = p.Trips
		}
		p.Thumbs = s.recentThumbs(cityID, 12)
		regionCounts[region]++
		countries[country] = struct{}{}
		tmps = append(tmps, tmp{p: p})
	}
	if err := rows.Err(); err != nil {
		return PlacesResponse{}, err
	}

	for i := range tmps {
		if tmps[i].p.Trips == maxTrips && maxTrips >= 2 {
			tmps[i].p.Home = true
		}
		resp.Places = append(resp.Places, tmps[i].p)
		resp.Stats.Photos += tmps[i].p.Count
	}
	resp.Stats.Cities = len(resp.Places)
	resp.Stats.Countries = len(countries)

	for region, n := range regionCounts {
		label := regionLabels[region]
		if label == "" {
			label = region
		}
		resp.Regions = append(resp.Regions, RegionCount{ID: region, Label: label, Count: n})
	}
	sort.Slice(resp.Regions, func(i, j int) bool { return resp.Regions[i].Count > resp.Regions[j].Count })
	return resp, nil
}

// recentThumbs returns up to n most-recent asset ids for a city.
func (s *PlacesService) recentThumbs(cityID int32, n int) []string {
	rows, err := s.db.Query(`
SELECT a.id FROM asset_geo g
JOIN assets a ON a.id=g.asset_id AND a.deleted_at IS NULL AND a.is_live_photo_video=0
WHERE g.city_id=?
ORDER BY a.taken_at DESC
LIMIT ?`, cityID, n)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			out = append(out, id)
		}
	}
	return out
}

// tripCount is a placeholder replaced in a later task.
func (s *PlacesService) tripCount(cityID int32) int { return 1 }
