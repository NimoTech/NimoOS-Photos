// Tests for shared curation: covers the Step 1 brief checklist — an in-memory
// DB seeded with aesthetic_score + an injectable, controllable vector reader;
// asserts that a burst (vector cosine > 0.95 within a 60s window) keeps only
// the highest score, featured count is truncated by maxFeatured, cover is the
// highest-score candidate, and an asset with no vector skips dedup and goes
// straight into the pool.
package service

import (
	"context"
	"database/sql"
	"image"
	"image/color"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// noCoverLoader simulates "thumbnail can't be read" (same naming convention
// as moments_test.go's noVecLoader), for test cases that don't care about the
// brightness gate and only want to verify featured/sort logic — when the
// loader returns false, pickCover accepts that candidate outright, equivalent
// to skipping the brightness gate.
func noCoverLoader(_ string) (image.Image, bool) { return nil, false }

// fakeCoverLoader looks up images from a preset map; an asset not in the map
// is treated as "unreadable".
func fakeCoverLoader(imgs map[string]image.Image) coverImageLoader {
	return func(assetID string) (image.Image, bool) {
		img, ok := imgs[assetID]
		return img, ok
	}
}

// solidGrayImage builds a w x h image whose grayscale value is uniformly gray
// (0-255), used for the "too dark/overexposed/low-contrast hazy" three
// candidate cases in the brightness gate tests.
func solidGrayImage(w, h int, gray uint8) image.Image {
	img := image.NewGray(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetGray(x, y, color.Gray{Y: gray})
		}
	}
	return img
}

// checkerImage builds a black-and-white checkerboard image (mean ~0.5, high
// std dev), representing a "normal photo" with normal brightness/contrast
// that passes the gate.
func checkerImage(w, h int) image.Image {
	img := image.NewGray(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if (x+y)%2 == 0 {
				img.SetGray(x, y, color.Gray{Y: 0})
			} else {
				img.SetGray(x, y, color.Gray{Y: 255})
			}
		}
	}
	return img
}

