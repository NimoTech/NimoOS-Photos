package service_test

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"path/filepath"
	"sync"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/common"
	"github.com/NimoTech/NimoOS-Photos/pkg/geo"
	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/stretchr/testify/require"
)

type mockTextML struct{}

func (m *mockTextML) CLIPTextEmbed(_ string) ([]float32, error) {
	v := make([]float32, common.CLIPDim)
	v[0] = 1.0
	return v, nil
}

func openSearchDB(t *testing.T) *service.SearchService {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "search.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	return service.NewSearchService(db, &mockTextML{})
}

func TestSmartSearch(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "s.db"))
	require.NoError(t, err)
	defer db.Close()

	db.Exec(`INSERT INTO assets(id, file_path, status, is_live_photo_video) VALUES('a1','/p/beach.jpg','indexed',0)`)
	db.Exec(`INSERT INTO asset_clip_idx(asset_id) VALUES('a1')`)
	var rowid int64
	db.QueryRow(`SELECT rowid FROM asset_clip_idx WHERE asset_id='a1'`).Scan(&rowid)

	vec := make([]float32, common.CLIPDim)
	vec[0] = 1.0
	db.Exec(`INSERT INTO clip_embeddings(rowid, embedding) VALUES(?,?)`, rowid, sqlite.SerializeFloat32(vec))

	svc := service.NewSearchService(db, &mockTextML{})
	results, err := svc.SmartSearch("beach", 10, 0, service.SearchFilters{})
	require.NoError(t, err)
	require.NotEmpty(t, results)
	require.Equal(t, "a1", results[0].ID)
}

// TestSearchAssetsByText verifies the extracted method reused by the Smart
// Moments theme engine (search.go's SearchAssetsByText, split out of
// SmartSearch's text-encode + vec KNN pathway): returns AssetScore sorted by
// distance, excluding live-photo video sides / trashed / offline assets, and
// unaffected by SmartSearch-only policies like IncludeOCR/MinMatchSimilarity.
func TestSearchAssetsByText(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "s2.db"))
	require.NoError(t, err)
	defer db.Close()

	db.Exec(`INSERT INTO assets(id, file_path, status, is_live_photo_video) VALUES('a1','/p/a1.jpg','indexed',0)`)
	db.Exec(`INSERT INTO asset_clip_idx(asset_id) VALUES('a1')`)
	var rowid int64
	db.QueryRow(`SELECT rowid FROM asset_clip_idx WHERE asset_id='a1'`).Scan(&rowid)
	vec := make([]float32, common.CLIPDim)
	vec[0] = 1.0
	db.Exec(`INSERT INTO clip_embeddings(rowid, embedding) VALUES(?,?)`, rowid, sqlite.SerializeFloat32(vec))

	// The live photo video side must not appear in the results.
	db.Exec(`INSERT INTO assets(id, file_path, status, is_live_photo_video) VALUES('a2','/p/a2.jpg','indexed',1)`)
	db.Exec(`INSERT INTO asset_clip_idx(asset_id) VALUES('a2')`)
	db.QueryRow(`SELECT rowid FROM asset_clip_idx WHERE asset_id='a2'`).Scan(&rowid)
	db.Exec(`INSERT INTO clip_embeddings(rowid, embedding) VALUES(?,?)`, rowid, sqlite.SerializeFloat32(vec))

	svc := service.NewSearchService(db, &mockTextML{})
	hits, err := svc.SearchAssetsByText(context.Background(), "beach", 10)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	require.Equal(t, "a1", hits[0].AssetID)
	require.Greater(t, hits[0].Score, 0.0)

	// Existing SmartSearch behavior is unaffected.
	results, err := svc.SmartSearch("beach", 10, 0, service.SearchFilters{})
	require.NoError(t, err)
	require.NotEmpty(t, results)
	require.Equal(t, "a1", results[0].ID)
}

// TestSmartSearchIncludeOCR verifies that with IncludeOCR on, OCR substring
// hits are pinned to the top at score 1.0, deduped against CLIP results by
// ID, and that the default (off) behavior is unchanged.
func TestSmartSearchIncludeOCR(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "ocr.db"))
	require.NoError(t, err)
	defer db.Close()

	// a1: CLIP vector only; a2: OCR text only; a3: has both (dedup scenario)
	for i, id := range []string{"a1", "a2", "a3"} {
		db.Exec(`INSERT INTO assets(id,file_path,status,is_live_photo_video,taken_at)
			VALUES(?,?,'indexed',0,?)`, id, "/p/"+id+".jpg", fmt.Sprintf("2025-07-%02d 10:00:00", i+1))
	}
	for _, id := range []string{"a1", "a3"} {
		db.Exec(`INSERT INTO asset_clip_idx(asset_id) VALUES(?)`, id)
		var rowid int64
		db.QueryRow(`SELECT rowid FROM asset_clip_idx WHERE asset_id=?`, id).Scan(&rowid)
		vec := make([]float32, common.CLIPDim)
		vec[0] = 1.0
		db.Exec(`INSERT INTO clip_embeddings(rowid,embedding) VALUES(?,?)`, rowid, sqlite.SerializeFloat32(vec))
	}
	db.Exec(`INSERT INTO asset_ocr(asset_id,text) VALUES('a2','SUPERMART Receipt TOTAL $42.00')`)
	db.Exec(`INSERT INTO asset_ocr(asset_id,text) VALUES('a3','Invoice receipt 2024')`)

	svc := service.NewSearchService(db, &mockTextML{})

	// Default off: pure CLIP, OCR-only a2 does not appear
	results, err := svc.SmartSearch("receipt", 10, 0, service.SearchFilters{})
	require.NoError(t, err)
	for _, a := range results {
		require.NotEqual(t, "a2", a.ID, "should not return an OCR-only hit when IncludeOCR=false")
	}

	// On: a2, a3 pinned to the top at score 1.0 (newest-first within the OCR
	// group → a3 first), a1 still present
	results, err = svc.SmartSearch("receipt", 10, 0, service.SearchFilters{IncludeOCR: true})
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(results), 3)
	require.Equal(t, "a3", results[0].ID)
	require.Equal(t, "a2", results[1].ID)
	require.NotNil(t, results[0].MatchScore)
	require.InDelta(t, 1.0, *results[0].MatchScore, 1e-9)
	// OCR hits are tagged matchedBy="ocr" (the OCR version wins on a double hit); pure CLIP hits are tagged "semantic"
	require.Equal(t, "ocr", results[0].MatchedBy)
	require.Equal(t, "ocr", results[1].MatchedBy)
	for _, a := range results {
		if a.ID == "a1" {
			require.Equal(t, "semantic", a.MatchedBy)
		}
	}
	ids := map[string]int{}
	for _, a := range results {
		ids[a.ID]++
	}
	require.Equal(t, 1, ids["a3"], "a double hit must be deduped")
	require.Equal(t, 1, ids["a1"])

	// Case-insensitive
	results, err = svc.SmartSearch("RECEIPT", 10, 0, service.SearchFilters{IncludeOCR: true})
	require.NoError(t, err)
	require.Equal(t, "a3", results[0].ID)
}

