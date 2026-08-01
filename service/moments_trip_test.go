// Tests for the trip engine: covers the Step 1 brief checklist — two GPS
// clusters 20 days apart produce 2 drafts, a small 5-photo cluster is
// filtered by min_assets, dual-city naming, subtitle formatting (including
// cross-month), id stability across recomputes of the same data.
package service

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// insertTripAsset inserts an asset satisfying the candidate pool criteria
// (status='indexed', not trashed, no OCR i.e. not a document) + a
// corresponding asset_geo row, for use by trip engine tests.
func insertTripAsset(t *testing.T, db *sql.DB, id string, takenAt time.Time, city, country string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO assets(id, file_path, status, taken_at) VALUES(?,?,'indexed',?)`,
		id, "/g/"+id+".jpg", takenAt.UTC().Format("2006-01-02 15:04:05"))
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO asset_geo(asset_id, city, country) VALUES(?,?,?)`, id, city, country)
	require.NoError(t, err)
}

// day is a shorthand for building test timestamps: 2011, month, day d, 12:00 UTC.
func day(month time.Month, d int) time.Time {
	return time.Date(2011, month, d, 12, 0, 0, 0, time.UTC)
}

func TestBuildTripMoments_SplitFilterAndNaming(t *testing.T) {
	db := makeTestDB(t)

	// Segment A: 6 Tokyo photos, Apr 1 ~ Apr 6 (single city, same month).
	segA := []time.Time{day(4, 1), day(4, 2), day(4, 3), day(4, 4), day(4, 5), day(4, 6)}
	for i, ts := range segA {
		insertTripAsset(t, db, "a"+string(rune('0'+i)), ts, "Tokyo", "Japan")
	}

	// Small cluster: 5 Kyoto photos, 20 days apart from segment A (4/6 →
	// 4/26), a count below min_assets=6, should be filtered.
	small := []time.Time{day(4, 26), day(4, 27), day(4, 28), day(4, 29), day(4, 30)}
	for i, ts := range small {
		insertTripAsset(t, db, "s"+string(rune('0'+i)), ts, "Kyoto", "Japan")
	}

	// Segment C: 6 photos, 20 days apart from the small cluster (4/30 →
	// 5/20), spanning May/June, dual-city (Paris 4 + Lyon 2, Lyon's share
	// 2/6=33.3% > 30%).
	segC := []struct {
		ts   time.Time
		city string
	}{
		{day(5, 20), "Paris"},
		{day(5, 22), "Paris"},
		{day(5, 24), "Paris"},
		{day(5, 26), "Paris"},
		{day(5, 30), "Lyon"},
		{time.Date(2011, 6, 4, 12, 0, 0, 0, time.UTC), "Lyon"},
	}
	for i, c := range segC {
		insertTripAsset(t, db, "c"+string(rune('0'+i)), c.ts, c.city, "France")
	}

	recipe := MomentRecipe{Key: "trip", Kind: "trip", ParamsJSON: `{"min_assets":6}`}

	drafts, err := BuildTripMoments(context.Background(), db, recipe)
	require.NoError(t, err)
	require.Len(t, drafts, 2, "the small cluster should be filtered by min_assets, leaving only segment A + segment C")

	dA, dC := drafts[0], drafts[1]

	// Segment A: single city, same-month subtitle, 6 members.
	require.Equal(t, "Tokyo", dA.Place)
	require.Equal(t, "Tokyo Trip", dA.Title)
	require.Equal(t, "Apr 2011", dA.Subtitle)
	require.Len(t, dA.Assets, 6)
	require.True(t, dA.TimeFrom.Equal(segA[0]))
	require.True(t, dA.TimeTo.Equal(segA[len(segA)-1]))
	for _, a := range dA.Assets {
		require.False(t, a.Featured)
		require.Zero(t, a.Score)
	}

	// Segment C: dual-city naming + cross-month subtitle (en dash, a space on each side).
	require.Equal(t, "Paris & Lyon", dC.Place)
	require.Equal(t, "Paris & Lyon Trip", dC.Title)
	require.Equal(t, "May – Jun 2011", dC.Subtitle)
	require.Len(t, dC.Assets, 6)
	require.True(t, dC.TimeFrom.Equal(segC[0].ts))
	require.True(t, dC.TimeTo.Equal(segC[len(segC)-1].ts))

	// id stability: recomputing the same data should yield exactly the same id
	// set (not "renamed" depending on recompute order/traversal).
	drafts2, err := BuildTripMoments(context.Background(), db, recipe)
	require.NoError(t, err)
	require.Len(t, drafts2, 2)
	require.Equal(t, dA.ID, drafts2[0].ID)
	require.Equal(t, dC.ID, drafts2[1].ID)
	require.Equal(t, TripMomentID("trip", segA[0]), dA.ID)
	require.Equal(t, TripMomentID("trip", segC[0].ts), dC.ID)
}

