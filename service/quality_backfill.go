package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/disintegration/imaging"
	"go.uber.org/zap"
)

const (
	// sharpnessBackfillBatchSize is how many face_detections rows are
	// processed before the loop pauses (sharpnessBackfillBatchSleep) — a
	// crop decode + resize + variance pass per row, kept small so a cold
	// start doesn't turn into a long uninterruptible IO/CPU burst.
	sharpnessBackfillBatchSize = 50

	// sharpnessBackfillBatchSleep is the pause between batches.
	sharpnessBackfillBatchSleep = 200 * time.Millisecond

	// sharpnessCropSize: the raw bbox crop is resized to this before the
	// Laplacian variance is computed. mlserver scores sharpness on a
	// 112x112 landmark-aligned crop (server/facemodel.py sharpness_from_crop
	// feeds norm_crop's 112x112 output); resizing our plain bbox crop to
	// the same target size can't reproduce the alignment, but it does keep
	// the two distributions in the same ballpark for a signal that's only
	// ever used for relative ranking (cover selection), not an absolute
	// threshold.
	sharpnessCropSize = 112

	// sharpnessK is the Laplacian-variance half-point used to squash the
	// raw variance into [0,1) via v/(v+sharpnessK), where the variance is
	// computed on 8-bit-per-channel luma (0-255 scale, matching cv2's
	// grayscale — see grayLaplacianVariance's descale comment). This
	// constant MUST stay equal to mlserver's SHARPNESS_K
	// (mlserver/server/facemodel.py) — that Python constant is the single
	// source of truth; if it ever changes, update this one too so
	// legacy-backfilled and ML-computed sharpness values stay on the same
	// scale.
	sharpnessK = 100.0

	// sharpnessBackfillMarkerFile is the one-shot marker written into the
	// markerDir passed to BackfillSharpness. Mirrors the
	// .clip_reembed_thumb_v1.done pattern in embedder.go's
	// reembedThumbnailsOnce: presence of the file means "never run again";
	// delete it to force a re-run.
	sharpnessBackfillMarkerFile = ".face_sharpness_backfill_v1.done"
)

// SharpnessBackfillStartupDelay is how long main() should wait after process
// startup before kicking off BackfillSharpness, so this one-shot legacy
// backfill doesn't compete with the cold-start indexing/detection burst.
const SharpnessBackfillStartupDelay = 3 * time.Minute

// legacySharpnessRow is one face_detections row missing a sharpness score,
// joined with enough of its asset to resolve the same source image
// detectFaceScanTarget would have used.
type legacySharpnessRow struct {
	faceID   string
	assetID  string
	bboxJSON string
	filePath string
	isVideo  bool
}