// TestSmartSearchBelowCutTiering verifies the adaptive cut placement in a
// SmartSearch integration scenario: OCR hits always stay in the best tier
// (excluded from the cut computation), and the textbook-cliff tail of
// semantic hits (the same spec §2 "fish" example: 4 real hits 0.86~0.66,
// then a cliff down to 0.13 noise) is correctly marked BelowCut.
func TestSmartSearchBelowCutTiering(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "cut.db"))
	require.NoError(t, err)
	defer db.Close()

	// OCR hit: exact text substring match, always score 1.0, always the best tier.
	_, err = db.Exec(`INSERT INTO assets(id,file_path,status,is_live_photo_video,taken_at)
		VALUES('ocr1','/p/ocr1.jpg','indexed',0,'2025-07-01 10:00:00')`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO asset_ocr(asset_id,text) VALUES('ocr1','fish menu special')`)
	require.NoError(t, err)

	// seedSemantic inverts displayScore's linear mapping (default floor=0.03/
	// ceil=0.13) to construct a CLIP vector that produces exactly the target
	// display score ds (the query vector is fixed at e0=[1,0,...]).
	seedSemantic := func(id string, ds float64) {
		x := 0.03 + ds*0.10
		_, err := db.Exec(`INSERT INTO assets(id,file_path,status,is_live_photo_video) VALUES(?,?,'indexed',0)`, id, "/p/"+id+".jpg")
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO asset_clip_idx(asset_id) VALUES(?)`, id)
		require.NoError(t, err)
		var rowid int64
		require.NoError(t, db.QueryRow(`SELECT rowid FROM asset_clip_idx WHERE asset_id=?`, id).Scan(&rowid))
		vec := make([]float32, common.CLIPDim)
		vec[0] = float32(x)
		vec[1] = float32(math.Sqrt(1 - x*x))
		_, err = db.Exec(`INSERT INTO clip_embeddings(rowid,embedding) VALUES(?,?)`, rowid, sqlite.SerializeFloat32(vec))
		require.NoError(t, err)
	}
	seedSemantic("s1", 0.86)
	seedSemantic("s2", 0.80)
	seedSemantic("s3", 0.72)
	seedSemantic("s4", 0.66)
	seedSemantic("tail1", 0.13)
	seedSemantic("tail2", 0.13)

	svc := service.NewSearchService(db, &mockTextML{})
	results, err := svc.SmartSearch("fish", 10, 0, service.SearchFilters{IncludeOCR: true})
	require.NoError(t, err)
	require.Len(t, results, 7)

	belowCut := map[string]bool{}
	matchedBy := map[string]string{}
	for _, a := range results {
		belowCut[a.ID] = a.BelowCut
		matchedBy[a.ID] = a.MatchedBy
	}
	require.Equal(t, "ocr", matchedBy["ocr1"])
	require.False(t, belowCut["ocr1"], "an OCR hit always stays in the best tier, excluded from the cut computation")
	for _, id := range []string{"s1", "s2", "s3", "s4"} {
		require.Equal(t, "semantic", matchedBy[id])
		require.False(t, belowCut[id], id+" should be in the best-match tier")
	}
	for _, id := range []string{"tail1", "tail2"} {
		require.Equal(t, "semantic", matchedBy[id])
		require.True(t, belowCut[id], id+" should be folded into the more-results tier")
	}
}

