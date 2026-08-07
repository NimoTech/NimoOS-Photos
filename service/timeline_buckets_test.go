package service_test

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/stretchr/testify/require"
)

func TestTimelineBuckets(t *testing.T) {
	db := openPerfDB(t)
	seedPerfAssets(t, db, 1000) // ~26 months, odd rows video, every 50th trashed

	svc := service.NewSearchService(db, nil)
	buckets, err := svc.TimelineBuckets()
	require.NoError(t, err)
	require.NotEmpty(t, buckets)

	// Descending year/month.
	for i := 1; i < len(buckets); i++ {
		prev, cur := buckets[i-1], buckets[i]
		require.True(t, prev.Year > cur.Year || (prev.Year == cur.Year && prev.Month > cur.Month),
			"buckets must be sorted newest-first")
	}
	// Totals must equal the live (non-trashed) asset count.
	total, videos := 0, 0
	for _, b := range buckets {
		total += b.Count
		videos += b.VideoCount
	}
	require.Equal(t, 980, total, "trashed assets excluded")
	// Trashed ids are multiples of 50 (all even = images), so all 500 odd
	// video rows survive.
	require.Equal(t, 500, videos)
}

// TestTimelineBucketsUnknownDateBucket covers the {0,0} "unknown date"
// bucket: an asset with both taken_at and indexed_at NULL still must show up
// in TimelineBuckets (as the last, {Year:0,Month:0} entry) and be fetchable
// through TimelineBucketAssets(uid, 0, 0, ...) — this is the fallback bucket
// for assets that predate any timestamp being recorded at all.
func TestTimelineBucketsUnknownDateBucket(t *testing.T) {
	db := openPerfDB(t)
	seedPerfAssets(t, db, 200) // ordinary dated assets, all visible under the three predicates
	// Manually insert one asset with taken_at and indexed_at both NULL:
	// status='indexed' and the three visibility predicates
	// (is_live_photo_video=0, deleted_at IS NULL, offline=0) hold via column
	// defaults, so this row is visible but has no derivable date.
	_, err := db.Exec(`INSERT INTO assets(id, file_path, status) VALUES('unknown-date-1','/g/unknown-date-1.jpg','indexed')`)
	require.NoError(t, err)

	svc := service.NewSearchService(db, nil)
	buckets, err := svc.TimelineBuckets()
	require.NoError(t, err)
	require.NotEmpty(t, buckets)

	last := buckets[len(buckets)-1]
	require.Equal(t, 0, last.Year, "unknown-date bucket must be {0,0}")
	require.Equal(t, 0, last.Month, "unknown-date bucket must be {0,0}")
	require.Equal(t, 1, last.Count)

	page, err := svc.TimelineBucketAssets("u1", 0, 0, 10, 0)
	require.NoError(t, err)
	require.Len(t, page, 1)
	require.Equal(t, "unknown-date-1", page[0].ID)
}

func TestTimelineBucketsUsesMonthIndex(t *testing.T) {
	db := openPerfDB(t)
	seedPerfAssets(t, db, 2000)
	// openPerfDB's migrate() ran ANALYZE on an empty table, so sqlite_stat1
	// still reflects 0 rows after the bulk insert above. Refresh stats so the
	// planner's cost model reflects the seeded data, mirroring
	// TestListAssetsUsesSortIndex's precedent for the sibling sort-key index.
	_, err := db.Exec(`ANALYZE assets`)
	require.NoError(t, err)
	rows, err := db.Query(`EXPLAIN QUERY PLAN
SELECT strftime('%Y-%m', COALESCE(a.taken_at, a.indexed_at)), COUNT(*)
FROM assets a
WHERE a.is_live_photo_video = 0 AND a.deleted_at IS NULL AND a.offline = 0
GROUP BY 1`)
	require.NoError(t, err)
	defer rows.Close()
	plan := ""
	for rows.Next() {
		var a, b, c int
		var detail string
		require.NoError(t, rows.Scan(&a, &b, &c, &detail))
		plan += detail + "\n"
	}
	require.Contains(t, plan, "idx_assets_monthkey")
}

