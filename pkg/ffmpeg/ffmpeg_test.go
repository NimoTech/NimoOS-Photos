package ffmpeg_test

import (
	"image"
	_ "image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/pkg/ffmpeg"
	"github.com/stretchr/testify/require"
)

// TestFFmpegAvailable checks that ffmpeg/ffprobe are present on this host.
// If they are not installed, all integration tests are skipped.
func TestFFmpegAvailable(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not found in PATH — skipping ffmpeg package tests")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not found in PATH — skipping ffmpeg package tests")
	}
}

// TestExtractKeyframeNonexistent verifies that passing a nonexistent video path
// returns an error rather than panicking or silently succeeding.
func TestExtractKeyframeNonexistent(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not found in PATH")
	}
	_, err := ffmpeg.ExtractKeyframe("/nonexistent/path/video.mp4", t.TempDir())
	require.Error(t, err)
}

// TestGetDurationNonexistent verifies that passing a nonexistent file returns
// an error.
func TestGetDurationNonexistent(t *testing.T) {
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not found in PATH")
	}
	_, err := ffmpeg.GetDurationMs("/nonexistent/path/video.mp4")
	require.Error(t, err)
}

func TestProbeNonexistent(t *testing.T) {
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not found in PATH")
	}
	_, err := ffmpeg.Probe("/nonexistent/path/video.mp4")
	require.Error(t, err)
}

func TestGenerateSpriteProducesTile(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not found in PATH")
	}
	dir := t.TempDir()
	// 造一个 6 秒测试视频
	src := filepath.Join(dir, "src.mp4")
	mk := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=duration=6:size=320x240:rate=25", "-y", src)
	require.NoError(t, mk.Run())

	out := filepath.Join(dir, "sub", "sprite.jpg") // 子目录不存在，验证自动建目录
	require.NoError(t, ffmpeg.GenerateSprite(src, out, 10, 6.0))

	f, err := os.Open(out)
	require.NoError(t, err)
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	require.NoError(t, err)
	// scale=240:-2 保留原始比例不补黑边：320×240(4:3) 源 → 单帧 240×180。
	require.Equal(t, 10*240, cfg.Width) // tile=10x1 → 宽恒为 N*240
	require.Equal(t, 180, cfg.Height)   // 240 * 240/320 = 180（按原比例）
}

func TestGenerateSpriteRejectsZeroDuration(t *testing.T) {
	require.Error(t, ffmpeg.GenerateSprite("/any.mp4", "/tmp/x.jpg", 10, 0))
}

func TestParseISO6709(t *testing.T) {
	cases := []struct {
		in       string
		lat, lon float64
		ok       bool
	}{
		{"+39.9042+116.4074/", 39.9042, 116.4074, true},
		{"-33.8568+151.2153+010.500/", -33.8568, 151.2153, true},
		{"+00.0000-000.0000/", 0, 0, true},
		{"garbage", 0, 0, false},
		{"", 0, 0, false},
	}
	for _, c := range cases {
		lat, lon, ok := ffmpeg.ParseISO6709(c.in)
		require.Equal(t, c.ok, ok, "case %q", c.in)
		if c.ok {
			require.InDelta(t, c.lat, lat, 1e-6, "lat %q", c.in)
			require.InDelta(t, c.lon, lon, 1e-6, "lon %q", c.in)
		}
	}
}