// TestSmartSearchNoBelowCutWhenFewResults verifies the boundary guard: fewer
// than 3 semantic results never tier (all fall into the best-match tier),
// even with a large score gap.
func TestSmartSearchNoBelowCutWhenFewResults(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "few.db"))
	require.NoError(t, err)
	defer db.Close()

	seed := func(id string, x float64) {
		_, err := db.Exec(`INSERT INTO assets(id,file_path,status,is_live_photo_video) VALUES(?,?,'indexed',0)`, id, "/p/"+id+".jpg")
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO asset_clip_idx(asset_id) VALUES(?)`, id)
		require.NoError(t, err)
		var rowid int64
		require.NoError(t, db.QueryRow(`SELECT rowid FROM asset_clip_idx WHERE asset_id=?`, id).Scan(&rowid))
		vec := make([]float32, common.CLIPDim)
		vec[0] = float32(x)
		vec[1] = float32(math.Sqrt(1 - x*x))
		_, err = db.Exec(`INSERT INTO clip_embeddings(rowid,embedding) VALUES(?,?)`, rowid, sqlite.SerializeFloat32(vec))
		require.NoError(t, err)
	}
	seed("hi", 0.12)
	seed("lo", 0.031)

	svc := service.NewSearchService(db, &mockTextML{})
	results, err := svc.SmartSearch("fish", 10, 0, service.SearchFilters{})
	require.NoError(t, err)
	require.Len(t, results, 2)
	for _, a := range results {
		require.False(t, a.BelowCut, a.ID+": should not tier when semantic results < 3")
	}
}

// seedRankedSemantic inserts n semantic assets id0..id{n-1} in order,
// constructing strictly decreasing scores by inverting displayScore's linear
// mapping, so the KNN ordering is fixed (id0 has the highest score and sorts first).
func seedRankedSemantic(t *testing.T, db *sql.DB, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("id%d", i)
		ds := 0.95 - float64(i)*0.03 // strictly decreasing, with enough gap between entries to avoid cut/ordering ambiguity
		x := 0.03 + ds*0.10
		_, err := db.Exec(`INSERT INTO assets(id,file_path,status,is_live_photo_video) VALUES(?,?,'indexed',0)`, id, "/p/"+id+".jpg")
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO asset_clip_idx(asset_id) VALUES(?)`, id)
		require.NoError(t, err)
		var rowid int64
		require.NoError(t, db.QueryRow(`SELECT rowid FROM asset_clip_idx WHERE asset_id=?`, id).Scan(&rowid))
		vec := make([]float32, common.CLIPDim)
		vec[0] = float32(x)
		vec[1] = float32(math.Sqrt(1 - x*x))
		_, err = db.Exec(`INSERT INTO clip_embeddings(rowid,embedding) VALUES(?,?)`, rowid, sqlite.SerializeFloat32(vec))
		require.NoError(t, err)
	}
}

// TestSmartSearchOffsetPaginationMatchesFullList verifies page-slicing
// correctness: the content of an offset>0 page must equal the corresponding
// section of a "fetch the full list at once" result (sliced from the same
// KNN ordering).
func TestSmartSearchOffsetPaginationMatchesFullList(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "page.db"))
	require.NoError(t, err)
	defer db.Close()
	seedRankedSemantic(t, db, 12)

	svc := service.NewSearchService(db, &mockTextML{})
	full, err := svc.SmartSearch("fish", 12, 0, service.SearchFilters{})
	require.NoError(t, err)
	require.Len(t, full, 12)

	page, err := svc.SmartSearch("fish", 5, 5, service.SearchFilters{})
	require.NoError(t, err)
	require.Len(t, page, 5)
	for i, a := range page {
		require.Equal(t, full[5+i].ID, a.ID, "entry %d of page 2 should equal entry %d of the full list", i, 5+i)
	}
}

// TestSmartSearchOffsetPageAllBelowCutNoOCR verifies that when offset>0: OCR
// prepend-merging is skipped (even with IncludeOCR=true, a pure OCR hit does
// not appear on a deep page), and every result has BelowCut=true.
func TestSmartSearchOffsetPageAllBelowCutNoOCR(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "page_ocr.db"))
	require.NoError(t, err)
	defer db.Close()
	seedRankedSemantic(t, db, 8)
	// A pure OCR hit: text matches but has no CLIP vector, gets prepend-merged into the first page.
	_, err = db.Exec(`INSERT INTO assets(id,file_path,status,is_live_photo_video) VALUES('ocr1','/p/ocr1.jpg','indexed',0)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO asset_ocr(asset_id,text) VALUES('ocr1','fish menu special')`)
	require.NoError(t, err)

	svc := service.NewSearchService(db, &mockTextML{})

	// First page: the OCR hit should appear and always be in the best tier.
	first, err := svc.SmartSearch("fish", 3, 0, service.SearchFilters{IncludeOCR: true})
	require.NoError(t, err)
	var sawOCR bool
	for _, a := range first {
		if a.MatchedBy == "ocr" {
			sawOCR = true
			require.False(t, a.BelowCut)
		}
	}
	require.True(t, sawOCR, "the first page should include the OCR prepend hit")

	// Deep page: OCR merging is skipped, ocr1 should not appear again; every result has BelowCut=true.
	deep, err := svc.SmartSearch("fish", 3, 3, service.SearchFilters{IncludeOCR: true})
	require.NoError(t, err)
	require.NotEmpty(t, deep)
	for _, a := range deep {
		require.NotEqual(t, "ocr1", a.ID, "a deep page should not do OCR prepend-merging")
		require.Equal(t, "semantic", a.MatchedBy)
		require.True(t, a.BelowCut, a.ID+": every offset>0 result should be belowCut=true")
	}
}

