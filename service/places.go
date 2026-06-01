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
	db     *sql.DB
	gaz    *geo.Gazetteer
	geo    *GeoService
	albums *AlbumService
}

// NewPlacesService constructs a PlacesService.
func NewPlacesService(db *sql.DB, gaz *geo.Gazetteer, geoSvc *GeoService) *PlacesService {
	return &PlacesService{db: db, gaz: gaz, geo: geoSvc}
}

// NewPlacesServiceWithAlbums constructs a PlacesService with album creation support.
func NewPlacesServiceWithAlbums(db *sql.DB, gaz *geo.Gazetteer, geoSvc *GeoService, albums *AlbumService) *PlacesService {
	return &PlacesService{db: db, gaz: gaz, geo: geoSvc, albums: albums}
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
	spotCount := len(s.spots(cityID))
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
		v.Spots = spotCount
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
JOIN face_detections fd ON fd.asset_id=a.id
JOIN face_person fp ON fp.face_id=fd.id
JOIN persons p ON p.id=fp.person_id
WHERE g.city_id=? AND a.taken_at>=? AND a.taken_at<=? AND p.name IS NOT NULL AND p.name<>''
  AND COALESCE(fd.excluded,0)=0 AND COALESCE(p.hidden,0)=0
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

// GetPlace returns the full detail payload for a city.
func (s *PlacesService) GetPlace(cityID int32) (PlaceDetail, error) {
	resp, err := s.ListPlaces()
	if err != nil {
		return PlaceDetail{}, err
	}
	var base *Place
	for i := range resp.Places {
		if resp.Places[i].Key == cityID {
			base = &resp.Places[i]
			break
		}
	}
	if base == nil {
		return PlaceDetail{}, ErrNotFound
	}
	d := PlaceDetail{Place: *base}
	d.Spots = s.spots(cityID)
	d.Recent = s.recentThumbs(cityID, 8)
	d.Visits, _ = s.Visits(cityID)
	d.Insights = s.insights(cityID, *base, resp)
	return d, nil
}

// insights builds template-based observations. Each returns an i18n key + params;
// the frontend renders the localized string.
func (s *PlacesService) insights(cityID int32, p Place, resp PlacesResponse) []Insight {
	var out []Insight

	// 1. Most-photographed city this period.
	top := true
	for _, o := range resp.Places {
		if o.Count > p.Count {
			top = false
			break
		}
	}
	if top {
		out = append(out, Insight{Ico: "sparkles", Key: "photos.places.insight.mostPhotographed",
			Params: map[string]interface{}{"count": p.Count}})
	}

	// 2. Top spot.
	if sp := s.spots(cityID); len(sp) > 0 {
		out = append(out, Insight{Ico: "sparkles", Key: "photos.places.insight.topSpot",
			Params: map[string]interface{}{"spot": sp[0].Name, "count": sp[0].Count}})
	}

	// 3. Companions (top co-occurring people in this city).
	if faces := s.topFacesBetween(cityID, time.Unix(0, 0), time.Now().AddDate(1, 0, 0), 2); len(faces) > 0 {
		out = append(out, Insight{Ico: "person", Key: "photos.places.insight.companions",
			Params: map[string]interface{}{"names": faces}})
	}

	// 4. Home base.
	if p.Home {
		out = append(out, Insight{Ico: "home", Key: "photos.places.insight.home",
			Params: map[string]interface{}{"count": p.Count, "trips": p.Trips}})
	}
	return out
}

// SetCover persists a per-user cover override for a place. Validates the asset exists.
func (s *PlacesService) SetCover(userID string, placeKey int32, assetID string) error {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM assets WHERE id=? AND deleted_at IS NULL`, assetID).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	_, err := s.db.Exec(`
INSERT INTO place_cover_overrides(user_id, place_key, asset_id) VALUES(?,?,?)
ON CONFLICT(user_id, place_key) DO UPDATE SET asset_id=excluded.asset_id`,
		userID, placeKey, assetID)
	return err
}

// GetCover returns the override asset id, validating it still exists ("" if none/stale).
func (s *PlacesService) GetCover(userID string, placeKey int32) (string, error) {
	var id string
	err := s.db.QueryRow(`SELECT asset_id FROM place_cover_overrides WHERE user_id=? AND place_key=?`,
		userID, placeKey).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM assets WHERE id=? AND deleted_at IS NULL`, id).Scan(&n) //nolint:errcheck
	if n == 0 {
		return "", nil
	}
	return id, nil
}