// insertPickAsset inserts an asset with taken_at + aesthetic_score, for use
// by PickFeaturedAndCover tests.
func insertPickAsset(t *testing.T, db *sql.DB, id string, takenAt time.Time, aesthetic float64) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO assets(id, file_path, status, taken_at, aesthetic_score) VALUES(?,?,'indexed',?,?)`,
		id, "/g/"+id+".jpg", takenAt.UTC().Format("2006-01-02 15:04:05"), aesthetic)
	require.NoError(t, err)
}

// fakeVecLoader looks up vectors from a preset map; an asset not in the map
// is treated as "no vector".
func fakeVecLoader(vecs map[string][]float32) clipVecLoader {
	return func(assetID string) ([]float32, bool) {
		v, ok := vecs[assetID]
		return v, ok
	}
}

func TestPickFeaturedAndCover_BurstDedupAndVectorlessSkip(t *testing.T) {
	db := makeTestDB(t)

	t0 := time.Date(2011, time.April, 1, 10, 0, 0, 0, time.UTC)

	// Cluster A (one burst within the 60s window): b1/b2 have highly similar
	// vectors (>0.95), judged a burst, keeping only b2 (the higher aesthetic
	// score); b3's vector is orthogonal, not in the same group as b1/b2, enters
	// the pool independently.
	insertPickAsset(t, db, "b1", t0, 0.9)
	insertPickAsset(t, db, "b2", t0.Add(10*time.Second), 0.95)
	insertPickAsset(t, db, "b3", t0.Add(20*time.Second), 0.5)

	// Cluster B (far more than 60s from cluster A, so it forms its own
	// cluster): d1 has no vector, e1 has a vector; even though the two are
	// adjacent to each other (within 10s), d1 must skip the dedup step and go
	// straight into the pool because it has no vector, not merging with e1.
	t1 := t0.Add(2000 * time.Second)
	insertPickAsset(t, db, "d1", t1, 0.4)
	insertPickAsset(t, db, "e1", t1.Add(10*time.Second), 0.6)

	// Cluster C: an isolated single photo, the highest aesthetic score overall,
	// should become the cover.
	t2 := t1.Add(2000 * time.Second)
	insertPickAsset(t, db, "c1", t2, 0.99)

	vecs := map[string][]float32{
		"b1": {1, 0},
		"b2": {1, 0.1}, // ~0.995 cosine similarity with b1, > 0.95, judged a burst
		"b3": {0, 1},   // orthogonal to both b1/b2, not judged a burst
		"e1": {0.5, 0.5},
		// d1 and c1 are deliberately left out of the vector table: d1 tests "no
		// vector skips dedup and goes straight into the pool"; c1 is already the
		// only member of its cluster and would enter the pool alone regardless
		// of whether it has a vector, incidentally verifying that "missing a
		// vector" doesn't affect its selection.
	}

	assets := []MomentAsset{
		{AssetID: "b1"}, {AssetID: "b2"}, {AssetID: "b3"},
		{AssetID: "d1"}, {AssetID: "e1"}, {AssetID: "c1"},
	}

	loadVec := fakeVecLoader(vecs)

	// maxFeatured=3: the (deduped) candidate pool sorted by aesthetic_score
	// descending is c1(0.99) > b2(0.95) > e1(0.6) > b3(0.5) > d1(0.4), take the
	// top 3.
	featured, cover, err := PickFeaturedAndCover(context.Background(), db, assets, 3, loadVec, noCoverLoader)
	require.NoError(t, err)
	require.Equal(t, []string{"c1", "b2", "e1"}, featured)
	require.Equal(t, "c1", cover)

	// Untruncated (maxFeatured large enough): the full candidate pool should
	// be 5 entries, b1 removed by burst dedup.
	full, cover2, err := PickFeaturedAndCover(context.Background(), db, assets, 10, loadVec, noCoverLoader)
	require.NoError(t, err)
	require.Equal(t, []string{"c1", "b2", "e1", "b3", "d1"}, full)
	require.Equal(t, "c1", cover2)
	require.NotContains(t, full, "b1", "a non-highest-score member within a burst group should be excluded from the featured candidate pool")
}

func TestPickFeaturedAndCover_EmptyAssets(t *testing.T) {
	db := makeTestDB(t)
	featured, cover, err := PickFeaturedAndCover(context.Background(), db, nil, 12, fakeVecLoader(nil), noCoverLoader)
	require.NoError(t, err)
	require.Empty(t, featured)
	require.Empty(t, cover)
}

// TestPickFeaturedAndCover_CoverPrefersScore covers the theme/pet scenario:
// among the featured candidates, the one with the highest aesthetic score
// isn't the one with the highest MomentAsset.Score, and cover should follow
// score (the CLIP topic-similarity score), not aesthetic_score — exactly the
// requirement of "Your Beagle's cover picks the photo that looks most like a
// dog". Uses noCoverLoader throughout to skip the brightness gate, verifying
// only the ordering.
func TestPickFeaturedAndCover_CoverPrefersScore(t *testing.T) {
	db := makeTestDB(t)
	t0 := time.Date(2011, time.April, 1, 10, 0, 0, 0, time.UTC)
	// Three photos, none adjacent (gaps far exceed the 60s burst window), each
	// entering the pool independently.
	insertPickAsset(t, db, "hi-aesthetic", t0, 0.99)
	insertPickAsset(t, db, "hi-score", t0.Add(2000*time.Second), 0.5)
	insertPickAsset(t, db, "low-both", t0.Add(4000*time.Second), 0.1)

	assets := []MomentAsset{
		{AssetID: "hi-aesthetic", Score: 0.2},
		{AssetID: "hi-score", Score: 0.9}, // highest topic-similarity score overall
		{AssetID: "low-both", Score: 0.1},
	}

	featured, cover, err := PickFeaturedAndCover(context.Background(), db, assets, 10, noVecLoader, noCoverLoader)
	require.NoError(t, err)
	// featured logic is unchanged: still sorted by aesthetic_score descending.
	require.Equal(t, []string{"hi-aesthetic", "hi-score", "low-both"}, featured)
	// But cover follows score, not featured[0].
	require.Equal(t, "hi-score", cover)
}

// TestPickFeaturedAndCover_CoverSkipsDarkCandidate covers the brightness
// gate's "too dark" branch: the highest-score candidate's thumbnail is too
// dark and should be skipped, with cover falling to the next-highest-score
// candidate that passes the gate.
func TestPickFeaturedAndCover_CoverSkipsDarkCandidate(t *testing.T) {
	db := makeTestDB(t)
	t0 := time.Date(2011, time.April, 1, 10, 0, 0, 0, time.UTC)
	insertPickAsset(t, db, "dark-top", t0, 0.9)
	insertPickAsset(t, db, "normal-second", t0.Add(2000*time.Second), 0.5)

	assets := []MomentAsset{
		{AssetID: "dark-top", Score: 0.9},
		{AssetID: "normal-second", Score: 0.5},
	}
	imgs := map[string]image.Image{
		"dark-top":      solidGrayImage(4, 4, 5), // 5/255 ≈ 0.02, too dark
		"normal-second": checkerImage(4, 4),
	}

	featured, cover, err := PickFeaturedAndCover(context.Background(), db, assets, 10, noVecLoader, fakeCoverLoader(imgs))
	require.NoError(t, err)
	require.Equal(t, []string{"dark-top", "normal-second"}, featured, "featured ordering is unaffected by the brightness gate")
	require.Equal(t, "normal-second", cover, "a too-dark candidate should be skipped by the brightness gate")
}

// TestPickFeaturedAndCover_CoverSkipsLowContrastCandidate covers the "hazy/
// low-contrast" branch: the candidate thumbnail's brightness mean is normal
// but its std dev is too low (a near-solid-color flat image), should be
// skipped.
func TestPickFeaturedAndCover_CoverSkipsLowContrastCandidate(t *testing.T) {
	db := makeTestDB(t)
	t0 := time.Date(2011, time.April, 1, 10, 0, 0, 0, time.UTC)
	insertPickAsset(t, db, "foggy-top", t0, 0.9)
	insertPickAsset(t, db, "normal-second", t0.Add(2000*time.Second), 0.5)

	assets := []MomentAsset{
		{AssetID: "foggy-top", Score: 0.9},
		{AssetID: "normal-second", Score: 0.5},
	}
	imgs := map[string]image.Image{
		"foggy-top":     solidGrayImage(4, 4, 128), // mean 0.5 is normal, but a solid color has std dev=0
		"normal-second": checkerImage(4, 4),
	}

	featured, cover, err := PickFeaturedAndCover(context.Background(), db, assets, 10, noVecLoader, fakeCoverLoader(imgs))
	require.NoError(t, err)
	require.Equal(t, []string{"foggy-top", "normal-second"}, featured)
	require.Equal(t, "normal-second", cover, "a low-contrast hazy candidate should be skipped by the brightness gate")
}

// TestPickFeaturedAndCover_CoverAllRejectedFallsBackToFeaturedFirst covers
// the all-rejected fallback: when every candidate fails the brightness gate,
// cover should fall back to the original featured[0] (the highest score
// under aesthetic_score order), rather than "no cover".
func TestPickFeaturedAndCover_CoverAllRejectedFallsBackToFeaturedFirst(t *testing.T) {
	db := makeTestDB(t)
	t0 := time.Date(2011, time.April, 1, 10, 0, 0, 0, time.UTC)
	insertPickAsset(t, db, "a", t0, 0.9)
	insertPickAsset(t, db, "b", t0.Add(2000*time.Second), 0.5)

	assets := []MomentAsset{
		{AssetID: "a", Score: 0.5},
		{AssetID: "b", Score: 0.9},
	}
	imgs := map[string]image.Image{
		"a": solidGrayImage(4, 4, 5),   // too dark
		"b": solidGrayImage(4, 4, 250), // overexposed
	}

	featured, cover, err := PickFeaturedAndCover(context.Background(), db, assets, 10, noVecLoader, fakeCoverLoader(imgs))
	require.NoError(t, err)
	require.Equal(t, []string{"a", "b"}, featured, "featured[0] should be a, the highest under aesthetic order")
	require.Equal(t, "a", cover, "with every candidate rejected, it should fall back to featured[0]")
}

// TestPickFeaturedAndCover_CoverLoaderMissingParticipates covers the branch
// where the loader can't read a thumbnail: that candidate should skip the
// brightness gate and participate directly (rather than being excluded), and
// the highest-score candidate whose image can't be read still gets picked as
// cover.
func TestPickFeaturedAndCover_CoverLoaderMissingParticipates(t *testing.T) {
	db := makeTestDB(t)
	t0 := time.Date(2011, time.April, 1, 10, 0, 0, 0, time.UTC)
	insertPickAsset(t, db, "no-thumb-top", t0, 0.9)
	insertPickAsset(t, db, "normal-second", t0.Add(2000*time.Second), 0.5)

	assets := []MomentAsset{
		{AssetID: "no-thumb-top", Score: 0.9},
		{AssetID: "normal-second", Score: 0.5},
	}
	// no-thumb-top is deliberately left out of imgs: simulates a missing
	// thumbnail/decode failure.
	imgs := map[string]image.Image{
		"normal-second": checkerImage(4, 4),
	}

	featured, cover, err := PickFeaturedAndCover(context.Background(), db, assets, 10, noVecLoader, fakeCoverLoader(imgs))
	require.NoError(t, err)
	require.Equal(t, []string{"no-thumb-top", "normal-second"}, featured)
	require.Equal(t, "no-thumb-top", cover, "when the loader can't read it, the gate should be skipped and the candidate accepted outright")
}
