package service_test

import (
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/stretchr/testify/require"
)

func TestSpriteFrameCountClamp(t *testing.T) {
	// Tiered density reduction: ≤30s→1 frame/s, 30–120s→1 frame/2s, >120s→1 frame/4s; clamped to [5, 120]
	require.Equal(t, 5, service.SpriteFrameCount(0))           // lower bound
	require.Equal(t, 5, service.SpriteFrameCount(3_000))       // 3s → 3 → clamped to 5
	require.Equal(t, 5, service.SpriteFrameCount(5_000))       // 5s → 5
	require.Equal(t, 10, service.SpriteFrameCount(10_000))     // 10s → 10
	require.Equal(t, 30, service.SpriteFrameCount(30_000))     // 30s → 30 (tier 1 upper bound)
	require.Equal(t, 15, service.SpriteFrameCount(31_000))     // 31s → 31/2 → 15 (enters tier 2)
	require.Equal(t, 30, service.SpriteFrameCount(60_000))     // 60s → 30
	require.Equal(t, 60, service.SpriteFrameCount(120_000))    // 120s → 60 (tier 2 upper bound)
	require.Equal(t, 30, service.SpriteFrameCount(121_000))    // 121s → 121/4 → 30 (enters tier 3)
	require.Equal(t, 75, service.SpriteFrameCount(300_000))    // 5min → 75
	require.Equal(t, 120, service.SpriteFrameCount(600_000))   // 10min → 150 → clamped to 120
	require.Equal(t, 120, service.SpriteFrameCount(7_200_000)) // 2h → clamped to 120
}

func TestEnsureSkipsWhenExists(t *testing.T) {
	g := service.NewSpriteGenerator()
	dir := t.TempDir()
	out := filepath.Join(dir, "sprite.jpg")
	require.NoError(t, writeFakeJPEG(t, out, 10*service.SpriteFrameW, service.SpriteFrameH))
	// File already exists → ffmpeg isn't called, so a fake srcPath should still succeed; 10s → 1 frame/s → 10 frames (matches the file)
	fc, err := g.Ensure("/does/not/matter.mp4", out, 10_000)
	require.NoError(t, err)
	require.Equal(t, 10, fc)
}

func TestSpriteFrameCountFromFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.jpg")
	require.NoError(t, writeFakeJPEG(t, p, 7*service.SpriteFrameW, service.SpriteFrameH))
	n, err := service.SpriteFrameCountFromFile(p)
	require.NoError(t, err)
	require.Equal(t, 7, n)
}

func TestSpriteInfoFromFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.jpg")
	// Portrait: a single frame is 240×427, 6 frames → sprite 1440×427; the height read back from the file is the original frame height
	require.NoError(t, writeFakeJPEG(t, p, 6*service.SpriteFrameW, 427))
	n, h, err := service.SpriteInfoFromFile(p)
	require.NoError(t, err)
	require.Equal(t, 6, n)
	require.Equal(t, 427, h)
}

func writeFakeJPEG(t *testing.T, path string, w, h int) error {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return jpeg.Encode(f, img, nil)
}