func TestTimelineBucketAssetsUsesMonthIndex(t *testing.T) {
	db := openPerfDB(t)
	seedPerfAssets(t, db, 2000)
	// Mirrors TestTimelineBucketsUsesMonthIndex / TestListAssetsUsesSortIndex:
	// without a fresh ANALYZE on the freshly-seeded table, the planner falls
	// back to a fixed equality-index cost guess and can pick a different
	// index (e.g. idx_assets_livevideo) plus a temp b-tree sort instead of
	// walking idx_assets_monthkey directly — the query still runs fast
	// in-memory either way, so a benchmark alone can't tell the two apart.
	_, err := db.Exec(`ANALYZE assets`)
	require.NoError(t, err)

	// Full shape of the equality-month-key branch TimelineBucketAssets
	// actually runs: userID join, hasOcrExpr's EXISTS column, equality on
	// the month key, ORDER BY COALESCE(...) DESC, LIMIT/OFFSET. Copied
	// verbatim from timeline_buckets.go so this test tracks the real query,
	// not an approximation of it.
	rows, err := db.Query(`EXPLAIN QUERY PLAN
SELECT a.id, a.file_path, a.file_size, COALESCE(a.mime_type,''),
       COALESCE(a.original_name,''), a.taken_at, a.duration_ms,
       COALESCE(a.live_photo_video_id,''), a.is_live_photo_video,
       EXISTS(SELECT 1 FROM asset_ocr ocr WHERE ocr.asset_id=a.id AND ocr.text<>'' AND COALESCE(ocr.coverage,1)>=0.05 AND COALESCE(ocr.line_count,0)>=8 AND COALESCE(ocr.is_doc,1)=1),
       a.indexed_at, a.status,
       e.width, e.height, e.latitude, e.longitude, e.make, e.model,
       e.iso, e.shutter_speed, e.aperture, e.focal_length, e.orientation,
       e.video_codec, e.audio_codec, e.frame_rate, e.bit_rate, e.rotation,
       f.favorited_at
FROM assets a
LEFT JOIN asset_exif e ON e.asset_id = a.id
LEFT JOIN asset_favorites f ON f.asset_id = a.id AND f.user_id = ?
WHERE a.is_live_photo_video = 0 AND a.deleted_at IS NULL AND a.offline = 0
  AND strftime('%Y-%m', COALESCE(a.taken_at, a.indexed_at)) = ?
ORDER BY COALESCE(a.taken_at, a.indexed_at) DESC
LIMIT ? OFFSET ?`, "u1", "2018-04", 500, 0)
	require.NoError(t, err)
	defer rows.Close()
	plan := ""
	for rows.Next() {
		var a, b, c int
		var detail string
		require.NoError(t, rows.Scan(&a, &b, &c, &detail))
		plan += detail + "\n"
	}
	require.Contains(t, plan, "idx_assets_monthkey",
		"bucket-assets query must equality-seek the month-key index, not fall back to a table scan plus temp b-tree sort")
	// Pin the negative directly: idx_assets_monthkey's second column already
	// orders by COALESCE(taken_at, indexed_at) DESC, so ORDER BY should be
	// satisfied by the index walk itself with no separate sort step.
	require.NotContains(t, plan, "TEMP B-TREE",
		"the index's own column order should satisfy ORDER BY without a temp b-tree sort")
}