// queryLegacySharpnessRows loads every face_detections row with
// sharpness IS NULL, up front, into memory. BackfillSharpness then walks
// this fixed snapshot in batches: since the snapshot doesn't shrink as rows
// get updated, a row that gets permanently skipped (unreadable source) is
// visited exactly once, never re-queried into an infinite loop.
func (s *FaceService) queryLegacySharpnessRows(ctx context.Context) ([]legacySharpnessRow, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT fd.id, fd.asset_id, fd.bbox, a.file_path, COALESCE(a.mime_type,'') LIKE 'video/%'
FROM face_detections fd
JOIN assets a ON a.id = fd.asset_id
WHERE fd.sharpness IS NULL AND a.deleted_at IS NULL AND a.offline = 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []legacySharpnessRow
	for rows.Next() {
		var r legacySharpnessRow
		if err := rows.Scan(&r.faceID, &r.assetID, &r.bboxJSON, &r.filePath, &r.isVideo); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// BackfillSharpness computes the sharpness signal for legacy face_detections
// rows (sharpness IS NULL) via pure-Go Laplacian variance on the stored
// bbox crop of the source image — no ML round-trip. Frontality is
// deliberately left untouched: it needs the 5-point landmarks, which were
// never stored for these rows, so it stays NULL and quality-neutral (see
// faceQualityFactor in persons.go).
//
// One-shot: guarded by a marker file in markerDir (mirrors
// reembedThumbnailsOnce's .clip_reembed_thumb_v1.done in embedder.go); a
// second call is a no-op. Rows are loaded once up front and then walked in
// batches of sharpnessBackfillBatchSize with a short sleep between batches,
// so a cold start doesn't saturate IO. Source files that are missing or
// fail to decode are skipped permanently (left NULL) rather than retried —
// the marker is written at the end regardless of how many were skipped —
// and the skipped count is logged once.
func (s *FaceService) BackfillSharpness(ctx context.Context, markerDir string) error {
	marker := filepath.Join(markerDir, sharpnessBackfillMarkerFile)
	if _, err := os.Stat(marker); err == nil {
		return nil // already done
	}

	targets, err := s.queryLegacySharpnessRows(ctx)
	if err != nil {
		return err
	}

	var updated, skipped int
	for i := 0; i < len(targets); i += sharpnessBackfillBatchSize {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		end := i + sharpnessBackfillBatchSize
		if end > len(targets) {
			end = len(targets)
		}
		for _, r := range targets[i:end] {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			v, ok := s.legacyFaceSharpness(r)
			if !ok {
				skipped++
				continue
			}
			if _, err := s.db.ExecContext(ctx, `UPDATE face_detections SET sharpness=? WHERE id=?`, v, r.faceID); err != nil {
				zap.L().Warn("sharpness backfill: failed to write score",
					zap.String("face_id", r.faceID), zap.Error(err))
				skipped++
				continue
			}
			updated++
		}
		if end < len(targets) {
			time.Sleep(sharpnessBackfillBatchSleep)
		}
	}

	zap.L().Info("legacy sharpness backfill complete",
		zap.Int("updated", updated), zap.Int("skipped_unreadable", skipped), zap.Int("total", len(targets)))
	if err := os.WriteFile(marker, []byte(fmt.Sprintf("updated=%d skipped=%d\n", updated, skipped)), 0o644); err != nil {
		zap.L().Warn("failed to write sharpness backfill marker", zap.Error(err))
	}
	return nil
}

// legacyFaceSharpness computes the sharpness score for one legacy face row:
// it loads the same source image detectFaceScanTarget would have used
// (resolveFaceScanSource, in mlinput.go — video assets read the
// thumbs/<id>/large.jpg keyframe), crops the stored bbox out of it, resizes
// the crop to sharpnessCropSize x sharpnessCropSize, and runs
// grayLaplacianVariance + squashSharpness on the result. ok=false whenever
// the source is missing/unreadable, undecodable, or the bbox is degenerate
// — callers must treat that as a permanent skip (never retried, since the
// row stays out of the marker-guarded backfill's scope once this pass ends).
func (s *FaceService) legacyFaceSharpness(r legacySharpnessRow) (score float64, ok bool) {
	data, err := resolveFaceScanSource(s.thumbDir, r.assetID, r.filePath, r.isVideo)
	if err != nil {
		return 0, false
	}
	img, err := imaging.Decode(bytes.NewReader(data))
	if err != nil {
		return 0, false
	}

	var bb struct {
		X1 float64 `json:"x1"`
		Y1 float64 `json:"y1"`
		X2 float64 `json:"x2"`
		Y2 float64 `json:"y2"`
	}
	if err := json.Unmarshal([]byte(r.bboxJSON), &bb); err != nil {
		return 0, false
	}
	rect := image.Rect(int(bb.X1), int(bb.Y1), int(math.Ceil(bb.X2)), int(math.Ceil(bb.Y2))).Intersect(img.Bounds())
	if rect.Dx() <= 0 || rect.Dy() <= 0 {
		return 0, false
	}

	cropped := imaging.Crop(img, rect)
	resized := imaging.Resize(cropped, sharpnessCropSize, sharpnessCropSize, imaging.Lanczos)
	v := grayLaplacianVariance(resized, resized.Bounds())
	return squashSharpness(v), true
}

// grayLaplacianVariance computes the variance of the 3x3 Laplacian response
// ({0,1,0; 1,-4,1; 0,1,0}) over img's luma (0.299 R + 0.587 G + 0.114 B) on
// the standard 8-bit-per-channel scale (0-255) — matching cv2's grayscale
// scale in mlserver/server/facemodel.py, which is what sharpnessK is
// calibrated against. img.At().RGBA() returns 16-bit-range components
// (0-65535, ~257x the 8-bit value), so each channel is right-shifted by 8
// before the luma weights are applied; skipping this descale would inflate
// the variance by ~257^2 (~66000x) and saturate squashSharpness to ~1.0 for
// virtually every real crop, destroying the signal. Evaluated only at
// interior points of rect (the kernel needs a full 3x3 neighborhood, so
// pixels on rect's own border are excluded). Returns 0 when rect has no
// interior point (either dimension under 3px) — a degenerate case that
// never occurs for a sharpnessCropSize-square resize, but keeps this pure
// function well defined for arbitrary input.
func grayLaplacianVariance(img image.Image, rect image.Rectangle) float64 {
	rect = rect.Intersect(img.Bounds())
	if rect.Dx() < 3 || rect.Dy() < 3 {
		return 0
	}
	luma := func(x, y int) float64 {
		r, g, b, _ := img.At(x, y).RGBA()
		// Descale 16-bit (0-65535) to 8-bit (0-255) to match cv2's grayscale
		// scale — see the doc comment above.
		return 0.299*float64(r>>8) + 0.587*float64(g>>8) + 0.114*float64(b>>8)
	}

	var sum, sumSq float64
	var n int
	for y := rect.Min.Y + 1; y < rect.Max.Y-1; y++ {
		for x := rect.Min.X + 1; x < rect.Max.X-1; x++ {
			lap := luma(x, y-1) + luma(x, y+1) + luma(x-1, y) + luma(x+1, y) - 4*luma(x, y)
			sum += lap
			sumSq += lap * lap
			n++
		}
	}
	if n == 0 {
		return 0
	}
	mean := sum / float64(n)
	return sumSq/float64(n) - mean*mean
}

// squashSharpness maps a Laplacian variance (theoretically unbounded, >=0)
// into [0,1) via v/(v+sharpnessK) — see the sharpnessK doc comment for the
// mlserver cross-reference. Exactly 0 at v<=0, strictly monotonic increasing,
// asymptotic to (never reaching) 1.
func squashSharpness(v float64) float64 {
	if v < 0 {
		v = 0
	}
	return v / (v + sharpnessK)
}