// TestSmartSearchOCRDoesNotDisplaceSemanticAcrossPages reproduces the Critical
// pagination defect found in review: 8 semantic hits id0..id7 (strictly
// decreasing) + 1 pure OCR hit, limit=3.
//
// Old behavior (mergeOCRFirst truncated OCR+semantic total length to limit):
// first page [ocr1,id0,id1], id2 gets pushed out by OCR; the next page always
// slices the semantic ranking starting at offset=3, giving [id3,id4,id5] —
// id2 never appears on any page, permanently lost.
//
// New contract (OCR does not occupy a semantic slot): offset/limit apply only
// to the semantic sequence, so the first page = deduped OCR prepend +
// semantic[0:limit], whose total length may exceed limit; the first page
// should therefore be 4 entries [ocr1,id0,id1,id2], the next page still
// starts at id3, and the union of the semantic portions of both pages covers
// id0..id5 with no gap.
func TestSmartSearchOCRDoesNotDisplaceSemanticAcrossPages(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "ocr_page.db"))
	require.NoError(t, err)
	defer db.Close()
	seedRankedSemantic(t, db, 8)
	_, err = db.Exec(`INSERT INTO assets(id,file_path,status,is_live_photo_video) VALUES('ocr1','/p/ocr1.jpg','indexed',0)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO asset_ocr(asset_id,text) VALUES('ocr1','fish menu special')`)
	require.NoError(t, err)

	svc := service.NewSearchService(db, &mockTextML{})

	first, err := svc.SmartSearch("fish", 3, 0, service.SearchFilters{IncludeOCR: true})
	require.NoError(t, err)
	require.Len(t, first, 4, "the first page should be OCR prepend + semantic[0:limit], not truncated by OCR occupying a slot")
	firstIDs := make([]string, len(first))
	for i, a := range first {
		firstIDs[i] = a.ID
	}
	require.Equal(t, []string{"ocr1", "id0", "id1", "id2"}, firstIDs, "id2 should not be pushed out of the first page by OCR")

	second, err := svc.SmartSearch("fish", 3, 3, service.SearchFilters{IncludeOCR: true})
	require.NoError(t, err)
	require.Len(t, second, 3)
	secondIDs := make([]string, len(second))
	for i, a := range second {
		secondIDs[i] = a.ID
	}
	require.Equal(t, []string{"id3", "id4", "id5"}, secondIDs)

	// The union of both pages covers the semantic ranking id0..id5, with no
	// gap (id2 appeared on the first page, so it isn't lost to the cross-page cut).
	seen := map[string]bool{}
	for _, id := range firstIDs {
		seen[id] = true
	}
	for _, id := range secondIDs {
		seen[id] = true
	}
	for _, id := range []string{"id0", "id1", "id2", "id3", "id4", "id5"} {
		require.True(t, seen[id], id+" should not be missing from the union of the first and second pages")
	}
}

// TestSmartSearchOffsetBeyondLibraryReturnsActualCount verifies that when the
// library falls short (offset+limit exceeds the library size), the actual
// count is returned rather than an error or a padded empty result.
func TestSmartSearchOffsetBeyondLibraryReturnsActualCount(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "short.db"))
	require.NoError(t, err)
	defer db.Close()
	seedRankedSemantic(t, db, 4)

	svc := service.NewSearchService(db, &mockTextML{})

	// offset is within the library size, but offset+limit exceeds it: only the remaining entries should come back.
	partial, err := svc.SmartSearch("fish", 10, 3, service.SearchFilters{})
	require.NoError(t, err)
	require.Len(t, partial, 1)
	require.Equal(t, "id3", partial[0].ID)

	// offset itself already exceeds the library size: returns an empty slice, no error.
	empty, err := svc.SmartSearch("fish", 10, 100, service.SearchFilters{})
	require.NoError(t, err)
	require.Empty(t, empty)
}

// markDeleted soft-deletes the given id (sets deleted_at), simulating an
// asset moved to the trash — its CLIP vector stays in clip_embeddings
// (unlike a hard delete, which calls dropClipVector), so KNN still picks it
// as a candidate, but SmartSearch's WHERE deleted_at IS NULL filters it out
// after KNN.
func markDeleted(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	_, err := db.Exec(`UPDATE assets SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?`, id)
	require.NoError(t, err)
}

// TestSmartSearchOffsetFullPageDespiteDeletedInWindow reproduces the Critical
// defect from a production acceptance report: an asset moved to the trash
// (deleted_at set) mixed into the KNN window, which, without over-fetching to
// pad the count, would leave the filtered window one entry short and
// misalign pagination. Constructs 87 assets with vectors (id0..id86, ranked
// by strictly decreasing score), marking id25 — which falls exactly in the
// offset=20,limit=10 window — as deleted. Before the fix, k=offset+limit=30,
// and id25 gets filtered out of the 30 KNN candidates, leaving only 9; after
// the fix, over-fetching should pad the page to a full 10 entries matching
// the corresponding section of the "full list of live assets".
func TestSmartSearchOffsetFullPageDespiteDeletedInWindow(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "del_window.db"))
	require.NoError(t, err)
	defer db.Close()
	seedRankedSemantic(t, db, 87)
	markDeleted(t, db, "id25") // falls within the offset=20,limit=10 window [20,30)

	svc := service.NewSearchService(db, &mockTextML{})

	// The full list of live assets (fetched at once with a large limit, as the baseline for the paginated result).
	full, err := svc.SmartSearch("fish", 200, 0, service.SearchFilters{})
	require.NoError(t, err)
	require.Len(t, full, 86, "87 minus 1 deleted should leave 86 alive")
	for _, a := range full {
		require.NotEqual(t, "id25", a.ID, "a deleted asset should not appear in the live list")
	}

	page, err := svc.SmartSearch("fish", 10, 20, service.SearchFilters{})
	require.NoError(t, err)
	require.Len(t, page, 10, "1 deleted asset mixed into the window should not shrink the page below limit")
	for i, a := range page {
		require.Equal(t, full[20+i].ID, a.ID, "entry %d should equal entry %d of the full live list", i, 20+i)
	}
}