func TestBuildTripMoments_EmptyPool(t *testing.T) {
	db := makeTestDB(t)
	recipe := MomentRecipe{Key: "trip", Kind: "trip"}
	drafts, err := BuildTripMoments(context.Background(), db, recipe)
	require.NoError(t, err)
	require.Empty(t, drafts)
}

func TestBuildTripMoments_ExcludesTrashOfflineAndDocs(t *testing.T) {
	db := makeTestDB(t)

	// 6 normal Tokyo photos (a custom threshold below the default min_assets=10).
	recipe := MomentRecipe{Key: "trip", Kind: "trip", ParamsJSON: `{"min_assets":6}`}
	base := []time.Time{day(7, 1), day(7, 2), day(7, 3), day(7, 4), day(7, 5), day(7, 6)}
	for i, ts := range base {
		insertTripAsset(t, db, "t"+string(rune('0'+i)), ts, "Tokyo", "Japan")
	}

	// A trashed asset (deleted_at non-null): should not count toward the
	// candidate pool, and should not affect the gap check either.
	insertTripAsset(t, db, "trashed", day(7, 7), "Tokyo", "Japan")
	_, err := db.Exec(`UPDATE assets SET deleted_at=? WHERE id='trashed'`, "2011-07-08 00:00:00")
	require.NoError(t, err)

	// An offline asset (offline=1): excluded for the same reason.
	insertTripAsset(t, db, "offline1", day(7, 8), "Tokyo", "Japan")
	_, err = db.Exec(`UPDATE assets SET offline=1 WHERE id='offline1'`)
	require.NoError(t, err)

	// A document-type asset (the hasOcrExpr criterion holds): the density gate
	// qualifies (coverage/line_count) + is_doc=1, excluded.
	insertTripAsset(t, db, "doc1", day(7, 9), "Tokyo", "Japan")
	_, err = db.Exec(`INSERT INTO asset_ocr(asset_id, text, coverage, line_count, is_doc)
		VALUES('doc1','some long ocr text with many lines',0.9,20,1)`)
	require.NoError(t, err)

	drafts, err := BuildTripMoments(context.Background(), db, recipe)
	require.NoError(t, err)
	require.Len(t, drafts, 1)
	require.Len(t, drafts[0].Assets, 6, "trashed/offline/document assets should not count toward the candidate pool")
}

// TestBuildTripMoments_ExcludesLivePhotoVideoSide covers a gap found during
// review: a live photo's MOV video side lands its own asset_geo row for the
// same instant as its still photo (see geo.go's BackfillPending, which
// doesn't exclude the video side); if the candidate pool didn't exclude
// is_live_photo_video=1, the same instant would be double-counted into a
// segment, inflating the count, skewing the dominant-city share, and
// duplicating segment members. Aligned with this codebase's other geo JOIN
// queries (places.go/persons.go/search.go/smartview.go etc.).
func TestBuildTripMoments_ExcludesLivePhotoVideoSide(t *testing.T) {
	db := makeTestDB(t)
	recipe := MomentRecipe{Key: "trip", Kind: "trip", ParamsJSON: `{"min_assets":6}`}

	base := []time.Time{day(8, 1), day(8, 2), day(8, 3), day(8, 4), day(8, 5), day(8, 6)}
	for i, ts := range base {
		insertTripAsset(t, db, "p"+string(rune('0'+i)), ts, "Tokyo", "Japan")
	}

	// p2's (base[2]=8/3) live photo video side: same instant, same city, also has an asset_geo row.
	insertTripAsset(t, db, "p2_video", day(8, 3), "Tokyo", "Japan")
	_, err := db.Exec(`UPDATE assets SET is_live_photo_video=1 WHERE id='p2_video'`)
	require.NoError(t, err)

	drafts, err := BuildTripMoments(context.Background(), db, recipe)
	require.NoError(t, err)
	require.Len(t, drafts, 1)
	require.Len(t, drafts[0].Assets, 6, "a live photo video side should not count toward the candidate pool, only the still photo side counts")
	for _, a := range drafts[0].Assets {
		require.NotEqual(t, "p2_video", a.AssetID)
	}
}

// TestTripSubtitle_CrossYear pins down the cross-year scenario: the year on
// both sides, e.g. "Dec 2011 – Jan 2012".
func TestTripSubtitle_CrossYear(t *testing.T) {
	from := time.Date(2011, time.December, 20, 0, 0, 0, 0, time.UTC)
	to := time.Date(2012, time.January, 3, 0, 0, 0, 0, time.UTC)
	require.Equal(t, "Dec 2011 – Jan 2012", tripSubtitle(from, to))
}
