// Shared curation: the candidate member sets produced by the trip/theme
// engines rely on the unified burst dedup + featured/cover selection logic
// here to decide "which photos are worth showing" — the engines themselves
// only care about "does this belong to this moment", not "should it be
// pushed to the front".
package service

import (
	"context"
	"database/sql"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// clipVecLoader returns the CLIP image vector for an asset_id; ok=false means
// no vector (not embedded, or ScenesEnabled off). PickFeaturedAndCover skips
// burst dedup for such assets and puts them straight into the featured
// candidate pool (with no vector there's no way to tell "is this the same
// burst shot", so it's better to skip dedup than risk wrongly dropping one).
type clipVecLoader func(assetID string) ([]float32, bool)

// RealClipVecLoader is the production implementation of clipVecLoader,
// wrapping readClipVector (see docverdict.go:14-24). Tests inject a fake
// instead of using this.
func RealClipVecLoader(db *sql.DB) clipVecLoader {
	return func(assetID string) ([]float32, bool) {
		v := readClipVector(db, assetID)
		return v, v != nil
	}
}

// coverImageLoader returns the thumbnail the cover brightness gate should
// check for an asset_id; ok=false means it couldn't be read (thumbnail
// missing/decode failure) — pickCover skips the brightness gate for such a
// candidate and accepts it outright (better to let through an image that
// can't be verified than to leave a whole recipe without a cover just
// because a thumbnail couldn't be read).
type coverImageLoader func(assetID string) (image.Image, bool)

// RealCoverImageLoader is the production implementation of coverImageLoader:
// reads <thumbDir>/<assetID>/small.jpg (the same on-disk path convention as
// pkg/thumb.Generate) and decodes it to an image.Image. A missing
// file/decode failure is treated as ok=false.
func RealCoverImageLoader(thumbDir string) coverImageLoader {
	return func(assetID string) (image.Image, bool) {
		f, err := os.Open(filepath.Join(thumbDir, assetID, "small.jpg"))
		if err != nil {
			return nil, false
		}
		defer f.Close()
		img, err := jpeg.Decode(f)
		if err != nil {
			return nil, false
		}
		return img, true
	}
}

// Cover brightness/contrast hard-gate thresholds — the probe aesthetic
// head's chosen covers are occasionally too dark/overexposed/hazy-low-
// contrast (confirmed by real-device screenshots); this is a transitional
// patch until the AVA alignment head ships, and the thresholds don't go into
// the recipe. Once the AVA head replaces it, pickCover and the brightness-
// gate helper functions below it can be retired wholesale.
const (
	coverMinMeanLuma = 0.12 // grayscale mean below this is judged too dark
	coverMaxMeanLuma = 0.92 // grayscale mean above this is judged overexposed
	coverMinStdDev   = 0.05 // grayscale std dev below this is judged hazy/low-contrast
)

// passesCoverBrightnessGate computes img's grayscale mean/std dev (range
// [0,1]) to decide whether it's fit to be a cover: too dark, overexposed, or
// hazy low-contrast all fail.
func passesCoverBrightnessGate(img image.Image) bool {
	mean, stddev := grayscaleMeanStdDev(img)
	if mean < coverMinMeanLuma || mean > coverMaxMeanLuma {
		return false
	}
	if stddev < coverMinStdDev {
		return false
	}
	return true
}

// grayscaleMeanStdDev iterates all of img's pixels converted to grayscale,
// returning the mean and std dev (both normalized to [0,1]). The thumbnail is
// small.jpg (250px wide), a small pixel count, so no extra downsampling is
// needed.
func grayscaleMeanStdDev(img image.Image) (mean, stddev float64) {
	bounds := img.Bounds()
	var sum, sumSq, n float64
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			g := color.GrayModel.Convert(img.At(x, y)).(color.Gray).Y
			v := float64(g) / 255.0
			sum += v
			sumSq += v * v
			n++
		}
	}
	if n == 0 {
		return 0, 0
	}
	mean = sum / n
	variance := sumSq/n - mean*mean
	if variance < 0 {
		variance = 0 // guard against floating-point error
	}
	return mean, math.Sqrt(variance)
}

// pickCover picks a cover from featured (the featured members, already sorted
// by aesthetic_score and truncated to maxFeatured): re-sorts the candidate
// order by MomentAsset.Score descending, then aesthetic_score descending on
// ties (Score is the CLIP topic-similarity score for theme/pet moments, so
// Your Beagle's cover ends up picking the photo "most like a dog"; for trip
// moments Score is always 0, which naturally degrades to pure aesthetic
// order, i.e. the existing behavior). Runs each candidate through the
// brightness/contrast gate in order, using the first one that passes as the
// cover; a candidate loadCover can't read (ok=false) is accepted outright,
// unconstrained by the gate; if all are rejected (or there's no usable
// loader), falls back to featured[0] (the highest score under aesthetic
// order), so a gate failure never results in "no cover".
func pickCover(featured []string, assets []MomentAsset, info map[string]pickAssetInfo, loadCover coverImageLoader) string {
	if len(featured) == 0 {
		return ""
	}
	if loadCover == nil {
		return featured[0]
	}

	scoreByID := make(map[string]float64, len(assets))
	for _, a := range assets {
		scoreByID[a.AssetID] = a.Score
	}

	ordered := append([]string(nil), featured...)
	sort.SliceStable(ordered, func(i, j int) bool {
		si, sj := scoreByID[ordered[i]], scoreByID[ordered[j]]
		if si != sj {
			return si > sj
		}
		ai, aj := aestheticOf(info, ordered[i]), aestheticOf(info, ordered[j])
		if ai != aj {
			return ai > aj
		}
		return ordered[i] < ordered[j]
	})

	for _, id := range ordered {
		img, ok := loadCover(id)
		if !ok {
			return id
		}
		if passesCoverBrightnessGate(img) {
			return id
		}
	}
	return featured[0]
}

