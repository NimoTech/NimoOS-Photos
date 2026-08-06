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
       SUM(CASE WHEN COALESCE(a.mime_type,'') LIKE 'video/%' THEN 1 ELSE 0 END)
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
		if err := rows.Scan(&b.Year, &b.Month, &b.Count, &b.VideoCount); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