// TestSmartSearchOffsetUnionNoGapsOrDupesWithDeletedMixedIn verifies that
// when the library has several deleted assets scattered through it, the
// union of consecutive pages (offset advancing by +limit each time) exactly
// matches the full live list — no gaps (a live asset missing from every
// page) and no duplicates (the same entry appearing on two pages).
func TestSmartSearchOffsetUnionNoGapsOrDupesWithDeletedMixedIn(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "del_union.db"))
	require.NoError(t, err)
	defer db.Close()
	seedRankedSemantic(t, db, 60)
	for _, id := range []string{"id3", "id4", "id17", "id18", "id19", "id41"} {
		markDeleted(t, db, id)
	}

	svc := service.NewSearchService(db, &mockTextML{})
	full, err := svc.SmartSearch("fish", 200, 0, service.SearchFilters{})
	require.NoError(t, err)
	require.Len(t, full, 54)

	const pageSize = 10
	var union []service.Asset
	for offset := 0; offset < len(full); offset += pageSize {
		page, err := svc.SmartSearch("fish", pageSize, offset, service.SearchFilters{})
		require.NoError(t, err)
		require.Len(t, page, min(pageSize, len(full)-offset), "each page should be filled as much as possible, only the last page can be short")
		union = append(union, page...)
	}
	require.Len(t, union, len(full), "the union across pages should equal the full live list length (no gaps, no duplicates)")
	for i, a := range union {
		require.Equal(t, full[i].ID, a.ID, "union entry %d should equal entry %d of the full live list", i, i)
	}
}

// TestSmartSearchOffsetTrueBottomReturnsActualCountWithDeletedMixedIn verifies
// that even when the window has deleted assets requiring over-fetch padding,
// once padding hits the global k cap and still falls short of offset+limit,
// that means the true bottom has been reached — the actual remaining count
// should be returned rather than retrying pointlessly or erroring out.
func TestSmartSearchOffsetTrueBottomReturnsActualCountWithDeletedMixedIn(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "del_bottom.db"))
	require.NoError(t, err)
	defer db.Close()
	seedRankedSemantic(t, db, 10)
	markDeleted(t, db, "id8")
	markDeleted(t, db, "id9")
	// Live: id0..id7, 8 total.

	svc := service.NewSearchService(db, &mockTextML{})
	page, err := svc.SmartSearch("fish", 10, 5, service.SearchFilters{})
	require.NoError(t, err)
	require.Len(t, page, 3, "with 8 alive and offset=5, the true bottom should leave only 3")
	require.Equal(t, []string{"id5", "id6", "id7"}, []string{page[0].ID, page[1].ID, page[2].ID})

	empty, err := svc.SmartSearch("fish", 10, 8, service.SearchFilters{})
	require.NoError(t, err)
	require.Empty(t, empty, "offset already past the live total should return an empty slice, not an error")
}

// TestSmartSearchNegativeOffsetClampsToZero verifies the service layer's
// defensive clamping of a negative offset to zero (the route layer already
// clamps it; this makes sure SmartSearch itself won't misbehave on a
// negative offset either, e.g. a slice out-of-bounds or an undersized k).
func TestSmartSearchNegativeOffsetClampsToZero(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "negoff.db"))
	require.NoError(t, err)
	defer db.Close()
	seedRankedSemantic(t, db, 5)

	svc := service.NewSearchService(db, &mockTextML{})
	withNeg, err := svc.SmartSearch("fish", 5, -3, service.SearchFilters{})
	require.NoError(t, err)
	withZero, err := svc.SmartSearch("fish", 5, 0, service.SearchFilters{})
	require.NoError(t, err)
	require.Equal(t, len(withZero), len(withNeg))
	for i := range withZero {
		require.Equal(t, withZero[i].ID, withNeg[i].ID)
	}
}

func TestTimeline(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "tl.db"))
	require.NoError(t, err)
	defer db.Close()

	db.Exec(`INSERT INTO assets(id,file_path,status,taken_at,is_live_photo_video) VALUES('a1','/p1.jpg','indexed','2025-07-15 10:00:00',0)`)
	db.Exec(`INSERT INTO assets(id,file_path,status,taken_at,is_live_photo_video) VALUES('v1','/p1.mov','indexed','2025-07-15 10:00:00',1)`)

	svc := service.NewSearchService(db, nil)
	groups, err := svc.Timeline("default")
	require.NoError(t, err)
	require.Len(t, groups, 1)
	require.Equal(t, 2025, groups[0].Year)
	require.Equal(t, 7, groups[0].Month)
	require.Len(t, groups[0].Assets, 1, "live photo video must be hidden")
}

// TestTimelineEnrichesPlaceName verifies Timeline/ListAssets surface a city-level
// PlaceName ("City, Country") from asset_geo, so the client filters by city rather
// than falling back to a coordinate-derived country.
func TestTimelineEnrichesPlaceName(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "place.db"))
	require.NoError(t, err)
	defer db.Close()

	db.Exec(`INSERT INTO assets(id,file_path,status,taken_at,is_live_photo_video) VALUES('a1','/p1.jpg','indexed','2025-07-15 10:00:00',0)`)
	db.Exec(`INSERT INTO assets(id,file_path,status,taken_at,is_live_photo_video) VALUES('a2','/p2.jpg','indexed','2025-07-15 11:00:00',0)`)
	// a1 is geocoded to Tokyo, Japan; a2 has no geo row → PlaceName stays empty.
	db.Exec(`INSERT INTO asset_geo(asset_id,city_id,city,country,region,lat,lon) VALUES('a1',1850147,'Tokyo','Japan','asia',35.68,139.69)`)

	svc := service.NewSearchService(db, nil)
	groups, err := svc.Timeline("default")
	require.NoError(t, err)
	got := map[string]string{}
	for _, g := range groups {
		for _, a := range g.Assets {
			got[a.ID] = a.PlaceName
		}
	}
	require.Equal(t, "Tokyo, Japan", got["a1"])
	require.Equal(t, "", got["a2"])

	assets, err := svc.ListAssets("default", 10, 0)
	require.NoError(t, err)
	for _, a := range assets {
		if a.ID == "a1" {
			require.Equal(t, "Tokyo, Japan", a.PlaceName)
		}
	}
}

