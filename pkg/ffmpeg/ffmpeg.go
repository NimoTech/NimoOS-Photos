// Package ffmpeg wraps ffmpeg/ffprobe CLI calls for video processing tasks
// required by the NimoOS-Photos indexer pipeline.
package ffmpeg

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
)

// ExtractKeyframe extracts a single representative frame from videoPath and
// writes it as JPEG to outDir/keyframe.jpg. It first tries seeking to 5 s; if
// ffmpeg reports failure (short video), it retries from 0 s. The path of the
// written JPEG is returned.
func ExtractKeyframe(videoPath, outDir string) (string, error) {
	out := filepath.Join(outDir, "keyframe.jpg")

	// first attempt: seek to 5 s
	if err := runExtract(videoPath, out, "5"); err == nil {
		// verify the file was actually created
		if _, statErr := os.Stat(out); statErr == nil {
			return out, nil
		}
	}

	// retry: seek from position 0 (works for very short clips)
	if err := runExtract(videoPath, out, "0"); err != nil {
		return "", fmt.Errorf("ffmpeg ExtractKeyframe: %w", err)
	}
	if _, err := os.Stat(out); err != nil {
		return "", fmt.Errorf("ffmpeg ExtractKeyframe: output file not created: %w", err)
	}
	return out, nil
}

// runExtract runs the actual ffmpeg command to pull a single frame.
func runExtract(videoPath, outPath, seekSec string) error {
	cmd := exec.Command("ffmpeg",
		"-ss", seekSec,
		"-i", videoPath,
		"-vframes", "1",
		"-q:v", "2",
		"-y",
		outPath,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ffmpeg: %w — %s", err, string(out))
	}
	return nil
}

// ffprobeFormat mirrors the JSON structure returned by:
//
//	ffprobe -v quiet -print_format json -show_format <path>
type ffprobeOutput struct {
	Format struct {
		Duration string `json:"duration"`
	} `json:"format"`
}

// GetDurationMs returns the duration of the media file at videoPath in
// milliseconds. It calls ffprobe with JSON output and parses the
// format.duration field (a decimal string of seconds).
func GetDurationMs(videoPath string) (int64, error) {
	cmd := exec.Command("ffprobe",
		"-v", "quiet",
		"-print_format", "json",
		"-show_format",
		videoPath,
	)
	raw, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("ffprobe GetDurationMs: %w", err)
	}

	var result ffprobeOutput
	if err := json.Unmarshal(raw, &result); err != nil {
		return 0, fmt.Errorf("ffprobe GetDurationMs: parse JSON: %w", err)
	}
	if result.Format.Duration == "" {
		return 0, fmt.Errorf("ffprobe GetDurationMs: duration field missing")
	}

	secs, err := strconv.ParseFloat(result.Format.Duration, 64)
	if err != nil {
		return 0, fmt.Errorf("ffprobe GetDurationMs: parse duration %q: %w",
			result.Format.Duration, err)
	}

	ms := int64(math.Round(secs * 1000))
	return ms, nil
}

// ExtractEmbeddedVideo extracts the embedded video stream (stream index 1) from
// a Google Motion Photo JPEG at jpegPath and writes it to outPath. This is
// used to separate the still image from the MP4 clip embedded by Android
// cameras.
func ExtractEmbeddedVideo(jpegPath, outPath string) error {
	cmd := exec.Command("ffmpeg",
		"-i", jpegPath,
		"-map", "0:v:1",
		"-y",
		outPath,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ffmpeg ExtractEmbeddedVideo: %w — %s", err, string(out))
	}
	return nil
}
