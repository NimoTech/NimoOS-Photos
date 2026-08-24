package service

import "fmt"

// TimelineBucket is one month's summary in the timeline bucket directory.
// Year==0 && Month==0 is the "unknown date" bucket (taken_at and indexed_at
// both NULL), always sorted last.
type TimelineBucket struct {
	Year       int `json:"year"`
	Month      int `json:"month"`
	Count      int `json:"count"`
	VideoCount int `json:"videoCount"`
	OCRCount   int `json:"ocrCount"`
}

// TimelineBuckets returns the per-month asset counts for the timeline, newest
// first. This is the cheap "directory" half of the paginated timeline: the
// frontend renders scrollbar/placeholders from it and fetches actual assets
// per bucket via TimelineBucketAssets. The GROUP BY key expression matches
// idx_assets_monthkey verbatim so the whole query is an index scan.
//
// Note: strftime buckets by the stored timestamp's UTC representation, while
// the older Timeline (search.go) buckets in Go using time.Time's own
// timezone. A photo taken late at night near month-end can therefore land in
// a different bucket than the legacy grouping. This is an accepted deviation:
// the bucket directory and the paginated bucket contents (task B2) share this
// exact expression, so the directory and its contents always stay consistent
// with each other.
func (s *SearchService) TimelineBuckets() ([]TimelineBucket, error) {
	rows, err := s.db.Query(`
SELECT COALESCE(CAST(strftime('%Y', COALESCE(a.taken_at, a.indexed_at)) AS INTEGER), 0),
       COALESCE(CAST(strftime('%m', COALESCE(a.taken_at, a.indexed_at)) AS INTEGER), 0),
       COUNT(*),
       SUM(CASE WHEN COALESCE(a.mime_type,'') LIKE 'video/%' THEN 1 ELSE 0 END),
       SUM(CASE WHEN ` + hasOcrExpr + ` THEN 1 ELSE 0 END)
FROM assets a
WHERE a.is_live_photo_video = 0 AND a.deleted_at IS NULL AND a.offline = 0
GROUP BY strftime('%Y-%m', COALESCE(a.taken_at, a.indexed_at))
ORDER BY 1 DESC, 2 DESC`)
	if err != nil {
		return nil, fmt.Errorf("TimelineBuckets query: %w", err)
	}
	defer rows.Close()
	out := []TimelineBucket{}
	for rows.Next() {
		var b TimelineBucket
		if err := rows.Scan(&b.Year, &b.Month, &b.Count, &b.VideoCount, &b.OCRCount); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// TimelineBucketAssets returns one month bucket's assets, newest first,
// paginated. Column set matches the legacy Timeline query exactly so the
// frontend's asset transform works unchanged. The WHERE month-key expression
// matches idx_assets_monthkey verbatim: equality on the key plus the index's
// second column ordering makes each page O(offset+limit) instead of a full
// re-sort. year==0 && month==0 addresses the "unknown date" bucket.
func (s *SearchService) TimelineBucketAssets(userID string, year, month, limit, offset int) ([]Asset, error) {
	// Clamp defensively here too, not just at the route layer (timeline.go):
	// this keeps the ≤500-row IN-batch contract attachNamedFaces relies on
	// intact even for a future direct caller that bypasses the HTTP handler.
	// limit<=0 also covers SQL's own LIMIT 0 quirk (zero rows, not
	// "everything"), so a zero/negative value must fall back to the default
	// page size rather than being passed through verbatim.
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	monthCond := `strftime('%Y-%m', COALESCE(a.taken_at, a.indexed_at)) = ?`
	args := []interface{}{userID}
	if year == 0 && month == 0 {
		monthCond = `strftime('%Y-%m', COALESCE(a.taken_at, a.indexed_at)) IS NULL`
	} else {
		args = append(args, fmt.Sprintf("%04d-%02d", year, month))
	}
	args = append(args, limit, offset)

	rows, err := s.db.Query(`
SELECT a.id, a.file_path, a.file_size, COALESCE(a.mime_type,''),
       COALESCE(a.original_name,''), a.taken_at, a.duration_ms,
       COALESCE(a.live_photo_video_id,''), a.is_live_photo_video, `+hasOcrExpr+`,
       a.indexed_at, a.status,
       e.width, e.height, e.latitude, e.longitude, e.make, e.model,
       e.iso, e.shutter_speed, e.aperture, e.focal_length, e.orientation,
       e.video_codec, e.audio_codec, e.frame_rate, e.bit_rate, e.rotation,
       f.favorited_at
FROM assets a
LEFT JOIN asset_exif e ON e.asset_id = a.id
LEFT JOIN asset_favorites f ON f.asset_id = a.id AND f.user_id = ?
WHERE a.is_live_photo_video = 0 AND a.deleted_at IS NULL AND a.offline = 0
  AND `+monthCond+`
ORDER BY COALESCE(a.taken_at, a.indexed_at) DESC
LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("TimelineBucketAssets query: %w", err)
	}
	defer rows.Close()

	assets, err := scanAssetsDetailedWithFav(rows)
	if err != nil {
		return nil, err
	}
	enrichPlaceNames(s.db, assets)
	if err := s.attachNamedFaces(assets); err != nil {
		return nil, err
	}
	return assets, nil
}
