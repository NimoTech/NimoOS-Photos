package service

import "path/filepath"

// AlbumPlaceCount is one city bucket in an album summary.
type AlbumPlaceCount struct {
	City    string `json:"city"`
	Country string `json:"country"`
	Count   int    `json:"count"`
}

// AlbumPersonCount is one named-person bucket in an album summary.
type AlbumPersonCount struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// AlbumSummary aggregates the naming signals of one album: counts, taken_at
// date range, top places/named persons, OCR text samples, filename samples
// and up to three time-spread photo IDs for vision fallback. Consumed by the
// AI agent's get_album_summary tool.
type AlbumSummary struct {
	AssetCount      int                `json:"assetCount"`
	PhotoCount      int                `json:"photoCount"`
	VideoCount      int                `json:"videoCount"`
	DateStart       string             `json:"dateStart"`
	DateEnd         string             `json:"dateEnd"`
	TopPlaces       []AlbumPlaceCount  `json:"topPlaces"`
	TopPersons      []AlbumPersonCount `json:"topPersons"`
	OCRSamples      []string           `json:"ocrSamples"`
	SampleFilenames []string           `json:"sampleFilenames"`
	CoverCandidates []string           `json:"coverCandidates"`
}

// Summary returns the AlbumSummary for albumID, or ErrNotFound when the
// album does not exist. An existing empty album yields a zero summary with
// empty (non-nil) slices so the JSON encodes as [] not null.
func (s *AlbumService) Summary(albumID string) (*AlbumSummary, error) {
	// Step 1: verify album exists.
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM albums WHERE id=?`, albumID).Scan(&count); err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, ErrNotFound
	}

	sum := &AlbumSummary{
		TopPlaces:       []AlbumPlaceCount{},
		TopPersons:      []AlbumPersonCount{},
		OCRSamples:      []string{},
		SampleFilenames: []string{},
		CoverCandidates: []string{},
	}

	// Step 2: counts and date range — exclude trashed and live companion videos.
	var dateStart, dateEnd string
	err := s.db.QueryRow(`
		SELECT
			COUNT(*),
			COALESCE(SUM(CASE WHEN COALESCE(a.mime_type,'') NOT LIKE 'video/%' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN COALESCE(a.mime_type,'') LIKE 'video/%' THEN 1 ELSE 0 END), 0),
			COALESCE(MIN(date(a.taken_at)),''),
			COALESCE(MAX(date(a.taken_at)),'')
		FROM album_assets aa
		JOIN assets a ON a.id = aa.asset_id
		WHERE aa.album_id = ?
		  AND a.deleted_at IS NULL
		  AND a.is_live_photo_video = 0
	`, albumID).Scan(&sum.AssetCount, &sum.PhotoCount, &sum.VideoCount, &dateStart, &dateEnd)
	if err != nil {
		return nil, err
	}
	sum.DateStart = dateStart
	sum.DateEnd = dateEnd

	if sum.AssetCount == 0 {
		return sum, nil
	}

	// Step 3: top 3 cities by non-trashed, non-live-companion assets.
	rows, err := s.db.Query(`
		SELECT g.city, g.country, COUNT(*) AS cnt
		FROM album_assets aa
		JOIN assets a ON a.id = aa.asset_id
		JOIN asset_geo g ON g.asset_id = a.id
		WHERE aa.album_id = ?
		  AND a.deleted_at IS NULL
		  AND a.is_live_photo_video = 0
		  AND COALESCE(g.city,'') <> ''
		GROUP BY g.city, g.country
		ORDER BY cnt DESC, g.city
		LIMIT 3
	`, albumID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var p AlbumPlaceCount
		if err := rows.Scan(&p.City, &p.Country, &p.Count); err != nil {
			rows.Close()
			return nil, err
		}
		sum.TopPlaces = append(sum.TopPlaces, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()

	// Step 4: top 3 named persons — exclude unnamed persons and excluded face detections.
	// Count distinct assets per named person.
	rows, err = s.db.Query(`
		SELECT p.name, COUNT(DISTINCT a.id) AS cnt
		FROM album_assets aa
		JOIN assets a ON a.id = aa.asset_id
		JOIN face_detections fd ON fd.asset_id = a.id
		JOIN face_person fp ON fp.face_id = fd.id
		JOIN persons p ON p.id = fp.person_id
		WHERE aa.album_id = ?
		  AND a.deleted_at IS NULL
		  AND a.is_live_photo_video = 0
		  AND fd.excluded = 0
		  AND p.name <> ''
		GROUP BY p.id, p.name
		ORDER BY cnt DESC, p.name
		LIMIT 3
	`, albumID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var pc AlbumPersonCount
		if err := rows.Scan(&pc.Name, &pc.Count); err != nil {
			rows.Close()
			return nil, err
		}
		sum.TopPersons = append(sum.TopPersons, pc)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()

	// Step 5: OCR samples — longest text first, truncated to 80 chars, up to 3.
	rows, err = s.db.Query(`
		SELECT substr(o.text, 1, 80)
		FROM album_assets aa
		JOIN assets a ON a.id = aa.asset_id
		JOIN asset_ocr o ON o.asset_id = a.id
		WHERE aa.album_id = ?
		  AND a.deleted_at IS NULL
		  AND a.is_live_photo_video = 0
		  AND o.text <> ''
		ORDER BY length(o.text) DESC
		LIMIT 3
	`, albumID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			rows.Close()
			return nil, err
		}
		sum.OCRSamples = append(sum.OCRSamples, text)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()

	// Step 6: filename samples — ascending taken_at order (NULL last), up to 5.
	// Use original_name when non-empty, fall back to file_path basename.
	rows, err = s.db.Query(`
		SELECT COALESCE(NULLIF(a.original_name,''), a.file_path)
		FROM album_assets aa
		JOIN assets a ON a.id = aa.asset_id
		WHERE aa.album_id = ?
		  AND a.deleted_at IS NULL
		  AND a.is_live_photo_video = 0
		ORDER BY a.taken_at IS NULL, a.taken_at
		LIMIT 5
	`, albumID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return nil, err
		}
		sum.SampleFilenames = append(sum.SampleFilenames, filepath.Base(name))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()

	// Step 7: cover candidates — photos only, time-spread (first, middle, last).
	rows, err = s.db.Query(`
		SELECT a.id
		FROM album_assets aa
		JOIN assets a ON a.id = aa.asset_id
		WHERE aa.album_id = ?
		  AND a.deleted_at IS NULL
		  AND a.is_live_photo_video = 0
		  AND COALESCE(a.mime_type,'') NOT LIKE 'video/%'
		ORDER BY a.taken_at IS NULL, a.taken_at
	`, albumID)
	if err != nil {
		return nil, err
	}
	var photoIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		photoIDs = append(photoIDs, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()

	n := len(photoIDs)
	if n > 0 {
		seen := make(map[string]struct{}, 3)
		for _, idx := range []int{0, n / 2, n - 1} {
			id := photoIDs[idx]
			if _, dup := seen[id]; !dup {
				sum.CoverCandidates = append(sum.CoverCandidates, id)
				seen[id] = struct{}{}
			}
		}
	}

	return sum, nil
}
