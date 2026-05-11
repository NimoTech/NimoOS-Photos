package ffmpeg_test

import (
	"os/exec"
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
