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
	// 分档降密度：≤30s→1帧/s，30–120s→1帧/2s，>120s→1帧/4s；钳制 [5, 120]
	require.Equal(t, 5, service.SpriteFrameCount(0))           // 下限
	require.Equal(t, 5, service.SpriteFrameCount(3_000))       // 3s → 3 → 钳到 5
	require.Equal(t, 5, service.SpriteFrameCount(5_000))       // 5s → 5
	require.Equal(t, 10, service.SpriteFrameCount(10_000))     // 10s → 10
	require.Equal(t, 30, service.SpriteFrameCount(30_000))     // 30s → 30（档一上界）
	require.Equal(t, 15, service.SpriteFrameCount(31_000))     // 31s → 31/2 → 15（进档二）
	require.Equal(t, 30, service.SpriteFrameCount(60_000))     // 60s → 30
	require.Equal(t, 60, service.SpriteFrameCount(120_000))    // 120s → 60（档二上界）
	require.Equal(t, 30, service.SpriteFrameCount(121_000))    // 121s → 121/4 → 30（进档三）
	require.Equal(t, 75, service.SpriteFrameCount(300_000))    // 5min → 75
	require.Equal(t, 120, service.SpriteFrameCount(600_000))   // 10min → 150 → 钳到 120
	require.Equal(t, 120, service.SpriteFrameCount(7_200_000)) // 2h → 钳到 120
}

func TestEnsureSkipsWhenExists(t *testing.T) {
	g := service.NewSpriteGenerator()
	dir := t.TempDir()
	out := filepath.Join(dir, "sprite.jpg")
	require.NoError(t, writeFakeJPEG(t, out, 10*service.SpriteFrameW, service.SpriteFrameH))
	// 文件已存在 → 不调用 ffmpeg，srcPath 给假路径也应成功；10s → 1帧/s → 10 帧（与文件一致）
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
	// 竖屏式：单帧 240×427，6 帧 → sprite 1440×427；高从文件读回原始帧高
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
