package thumb_test

import (
	"bytes"
	"image"
	"image/jpeg"
	"os"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/pkg/thumb"
	"github.com/stretchr/testify/require"
)

// makeTestJPEG creates a temporary JPEG file with the given dimensions and
// returns its path. The file is placed inside t.TempDir() and cleaned up
// automatically when the test ends.
func makeTestJPEG(t *testing.T, w, h int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	var buf bytes.Buffer
	jpeg.Encode(&buf, img, nil)
	f, _ := os.CreateTemp(t.TempDir(), "*.jpg")
	f.Write(buf.Bytes())
	f.Close()
	return f.Name()
}

// TestGenerateSmall verifies that:
//   - a 1000×800 source produces a small thumbnail 250 px wide, and
//   - the large variant is capped at 1280 px wide (source > 1280).
func TestGenerateSmall(t *testing.T) {
	src := makeTestJPEG(t, 1000, 800)
	smallPath, largePath, err := thumb.Generate(src, "asset-1", t.TempDir())
	require.NoError(t, err)

	// small: 250 px wide
	f, err := os.Open(smallPath)
	require.NoError(t, err)
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	require.NoError(t, err)
	require.Equal(t, 250, cfg.Width)

	// large: source (1000 px) is ≤ 1280 so large == original width
	f2, err := os.Open(largePath)
	require.NoError(t, err)
	defer f2.Close()
	cfg2, _, err := image.DecodeConfig(f2)
	require.NoError(t, err)
	require.Equal(t, 1000, cfg2.Width)
}

// TestGenerateLargeCapped verifies that a wide source (> 1280 px) is resized
// to exactly 1280 px in the large variant.
func TestGenerateLargeCapped(t *testing.T) {
	src := makeTestJPEG(t, 2000, 1500)
	_, largePath, err := thumb.Generate(src, "asset-wide", t.TempDir())
	require.NoError(t, err)

	f, err := os.Open(largePath)
	require.NoError(t, err)
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	require.NoError(t, err)
	require.Equal(t, 1280, cfg.Width)
}

// TestGeneratePreservesAspectRatio verifies that a 400×800 portrait image is
// resized proportionally: small should be 250 wide → 500 tall.
func TestGeneratePreservesAspectRatio(t *testing.T) {
	src := makeTestJPEG(t, 400, 800) // portrait
	smallPath, _, err := thumb.Generate(src, "asset-2", t.TempDir())
	require.NoError(t, err)

	f, err := os.Open(smallPath)
	require.NoError(t, err)
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	require.NoError(t, err)
	require.Equal(t, 250, cfg.Width)
	require.Equal(t, 500, cfg.Height)
}

// TestGenerateNonexistent verifies that Generate returns an error when the
// source file does not exist.
func TestGenerateNonexistent(t *testing.T) {
	_, _, err := thumb.Generate("/nonexistent/source.jpg", "asset-x", t.TempDir())
	require.Error(t, err)
}
