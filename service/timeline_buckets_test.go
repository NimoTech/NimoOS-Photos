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
