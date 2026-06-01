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

const tripGapDays = 14

type takenItem struct {
	id string
	t  time.Time
}

// loadTakenTimes returns ordered (asc) taken_at + asset id for a city.
func (s *PlacesService) loadTakenTimes(cityID int32) ([]takenItem, error) {
	rows, err := s.db.Query(`
SELECT a.id, a.taken_at FROM asset_geo g
JOIN assets a ON a.id=g.asset_id AND a.deleted_at IS NULL AND a.is_live_photo_video=0
WHERE g.city_id=? AND a.taken_at IS NOT NULL
ORDER BY a.taken_at ASC`, cityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []takenItem
	for rows.Next() {
		var id string
		var ts sql.NullString
		if err := rows.Scan(&id, &ts); err != nil {
			return nil, err
		}
		if tt := parseSQLiteTime(ts); tt != nil {
			out = append(out, takenItem{id: id, t: *tt})
		}
	}
	return out, rows.Err()
}

type segment struct{ start, end int }

func splitTrips(times []time.Time) []segment {
	if len(times) == 0 {
		return nil
	}
	segs := []segment{{0, 0}}
	for i := 1; i < len(times); i++ {
		if times[i].Sub(times[i-1]) > tripGapDays*24*time.Hour {
			segs = append(segs, segment{i, i})
		} else {
			segs[len(segs)-1].end = i
		}
	}
	return segs
}

func (s *PlacesService) tripCount(cityID int32) int {
	items, err := s.loadTakenTimes(cityID)
	if err != nil || len(items) == 0 {
		return 1
	}
	ts := make([]time.Time, len(items))
	for i := range items {
		ts[i] = items[i].t
	}
	return len(splitTrips(ts))
}

// Visits returns detected trips, most recent first.
func (s *PlacesService) Visits(cityID int32) ([]Visit, error) {
	items, err := s.loadTakenTimes(cityID)
	if err != nil {
		return nil, err
	}
	ts := make([]time.Time, len(items))
	for i := range items {
		ts[i] = items[i].t
	}
	segs := splitTrips(ts)
	var out []Visit
	for _, seg := range segs {
		from := items[seg.start].t
		to := items[seg.end].t
		v := Visit{
			From:    from.Format("2006-01-02"),
			To:      to.Format("2006-01-02"),
			Days:    int(to.Sub(from).Hours()/24) + 1,
			Photos:  seg.end - seg.start + 1,
			Current: time.Since(to) <= recentDays*24*time.Hour,
		}
		if from.Format("Jan 2, 2006") == to.Format("Jan 2, 2006") {
			v.When = from.Format("Jan 2, 2006")
		} else {
			v.When = from.Format("Jan 2") + " – " + to.Format("Jan 2, 2006")
		}
		for i := seg.start; i <= seg.end && len(v.Thumbs) < 6; i++ {
			v.Thumbs = append(v.Thumbs, items[i].id)
		}
		v.Faces = s.topFacesBetween(cityID, from, to, 3)
		v.Spots = len(s.spots(cityID))
		out = append(out, v)
	}
	// reverse → most recent first
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// topFacesBetween returns up to n person names appearing in a city within [from,to].
func (s *PlacesService) topFacesBetween(cityID int32, from, to time.Time, n int) []string {
	rows, err := s.db.Query(`
SELECT p.name, COUNT(*) c FROM asset_geo g
JOIN assets a ON a.id=g.asset_id AND a.deleted_at IS NULL
JOIN face_person fp ON fp.face_id IN (
    SELECT fd.id FROM face_detections fd WHERE fd.asset_id=a.id
)
JOIN persons p ON p.id=fp.person_id
WHERE g.city_id=? AND a.taken_at>=? AND a.taken_at<=? AND p.name IS NOT NULL AND p.name<>''
GROUP BY p.id ORDER BY c DESC LIMIT ?`,
		cityID, from.Format("2006-01-02 15:04:05"), to.Format("2006-01-02 15:04:05"), n)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		var c int
		if rows.Scan(&name, &c) == nil {
			out = append(out, name)
		}
	}
	return out
}

const spotGrid = 0.01 // ~1km
const spotMinPhotos = 3
const spotMaxCount = 8

// Spots clusters a city's assets into fine-grained spots.
func (s *PlacesService) Spots(cityID int32) []Spot { return s.spots(cityID) }

func (s *PlacesService) spots(cityID int32) []Spot {
	rows, err := s.db.Query(`
SELECT a.id, g.lat, g.lon FROM asset_geo g
JOIN assets a ON a.id=g.asset_id AND a.deleted_at IS NULL AND a.is_live_photo_video=0
WHERE g.city_id=? AND g.lat IS NOT NULL AND g.lon IS NOT NULL
ORDER BY a.taken_at DESC`, cityID)
	if err != nil {
		return nil
	}
	defer rows.Close()

	type bucket struct {
		count   int
		sumLat  float64
		sumLon  float64
		firstID string
	}
	buckets := map[[2]int]*bucket{}
	var order [][2]int
	for rows.Next() {
		var id string
		var lat, lon float64
		if rows.Scan(&id, &lat, &lon) != nil {
			continue
		}
		k := [2]int{int(lat / spotGrid), int(lon / spotGrid)}
		b := buckets[k]
		if b == nil {
			b = &bucket{firstID: id}
			buckets[k] = b
			order = append(order, k)
		}
		b.count++
		b.sumLat += lat
		b.sumLon += lon
	}

	var spots []Spot
	n := 0
	for _, k := range order {
		b := buckets[k]
		if b.count < spotMinPhotos {
			continue
		}
		cLat := b.sumLat / float64(b.count)
		cLon := b.sumLon / float64(b.count)
		name, ok := s.gaz.NearestFeature(cLat, cLon, 5)
		if !ok {
			n++
			name = fmt.Sprintf("Spot %d", n)
		}
		spots = append(spots, Spot{
			Key:   fmt.Sprintf("%d:%d:%d", cityID, k[0], k[1]),
			Name:  name,
			Lat:   cLat,
			Lon:   cLon,
			Count: b.count,
			Thumb: b.firstID,
		})
	}
	sort.Slice(spots, func(i, j int) bool { return spots[i].Count > spots[j].Count })
	if len(spots) > spotMaxCount {
		spots = spots[:spotMaxCount]
	}
	return spots
}
