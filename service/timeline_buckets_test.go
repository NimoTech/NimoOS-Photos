package service_test

import (
	"testing"

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
