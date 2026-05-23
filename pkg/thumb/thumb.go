// Package thumb generates JPEG thumbnails for photos and videos.
// It produces two variants per asset:
//   - small.jpg  — 250 px wide (aspect-ratio preserved)
//   - large.jpg  — original width, or capped at 1280 px if the source is wider
package thumb

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/disintegration/imaging"
)

const (
	smallWidth = 250
	largeWidth = 1280
)

// Generate creates thumbnail files for the image at srcPath. The output files
// are written to outDir/<assetID>/small.jpg and outDir/<assetID>/large.jpg.
// It returns the absolute paths of small and large thumbnails, or an error.
// generateViaFFmpeg uses ffmpeg to decode srcPath into a temporary JPEG, then
// calls Generate again on that JPEG. Used as fallback for formats that the
// imaging library cannot decode (e.g. WebP, corrupt-but-recoverable JPEGs).
func generateViaFFmpeg(srcPath, assetID, outDir string) (smallPath, largePath string, err error) {
	assetDir := filepath.Join(outDir, assetID)
	if err := os.MkdirAll(assetDir, 0o755); err != nil {
		return "", "", fmt.Errorf("thumb.generateViaFFmpeg: mkdir: %w", err)
	}
	tmp := filepath.Join(assetDir, "_decoded.jpg")
	cmd := exec.Command("ffmpeg", "-i", srcPath, "-vframes", "1", "-q:v", "2", "-update", "1", "-y", tmp)
	if out, cmdErr := cmd.CombinedOutput(); cmdErr != nil {
		return "", "", fmt.Errorf("thumb.generateViaFFmpeg: ffmpeg: %w — %s", cmdErr, string(out))
	}
	defer os.Remove(tmp)
	return Generate(tmp, assetID, outDir)
}

func Generate(srcPath, assetID, outDir string) (smallPath, largePath string, err error) {
	// open source image with automatic EXIF orientation correction
	img, err := imaging.Open(srcPath, imaging.AutoOrientation(true))
	if err != nil {
		// imaging doesn't support all formats (e.g. WebP); fall back to ffmpeg
		return generateViaFFmpeg(srcPath, assetID, outDir)
	}

	// create output directory
	assetDir := filepath.Join(outDir, assetID)
	if err := os.MkdirAll(assetDir, 0o755); err != nil {
		return "", "", fmt.Errorf("thumb.Generate: mkdir %q: %w", assetDir, err)
	}

	// ---- small thumbnail (250 px wide, proportional height) ----
	small := imaging.Resize(img, smallWidth, 0, imaging.Lanczos)
	smallPath = filepath.Join(assetDir, "small.jpg")
	if err := imaging.Save(small, smallPath); err != nil {
		return "", "", fmt.Errorf("thumb.Generate: save small: %w", err)
	}

	// ---- large variant (cap at 1280 px wide if necessary) ----
	largePath = filepath.Join(assetDir, "large.jpg")
	srcWidth := img.Bounds().Dx()
	if srcWidth > largeWidth {
		large := imaging.Resize(img, largeWidth, 0, imaging.Lanczos)
		if err := imaging.Save(large, largePath); err != nil {
			return "", "", fmt.Errorf("thumb.Generate: save large (resized): %w", err)
		}
	} else {
		// source is already ≤ 1280 px wide — save the original as large
		if err := imaging.Save(img, largePath); err != nil {
			return "", "", fmt.Errorf("thumb.Generate: save large (original): %w", err)
		}
	}

	return smallPath, largePath, nil
}