// burstWindowSeconds is the time window for burst clustering: two adjacent
// photos can only possibly belong to the same burst cluster if their taken
// time gap is <= this value (vector similarity within the cluster then
// decides whether it's really a burst).
const burstWindowSeconds = 60

// burstCosineThreshold is the cosine similarity threshold for burst dedup:
// two photos in the same time cluster are judged a burst (consecutive
// shutter presses of the same scene) once their CLIP vector cosine
// similarity > this value; only the highest-aesthetic-score photo within the
// cluster enters the featured candidate pool.
const burstCosineThreshold = 0.95

// pickAssetInfo is the minimal per-asset information PickFeaturedAndCover
// needs during its computation.
type pickAssetInfo struct {
	takenAt      time.Time
	aesthetic    float64
	hasAesthetic bool
}

// PickFeaturedAndCover is the curation shared by the trip/theme engines:
// clusters assets by a 60s adjacent taken-time window → within a cluster,
// pairwise CLIP vector cosine similarity > 0.95 judges a burst (only the
// highest aesthetic_score in a burst group enters the featured candidate
// pool; the rest remain members of the moment, just not part of featured/not
// a cover candidate) → the candidate pool is sorted by aesthetic_score
// descending and the top maxFeatured become featured. The cover is then
// picked from the featured candidates by re-sorting on MomentAsset.Score
// (the CLIP topic-similarity score, always 0 for trip moments) descending +
// aesthetic_score descending, passed through pickCover's brightness/contrast
// gate (a transitional patch, see the pickCover comment). Assets with no
// vector skip the dedup step and go straight into the candidate pool.
//
// Note: aesthetic_score is the probe head model's output, used only for
// relative ranking within the same batch of candidates (who is "more worth
// showing" than whom), not an absolute quality score — never use it for an
// absolute threshold judgment across batches/moments (a rule like "below 0.4
// can't be a cover" would be wrong).
func PickFeaturedAndCover(ctx context.Context, db *sql.DB, assets []MomentAsset, maxFeatured int, loadVec clipVecLoader, loadCover coverImageLoader) ([]string, string, error) {
	if len(assets) == 0 {
		return nil, "", nil
	}

	info, err := loadPickAssetInfo(ctx, db, assets)
	if err != nil {
		return nil, "", err
	}

	ordered := make([]string, len(assets))
	for i, a := range assets {
		ordered[i] = a.AssetID
	}
	// Sort by taken_at ascending; assets with no taken_at go last (SliceStable
	// keeps their relative original order) — with no time anchor there's no
	// way to tell "is this adjacent", so in clusterByTimeWindow each becomes its
	// own cluster.
	sort.SliceStable(ordered, func(i, j int) bool {
		hi, hj := !info[ordered[i]].takenAt.IsZero(), !info[ordered[j]].takenAt.IsZero()
		if hi != hj {
			return hi
		}
		if hi && hj {
			return info[ordered[i]].takenAt.Before(info[ordered[j]].takenAt)
		}
		return false
	})

	var poolIDs []string
	for _, cluster := range clusterByTimeWindow(ordered, info) {
		for _, group := range groupByCosine(cluster, loadVec) {
			poolIDs = append(poolIDs, bestByAestheticInGroup(group, info))
		}
	}
	if len(poolIDs) == 0 {
		return nil, "", nil
	}

	// Candidate pool sorted by aesthetic_score descending; ties broken by id to
	// guarantee a deterministic, reproducible result.
	sort.SliceStable(poolIDs, func(i, j int) bool {
		si, sj := aestheticOf(info, poolIDs[i]), aestheticOf(info, poolIDs[j])
		if si != sj {
			return si > sj
		}
		return poolIDs[i] < poolIDs[j]
	})

	featured := poolIDs
	if maxFeatured >= 0 && maxFeatured < len(featured) {
		featured = featured[:maxFeatured]
	}
	cover := pickCover(featured, assets, info, loadCover)
	return featured, cover, nil
}