func TestTimelineBucketAssets(t *testing.T) {
	db := openPerfDB(t)
	seedPerfAssets(t, db, 1000)

	svc := service.NewSearchService(db, nil)
	buckets, err := svc.TimelineBuckets()
	require.NoError(t, err)
	// The newest bucket may be a partial month — pick one big enough to page.
	var b service.TimelineBucket
	for _, cand := range buckets {
		if cand.Count >= 25 {
			b = cand
			break
		}
	}
	require.GreaterOrEqual(t, b.Count, 25, "need a bucket with enough assets to paginate")

	page1, err := svc.TimelineBucketAssets("u1", b.Year, b.Month, 10, 0)
	require.NoError(t, err)
	require.Len(t, page1, 10)
	// Newest-first within the bucket, and every asset belongs to the bucket.
	for i, a := range page1 {
		ts := a.TakenAt
		require.NotNil(t, ts)
		require.Equal(t, b.Year, ts.Year())
		require.Equal(t, b.Month, int(ts.Month()))
		if i > 0 {
			require.False(t, ts.After(*page1[i-1].TakenAt), "descending order")
		}
	}
	// Paging: page 2 starts where page 1 ended, no overlap.
	page2, err := svc.TimelineBucketAssets("u1", b.Year, b.Month, 10, 10)
	require.NoError(t, err)
	require.NotEqual(t, page1[0].ID, page2[0].ID)

	// Whole-bucket sweep equals the directory count.
	got := 0
	for off := 0; ; off += 500 {
		page, err := svc.TimelineBucketAssets("u1", b.Year, b.Month, 500, off)
		require.NoError(t, err)
		got += len(page)
		if len(page) < 500 {
			break
		}
	}
	require.Equal(t, b.Count, got)
}

// seedOneMonthAssets bulk-inserts n indexed assets, all timestamped one
// minute apart within a single given year/month, so a single bucket can be
// pushed past the 500-row IN-batch contract — seedPerfAssets' 3h cadence caps
// out well under 500 rows per month regardless of n.
func seedOneMonthAssets(t *testing.T, db *sql.DB, year, month, n int) {
	t.Helper()
	tx, err := db.Begin()
	require.NoError(t, err)
	stmt, err := tx.Prepare(`INSERT INTO assets
		(id, file_path, file_size, mime_type, taken_at, indexed_at, status, is_live_photo_video, offline)
		VALUES (?,?,?,?,?,?, 'indexed', 0, 0)`)
	require.NoError(t, err)
	base := time.Date(year, time.Month(month), 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("month-%04d%02d-%05d", year, month, i)
		taken := base.Add(time.Duration(i) * time.Minute)
		_, err = stmt.Exec(id, "/g/"+id+".jpg", int64(1000), "image/jpeg", taken, taken)
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())
}

// TestTimelineBucketAssetsClampsLimit pins TimelineBucketAssets' own internal
// limit clamp (<=500), independent of the route-layer clamp in timeline.go —
// a future direct caller that skips the route layer must not be able to blow
// past the ≤500-row IN-batch contract attachNamedFaces relies on. Without the
// clamp, SQL LIMIT 0 returns zero rows (not "everything") and LIMIT 9999
// would return more than 500 rows from an oversized bucket — both wrong.
func TestTimelineBucketAssetsClampsLimit(t *testing.T) {
	db := openPerfDB(t)
	seedOneMonthAssets(t, db, 2021, 5, 600)

	svc := service.NewSearchService(db, nil)

	// limit=0 must fall back to the 500 default, not literally zero rows.
	page, err := svc.TimelineBucketAssets("u1", 2021, 5, 0, 0)
	require.NoError(t, err)
	require.Len(t, page, 500)

	// limit=9999 must be clamped down to 500, even though the bucket has 600 rows.
	page, err = svc.TimelineBucketAssets("u1", 2021, 5, 9999, 0)
	require.NoError(t, err)
	require.Len(t, page, 500)
}

func BenchmarkTimelineLegacyVsBucket(b *testing.B) {
	db := openPerfDB(b)
	seedPerfAssets(b, db, 100_000)
	svc := service.NewSearchService(db, nil)
	b.Run("legacy-full-timeline", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := svc.Timeline("u1"); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("bucket-page", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := svc.TimelineBucketAssets("u1", 2020, 6, 500, 0); err != nil {
				b.Fatal(err)
			}
		}
	})
}
