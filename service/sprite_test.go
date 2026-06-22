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
	require.Equal(t, 5, service.SpriteFrameCount(0))          // 下限
	require.Equal(t, 5, service.SpriteFrameCount(30_000))     // 30s → 1 → 钳到 5
	require.Equal(t, 10, service.SpriteFrameCount(300_000))   // 5min → 10
	require.Equal(t, 20, service.SpriteFrameCount(600_000))   // 10min → 20
	require.Equal(t, 20, service.SpriteFrameCount(7_200_000)) // 2h → 钳到 20
}

func TestEnsureSkipsWhenExists(t *testing.T) {
	g := service.NewSpriteGenerator()
	dir := t.TempDir()
	out := filepath.Join(dir, "sprite.jpg")
	require.NoError(t, writeFakeJPEG(t, out, 10*service.SpriteFrameW, service.SpriteFrameH))
	// 文件已存在 → 不调用 ffmpeg，srcPath 给假路径也应成功
	fc, err := g.Ensure("/does/not/matter.mp4", out, 300_000)
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