// ResetCover removes a place's cover override.
func (s *PlacesService) ResetCover(userID string, placeKey int32) error {
	_, err := s.db.Exec(`DELETE FROM place_cover_overrides WHERE user_id=? AND place_key=?`, userID, placeKey)
	return err
}

// placeAssetIDs returns all asset ids for a city, optionally within [from, to] (YYYY-MM-DD).
func (s *PlacesService) placeAssetIDs(cityID int32, from, to string) ([]string, error) {
	q := `
SELECT a.id FROM asset_geo g
JOIN assets a ON a.id=g.asset_id AND a.deleted_at IS NULL AND a.is_live_photo_video=0
WHERE g.city_id=?`
	args := []any{cityID}
	if from != "" && to != "" {
		q += ` AND a.taken_at>=? AND a.taken_at<=?`
		args = append(args, from+" 00:00:00", to+" 23:59:59")
	}
	q += ` ORDER BY a.taken_at ASC`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	return ids, rows.Err()
}

// CoverCandidates returns paged asset ids for the cover picker.
// tab: "recent" | "top" | "fav" | "all"
// q is reserved for future search filtering (ignored this release).
// asset_favorites has a user_id column; fav counts here are cross-user (all favorites for the place).
func (s *PlacesService) CoverCandidates(cityID int32, tab, q string, page, pageSize int) (CoverCandidatesResult, error) {
	base := `
FROM asset_geo g
JOIN assets a ON a.id=g.asset_id AND a.deleted_at IS NULL AND a.is_live_photo_video=0`
	order := ` ORDER BY a.taken_at DESC`
	join := ``
	where := ` WHERE g.city_id=?`
	switch tab {
	case "top":
		join = ` LEFT JOIN asset_views v ON v.asset_id=a.id`
		order = ` ORDER BY COALESCE(v.view_count,0) DESC, a.taken_at DESC`
	case "fav":
		join = ` JOIN asset_favorites f ON f.asset_id=a.id`
	}
	rows, err := s.db.Query(`SELECT a.id `+base+join+where+order, cityID)
	if err != nil {
		return CoverCandidatesResult{}, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	if err := rows.Err(); err != nil {
		return CoverCandidatesResult{}, err
	}

	if page < 0 {
		page = 0
	}
	res := CoverCandidatesResult{Page: page, Total: len(ids)}
	res.TotalPages = (len(ids) + pageSize - 1) / pageSize
	if res.TotalPages < 1 {
		res.TotalPages = 1
	}
	start := page * pageSize
	if start < len(ids) {
		end := start + pageSize
		if end > len(ids) {
			end = len(ids)
		}
		res.Items = ids[start:end]
	}
	res.Tabs = s.coverTabCounts(cityID)
	return res, nil
}

func (s *PlacesService) coverTabCounts(cityID int32) []CoverTab {
	count := func(extra string) int {
		var n int
		s.db.QueryRow(`SELECT COUNT(*) FROM asset_geo g
JOIN assets a ON a.id=g.asset_id AND a.deleted_at IS NULL AND a.is_live_photo_video=0`+extra+
			` WHERE g.city_id=?`, cityID).Scan(&n) //nolint:errcheck
		return n
	}
	all := count("")
	recent := all
	if recent > 12 {
		recent = 12
	}
	return []CoverTab{
		{ID: "recent", Label: "近期", Icon: "clock", Count: recent},
		{ID: "top", Label: "最高分", Icon: "sparkles", Count: count(" LEFT JOIN asset_views v ON v.asset_id=a.id")},
		{ID: "fav", Label: "已收藏", Icon: "star", Count: count(" JOIN asset_favorites f ON f.asset_id=a.id")},
		{ID: "all", Label: "全部", Icon: "grid", Count: all},
	}
}

// CreateAlbumFromPlace creates an album from a city's assets (optionally a trip window).
func (s *PlacesService) CreateAlbumFromPlace(cityID int32, name, from, to string) (string, int, error) {
	ids, err := s.placeAssetIDs(cityID, from, to)
	if err != nil {
		return "", 0, err
	}
	if len(ids) == 0 {
		return "", 0, ErrInvalidInput
	}
	album, err := s.albums.Create(name)
	if err != nil {
		return "", 0, err
	}
	if err := s.albums.BatchAddAssets(album.ID, ids); err != nil {
		_ = s.albums.Delete(album.ID)
		return "", 0, err
	}
	return album.ID, len(ids), nil
}