// TestSmartSearchExcludesOffline verifies that when an asset's removable
// drive is unplugged (offline=1), it should not appear in semantic search
// results even with a CLIP vector — as if it temporarily doesn't exist.
func TestSmartSearchExcludesOffline(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "so.db"))
	require.NoError(t, err)
	defer db.Close()

	seed := func(id, path string) {
		_, err := db.Exec(`INSERT INTO assets(id, file_path, status, is_live_photo_video) VALUES(?,?,'indexed',0)`, id, path)
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO asset_clip_idx(asset_id) VALUES(?)`, id)
		require.NoError(t, err)
		var rowid int64
		require.NoError(t, db.QueryRow(`SELECT rowid FROM asset_clip_idx WHERE asset_id=?`, id).Scan(&rowid))
		vec := make([]float32, common.CLIPDim)
		vec[0] = 1.0
		_, err = db.Exec(`INSERT INTO clip_embeddings(rowid, embedding) VALUES(?,?)`, rowid, sqlite.SerializeFloat32(vec))
		require.NoError(t, err)
	}
	seed("online", "/DATA/beach.jpg")
	seed("offline", "/media/X/beach2.jpg")
	_, err = db.Exec(`UPDATE assets SET offline=1 WHERE id='offline'`)
	require.NoError(t, err)

	svc := service.NewSearchService(db, &mockTextML{})
	results, err := svc.SmartSearch("beach", 10, 0, service.SearchFilters{})
	require.NoError(t, err)
	ids := map[string]bool{}
	for _, a := range results {
		ids[a.ID] = true
	}
	require.True(t, ids["online"], "an online asset should appear in the results")
	require.False(t, ids["offline"], "an offline asset must be hidden from semantic search results")
}

// TestTimelineExcludesOffline verifies the timeline view hides offline=1 assets.
func TestTimelineExcludesOffline(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "tlo.db"))
	require.NoError(t, err)
	defer db.Close()

	db.Exec(`INSERT INTO assets(id,file_path,status,taken_at,is_live_photo_video) VALUES('online','/p1.jpg','indexed','2025-07-15 10:00:00',0)`)
	db.Exec(`INSERT INTO assets(id,file_path,status,taken_at,is_live_photo_video) VALUES('offline','/media/X/p2.jpg','indexed','2025-07-15 11:00:00',0)`)
	_, err = db.Exec(`UPDATE assets SET offline=1 WHERE id='offline'`)
	require.NoError(t, err)

	svc := service.NewSearchService(db, nil)
	groups, err := svc.Timeline("default")
	require.NoError(t, err)
	require.Len(t, groups, 1)
	require.Len(t, groups[0].Assets, 1, "an offline asset must be hidden from Timeline")
	require.Equal(t, "online", groups[0].Assets[0].ID)
}

// TestListAssetsExcludesOffline verifies ListAssets hides offline=1 assets.
func TestListAssetsExcludesOffline(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "lao.db"))
	require.NoError(t, err)
	defer db.Close()

	db.Exec(`INSERT INTO assets(id,file_path,status,is_live_photo_video) VALUES('online','/p1.jpg','indexed',0)`)
	db.Exec(`INSERT INTO assets(id,file_path,status,is_live_photo_video) VALUES('offline','/media/X/p2.jpg','indexed',0)`)
	_, err = db.Exec(`UPDATE assets SET offline=1 WHERE id='offline'`)
	require.NoError(t, err)

	svc := service.NewSearchService(db, nil)
	assets, err := svc.ListAssets("default", 10, 0)
	require.NoError(t, err)
	require.Len(t, assets, 1)
	require.Equal(t, "online", assets[0].ID)
}

// TestPersonAssetsExcludesOffline verifies the person detail page's asset list hides offline assets.
func TestPersonAssetsExcludesOffline(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "pao.db"))
	require.NoError(t, err)
	defer db.Close()

	db.Exec(`INSERT INTO assets(id,file_path,status,is_live_photo_video) VALUES('online','/p1.jpg','indexed',0)`)
	db.Exec(`INSERT INTO assets(id,file_path,status,is_live_photo_video) VALUES('offline','/media/X/p2.jpg','indexed',0)`)
	_, err = db.Exec(`UPDATE assets SET offline=1 WHERE id='offline'`)
	require.NoError(t, err)
	db.Exec(`INSERT INTO persons(id,name) VALUES('per1','Alice')`)
	for _, aid := range []string{"online", "offline"} {
		db.Exec(`INSERT INTO face_detections(id,asset_id,bbox,embedding) VALUES(?,?,'{}',?)`, "fd-"+aid, aid, []byte{})
		db.Exec(`INSERT INTO face_person(face_id,person_id) VALUES(?,?)`, "fd-"+aid, "per1")
	}

	svc := service.NewSearchService(db, nil)
	assets, err := svc.PersonAssets("per1", 10, 0)
	require.NoError(t, err)
	require.Len(t, assets, 1)
	require.Equal(t, "online", assets[0].ID)
}

func TestListAssets(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "la.db"))
	require.NoError(t, err)
	defer db.Close()

	for i := 0; i < 3; i++ {
		db.Exec(fmt.Sprintf(`INSERT INTO assets(id,file_path,status,is_live_photo_video) VALUES('id%d','/p%d.jpg','indexed',0)`, i, i))
	}

	svc := service.NewSearchService(db, nil)
	assets, err := svc.ListAssets("default", 10, 0)
	require.NoError(t, err)
	require.Len(t, assets, 3)
}

func TestGetAssetReturnsImageMetadata(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "img.db"))
	require.NoError(t, err)
	defer db.Close()

	_, err = db.Exec(`INSERT INTO assets(id, file_path, status, mime_type) VALUES('img1','/tmp/x.jpg','indexed','image/jpeg')`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO asset_exif(asset_id, width, height, iso, aperture, make, focal_length, orientation)
		VALUES('img1', 4000, 3000, 800, 1.8, 'Apple', 35.0, 1)`)
	require.NoError(t, err)

	svc := service.NewSearchService(db, nil)
	a, err := svc.GetAsset("default", "img1")
	require.NoError(t, err)
	require.Equal(t, 4000, a.Width)
	require.Equal(t, 3000, a.Height)
	require.Equal(t, 800, a.ISO)
	require.InDelta(t, 1.8, a.Aperture, 1e-6)
	require.Equal(t, "Apple", a.Make)
	require.InDelta(t, 35.0, a.FocalLength, 1e-6)
	require.Equal(t, 1, a.Orientation)
}

