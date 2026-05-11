// Package thumb generates JPEG thumbnails for photos and videos.
// It produces two variants per asset:
//   - small.jpg  — 250 px wide (aspect-ratio preserved)
//   - large.jpg  — original width, or capped at 1280 px if the source is wider
package thumb

import (
	"fmt"
	"os"
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
func Generate(srcPath, assetID, outDir string) (smallPath, largePath string, err error) {
	// open source image with automatic EXIF orientation correction
	img, err := imaging.Open(srcPath, imaging.AutoOrientation(true))
	if err != nil {
		return "", "", fmt.Errorf("thumb.Generate: open %q: %w", srcPath, err)
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