// clusterByTimeWindow takes the ordered sequence (already sorted by taken_at
// ascending, with no-taken_at assets last) and chain-clusters it wherever
// adjacent points are <= burstWindowSeconds apart: the algorithm mirrors
// moments_trip.go's splitByGap, except the window unit here is seconds and
// the criterion is "close enough → merge into the same cluster" rather than
// "far enough → cut a new segment". Assets with no taken_at each become their
// own cluster — with no time anchor, there's no way to tell it's adjacent to
// anything.
func clusterByTimeWindow(ordered []string, info map[string]pickAssetInfo) [][]string {
	var clusters [][]string
	var cur []string
	var prev time.Time
	window := time.Duration(burstWindowSeconds) * time.Second

	flush := func() {
		if len(cur) > 0 {
			clusters = append(clusters, cur)
			cur = nil
		}
	}

	for _, id := range ordered {
		t := info[id].takenAt
		if t.IsZero() {
			flush()
			clusters = append(clusters, []string{id})
			continue
		}
		if len(cur) > 0 && t.Sub(prev) <= window {
			cur = append(cur, id)
		} else {
			flush()
			cur = []string{id}
		}
		prev = t
	}
	flush()
	return clusters
}

// groupByCosine, within a single time cluster, uses a union-find to merge
// assets whose pairwise cosine similarity > the threshold into a burst group
// (allowing chained transitivity: if A~B and B~C both match even though A~C
// doesn't, A/B/C are still one group — consistent with the intuition of a
// "burst": across a run of consecutive shutter presses, the first and last
// frame's composition may already differ noticeably). Assets with no vector
// each become their own group (skipping dedup, going straight into the pool).
func groupByCosine(cluster []string, loadVec clipVecLoader) [][]string {
	if len(cluster) <= 1 {
		groups := make([][]string, len(cluster))
		for i, id := range cluster {
			groups[i] = []string{id}
		}
		return groups
	}

	type item struct {
		id  string
		vec []float32
	}
	var vecItems []item
	var groups [][]string
	for _, id := range cluster {
		v, ok := loadVec(id)
		if !ok {
			groups = append(groups, []string{id})
			continue
		}
		vecItems = append(vecItems, item{id, v})
	}

	n := len(vecItems)
	parent := make([]int, n)
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(x int) int {
		for parent[x] != x {
			parent[x] = parent[parent[x]]
			x = parent[x]
		}
		return x
	}
	union := func(a, b int) {
		ra, rb := find(a), find(b)
		if ra != rb {
			parent[ra] = rb
		}
	}

	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			sim := 1 - cosDist(vecItems[i].vec, vecItems[j].vec)
			if sim > burstCosineThreshold {
				union(i, j)
			}
		}
	}

	byRoot := map[int][]string{}
	var order []int
	for i := 0; i < n; i++ {
		r := find(i)
		if _, ok := byRoot[r]; !ok {
			order = append(order, r)
		}
		byRoot[r] = append(byRoot[r], vecItems[i].id)
	}
	for _, r := range order {
		groups = append(groups, byRoot[r])
	}
	return groups
}

// bestByAestheticInGroup returns the highest aesthetic_score in the group;
// an asset with no aesthetic score is treated as the lowest (-1), and ties
// are broken by id to guarantee determinism.
func bestByAestheticInGroup(group []string, info map[string]pickAssetInfo) string {
	best := group[0]
	bestScore := aestheticOf(info, best)
	for _, id := range group[1:] {
		s := aestheticOf(info, id)
		if s > bestScore || (s == bestScore && id < best) {
			best, bestScore = id, s
		}
	}
	return best
}

// aestheticOf returns an asset's aesthetic_score; an asset never scored
// (NULL) is treated as -1, naturally sorting after any scored asset.
func aestheticOf(info map[string]pickAssetInfo, id string) float64 {
	v := info[id]
	if !v.hasAesthetic {
		return -1
	}
	return v.aesthetic
}

// loadPickAssetInfo batch-queries assets.taken_at/aesthetic_score (chunked
// to 500 per batch to stay under the SQLite variable limit, same approach as
// places.go's bestByAesthetic).
func loadPickAssetInfo(ctx context.Context, db *sql.DB, assets []MomentAsset) (map[string]pickAssetInfo, error) {
	out := make(map[string]pickAssetInfo, len(assets))
	ids := make([]string, len(assets))
	for i, a := range assets {
		ids[i] = a.AssetID
	}

	const chunkSize = 500
	for start := 0; start < len(ids); start += chunkSize {
		end := start + chunkSize
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[start:end]

		ph := strings.Repeat("?,", len(chunk)-1) + "?"
		args := make([]interface{}, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}

		rows, err := db.QueryContext(ctx, `
			SELECT id, taken_at, aesthetic_score FROM assets WHERE id IN (`+ph+`)`, args...)
		if err != nil {
			return nil, fmt.Errorf("moments: pick load asset info: %w", err)
		}
		for rows.Next() {
			var id string
			var ts sql.NullString
			var score sql.NullFloat64
			if err := rows.Scan(&id, &ts, &score); err != nil {
				rows.Close()
				return nil, fmt.Errorf("moments: scan pick asset info: %w", err)
			}
			var info pickAssetInfo
			if t := parseSQLiteTime(ts); t != nil {
				info.takenAt = *t
			}
			if score.Valid {
				info.aesthetic = score.Float64
				info.hasAesthetic = true
			}
			out[id] = info
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("moments: iterate pick asset info: %w", err)
		}
		rows.Close()
	}
	return out, nil
}