func TestGetAssetReturnsVideoMetadata(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "vid.db"))
	require.NoError(t, err)
	defer db.Close()

	_, err = db.Exec(`INSERT INTO assets(id, file_path, status, mime_type) VALUES('vid1','/tmp/x.mp4','indexed','video/mp4')`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO asset_exif(asset_id, width, height, video_codec, audio_codec, frame_rate, bit_rate, rotation, latitude, longitude)
		VALUES('vid1', 1920, 1080, 'h264', 'aac', 29.97, 12000000, 90, 39.9, 116.4)`)
	require.NoError(t, err)

	svc := service.NewSearchService(db, nil)
	a, err := svc.GetAsset("default", "vid1")
	require.NoError(t, err)
	require.Equal(t, 1920, a.Width)
	require.Equal(t, 1080, a.Height)
	require.Equal(t, "h264", a.VideoCodec)
	require.Equal(t, "aac", a.AudioCodec)
	require.InDelta(t, 29.97, a.FrameRate, 1e-3)
	require.Equal(t, int64(12000000), a.BitRate)
	require.Equal(t, 90, a.Rotation)
	require.InDelta(t, 39.9, a.Latitude, 1e-6)
	require.InDelta(t, 116.4, a.Longitude, 1e-6)
}

func TestGetAssetWithoutExifRow(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "bare.db"))
	require.NoError(t, err)
	defer db.Close()

	_, err = db.Exec(`INSERT INTO assets(id, file_path, status) VALUES('bare','/tmp/y.jpg','indexed')`)
	require.NoError(t, err)

	svc := service.NewSearchService(db, nil)
	a, err := svc.GetAsset("default", "bare")
	require.NoError(t, err)
	require.Equal(t, "bare", a.ID)
	require.Equal(t, 0, a.Width)
}

// TestListAssetsByPlaceKey verifies the place_key filter returns only that city's photos.
// TestListAssetsBySpotKey verifies the spot_key filter precisely narrows to photos within a grid cell.
func TestListAssetsByPlaceAndSpotKey(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "geo_filter.db"))
	require.NoError(t, err)
	defer db.Close()

	gaz, err := geo.Load()
	require.NoError(t, err)
	geoSvc := service.NewGeoService(db, gaz)

	// seed helper: inserts asset + exif, and reverse-geocodes into asset_geo
	seed := func(id string, lat, lon float64) {
		_, err := db.Exec(
			`INSERT INTO assets(id,file_path,status,taken_at,is_live_photo_video)
			 VALUES(?,?,'indexed','2026-01-01 00:00:00',0)`,
			id, "/x/"+id,
		)
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO asset_exif(asset_id,latitude,longitude) VALUES(?,?,?)`, id, lat, lon)
		require.NoError(t, err)
		require.NoError(t, geoSvc.GeocodeAsset(id))
	}

	// 2 photos in Tokyo: the same grid coordinate (35.6895, 139.6917)
	seed("tok1", 35.6895, 139.6917)
	seed("tok2", 35.6895, 139.6917)
	// 1 photo in New York
	seed("nyc1", 40.71, -74.00)

	// Get Tokyo's city_id (via PlacesService.ListPlaces, looking up City=="Tokyo")
	placesSvc := service.NewPlacesService(db, gaz, geoSvc)
	resp, err := placesSvc.ListPlaces()
	require.NoError(t, err)

	var tokyoCityID int32
	for _, p := range resp.Places {
		if p.City == "Tokyo" {
			tokyoCityID = p.Key
			break
		}
	}
	require.NotZero(t, tokyoCityID, "Tokyo must appear in ListPlaces")

	searchSvc := service.NewSearchService(db, nil)

	// ── Test place_key filter ─────────────────────────────────
	assets, err := searchSvc.ListAssets("default", 50, 0,
		service.AssetFilter{PlaceKey: tokyoCityID})
	require.NoError(t, err)
	require.Len(t, assets, 2, "place_key filter must return only Tokyo photos")
	for _, a := range assets {
		require.Contains(t, []string{"tok1", "tok2"}, a.ID)
	}

	// ── Test spot_key filter ──────────────────────────────────
	// spot_key format: cityID:int(lat/0.01):int(lon/0.01)
	tokLat, tokLon := 35.6895, 139.6917
	gx := int(tokLat / 0.01) // 3568
	gy := int(tokLon / 0.01) // 13969
	spotKey := fmt.Sprintf("%d:%d:%d", tokyoCityID, gx, gy)

	assets, err = searchSvc.ListAssets("default", 50, 0,
		service.AssetFilter{SpotKey: spotKey})
	require.NoError(t, err)
	require.Len(t, assets, 2, "spot_key filter must return only the 2 Tokyo photos in that grid cell")
	for _, a := range assets {
		require.Contains(t, []string{"tok1", "tok2"}, a.ID)
	}

	// ── New York place_key filter ─────────────────────────────────
	var nycCityID int32
	for _, p := range resp.Places {
		if p.City != "Tokyo" {
			nycCityID = p.Key
			break
		}
	}
	require.NotZero(t, nycCityID)
	assets, err = searchSvc.ListAssets("default", 50, 0,
		service.AssetFilter{PlaceKey: nycCityID})
	require.NoError(t, err)
	require.Len(t, assets, 1)
	require.Equal(t, "nyc1", assets[0].ID)
}

