package service

import (
	"bytes"
	"image"
	"os"
	"path/filepath"
)

// maxMLInputPixels is the safe pixel cap on the image input fed to
// immich-ml (/predict). PIL inside the immich-ml container has its own
// decompression-bomb protection, with a hard cap of 178,956,970 pixels
// (Pillow's default Image.MAX_IMAGE_PIXELS, roughly 0.25 * 2^31); above
// that, PIL throws directly and /predict necessarily returns 500. This is
// a threshold set a bit below the hard cap: once the original exceeds it,
// face detection/OCR input is automatically degraded to a thumbnail,
// preventing a genuinely oversized image (e.g. a 16320x12240=199.8MP
// high-resolution photo that has appeared in the library) from permanently
// jamming those two ML pipelines — OCR getting a 500 and being swallowed
// on every indexing pass, face_scanned never getting set so RunPipeline
// retries the same image forever.
// Not made configurable: this is a workaround for a third-party hard
// limit, no need for it to be user-tunable (YAGNI).
const maxMLInputPixels = 170_000_000

// pixelsExceedMLLimit is the pure-function part of the predicate, doing
// only a multiply comparison, so boundary values can be unit tested
// directly without constructing image bytes.
func pixelsExceedMLLimit(width, height int) bool {
	return int64(width)*int64(height) > maxMLInputPixels
}

// oversizedForML only reads the image bytes' header (image.DecodeConfig, no
// full decode) to get width/height, and checks whether it exceeds
// maxMLInputPixels.
// When data isn't a format any registered decoder recognizes
// (DecodeConfig errors), it's treated as not exceeding the limit,
// preserving the original behavior — pass the data through to ML as-is and
// let ML decide whether to error.
func oversizedForML(data []byte) bool {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return false
	}
	return pixelsExceedMLLimit(cfg.Width, cfg.Height)
}

// readLargeOrSmallThumb reads <thumbDir>/<id>/large.jpg, falling back to
// small.jpg when that's unavailable; returns nil if neither is available.
// Used to provide a degraded input to face detection/OCR when the original
// exceeds maxMLInputPixels (following the same large→small thumbnail
// fallback strategy already used by video face detection/CLIP backfill).
func readLargeOrSmallThumb(thumbDir, id string) []byte {
	if b, err := os.ReadFile(filepath.Join(thumbDir, id, "large.jpg")); err == nil && len(b) > 0 {
		return b
	}
	if b, err := os.ReadFile(filepath.Join(thumbDir, id, "small.jpg")); err == nil && len(b) > 0 {
		return b
	}
	return nil
}