func TestUpdateDurationMs(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "t.db"))
	require.NoError(t, err)
	defer db.Close()
	_, err = db.Exec(`INSERT INTO assets(id, file_path, mime_type, duration_ms, status)
		VALUES('v1','/v/v1.mp4','video/mp4',0,'indexed')`)
	require.NoError(t, err)

	s := service.NewSearchService(db, nil) // ml may be nil (only non-CLIP methods are used)
	require.NoError(t, s.UpdateDurationMs("v1", 62000))

	var got int64
	require.NoError(t, db.QueryRow(`SELECT duration_ms FROM assets WHERE id='v1'`).Scan(&got))
	require.Equal(t, int64(62000), got)
}

// TestOCRLinesMatchAndAll verifies the service method backing
// GET /assets/:id/ocr: with a query, filters lines using the same rule as
// ocrSearch (case-insensitive substring); without one, returns all lines
// (reserved for Live Text); line order is by line_no; a missing asset gives ErrNotFound.
func TestOCRLinesMatchAndAll(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "ocr.db"))
	require.NoError(t, err)
	defer db.Close()
	s := service.NewSearchService(db, &mockTextML{})

	_, err = db.Exec(`INSERT INTO assets(id, file_path, mime_type, status) VALUES
		('a1', '/g/a1.jpg', 'image/jpeg', 'indexed')`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO asset_ocr_lines(asset_id, line_no, text, box, score) VALUES
		('a1', 0, 'XX公司发票代开', '[0.1,0.1,0.9,0.1,0.9,0.2,0.1,0.2]', 0.98),
		('a1', 1, 'TOTAL $42.00',   '[0.1,0.3,0.5,0.3,0.5,0.4,0.1,0.4]', 0.95)`)
	require.NoError(t, err)

	// Hit filter: Chinese substring.
	hits, err := s.OCRLines("a1", "发票")
	require.NoError(t, err)
	require.Len(t, hits, 1)
	require.Equal(t, "XX公司发票代开", hits[0].Text)
	require.Equal(t, []float64{0.1, 0.1, 0.9, 0.1, 0.9, 0.2, 0.1, 0.2}, hits[0].Box)

	// Case-insensitive (same as ocrSearch's instr(lower,lower)).
	hits, err = s.OCRLines("a1", "total")
	require.NoError(t, err)
	require.Len(t, hits, 1)

	// No query → all lines, ordered by line_no.
	all, err := s.OCRLines("a1", "")
	require.NoError(t, err)
	require.Len(t, all, 2)
	require.Equal(t, "XX公司发票代开", all[0].Text)

	// No hit → empty slice (handler JSON guarantees non-nil semantics).
	none, err := s.OCRLines("a1", "不存在的词")
	require.NoError(t, err)
	require.Len(t, none, 0)

	// Asset does not exist → ErrNotFound.
	_, err = s.OCRLines("ghost", "x")
	require.ErrorIs(t, err, service.ErrNotFound)

	// Soft-deleted asset (deleted_at non-null) is treated as not existing → ErrNotFound.
	_, err = db.Exec(`UPDATE assets SET deleted_at = CURRENT_TIMESTAMP WHERE id='a1'`)
	require.NoError(t, err)
	_, err = s.OCRLines("a1", "发票")
	require.ErrorIs(t, err, service.ErrNotFound)
}

// TestDeleteAsset_TriggersCaptionDelete: after a hard delete (DeleteAsset, the
// call site right next to dropClipVector) succeeds, it should invoke the
// callback injected via SetCaptionDelete with the correct assetID (Task 4
// caption cascade: prevents the agent from retrieving ghost results for a deleted photo).
func TestDeleteAsset_TriggersCaptionDelete(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "delcap.db"))
	require.NoError(t, err)
	defer db.Close()

	db.Exec(`INSERT INTO assets(id, file_path, status, is_live_photo_video) VALUES('a1','/p/beach.jpg','indexed',0)`)

	svc := service.NewSearchService(db, &mockTextML{})

	var mu sync.Mutex
	var got []string
	svc.SetCaptionDelete(func(id string) {
		mu.Lock()
		got = append(got, id)
		mu.Unlock()
	})

	require.NoError(t, svc.DeleteAsset("a1"))

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"a1"}, got)
}
