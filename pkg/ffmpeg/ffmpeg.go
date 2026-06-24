// Package ffmpeg wraps ffmpeg/ffprobe CLI calls for video processing tasks
// required by the NimoOS-Photos indexer pipeline.
package ffmpeg

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"image/png"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
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

// GenerateSprite extracts frameCount evenly-spaced frames from videoPath and
// writes them as a single horizontal sprite JPEG to outPath. Each cell is 240px
// wide; height preserves the video's native aspect ratio (no padding), so the
// frontend can letterbox/pillarbox it however it likes. durationS must be > 0.
// It oversamples by one frame (fps=(N+1)/D) so the tile is always full — never
// use -vframes to bound the count (it is an output option and cannot live inside
// -vf). The image is written to a temp file and atomically renamed, so
// concurrent generations and crashes never leave a partial sprite.
func GenerateSprite(videoPath, outPath string, frameCount int, durationS float64) error {
	if durationS <= 0 {
		return fmt.Errorf("GenerateSprite: durationS must be > 0, got %v", durationS)
	}
	if frameCount < 1 {
		return fmt.Errorf("GenerateSprite: frameCount must be >= 1, got %d", frameCount)
	}
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return fmt.Errorf("GenerateSprite: mkdir: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(outPath), ".sprite-*.jpg")
	if err != nil {
		return fmt.Errorf("GenerateSprite: temp: %w", err)
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath) // no-op after a successful rename

	// Extract each frame with input-side fast seek (-ss before -i) so ffmpeg
	// only decodes one GOP per frame instead of the whole stream. The old
	// single-pass `fps=N/D` filter decoded the *entire* video (minutes for a
	// 40-min clip); cost here is N keyframe seeks, independent of duration.
	// Frames are sampled at centers ts_i = (i+0.5)*D/N (matches the frame↔time
	// mapping), composited horizontally in-memory, and encoded once to JPEG.
	imgs := make([]image.Image, frameCount)
	errs := make([]error, frameCount)
	workers := runtime.NumCPU()
	if workers > spriteExtractWorkers {
		workers = spriteExtractWorkers
	}
	if workers < 1 {
		workers = 1
	}
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for i := 0; i < frameCount; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ts := (float64(i) + 0.5) * durationS / float64(frameCount)
			imgs[i], errs[i] = extractFrameAt(videoPath, ts)
		}(i)
	}
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			return fmt.Errorf("GenerateSprite: extract frame %d: %w", i, e)
		}
	}

	// Composite the N frames into one horizontal row. All frames share the same
	// scaled dimensions (same source), so width = N*frameW, height = frameH.
	fw := imgs[0].Bounds().Dx()
	fh := imgs[0].Bounds().Dy()
	canvas := image.NewRGBA(image.Rect(0, 0, fw*frameCount, fh))
	for i, im := range imgs {
		draw.Draw(canvas, image.Rect(i*fw, 0, i*fw+fw, fh), im, im.Bounds().Min, draw.Src)
	}

	out, err := os.Create(tmpPath)
	if err != nil {
		return fmt.Errorf("GenerateSprite: create temp: %w", err)
	}
	if err := jpeg.Encode(out, canvas, &jpeg.Options{Quality: spriteJPEGQuality}); err != nil {
		out.Close()
		return fmt.Errorf("GenerateSprite: encode: %w", err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("GenerateSprite: close: %w", err)
	}
	return os.Rename(tmpPath, outPath)
}

const (
	// spriteExtractWorkers bounds per-sprite concurrent frame extraction. Kept
	// small because SpriteGenerator already caps concurrent generations at 2;
	// peak ffmpeg processes ≈ 2 × this.
	spriteExtractWorkers = 4
	// spriteJPEGQuality ≈ the old ffmpeg `-q:v 4` (high quality).
	spriteJPEGQuality = 90
)

// extractFrameAt fast-seeks to ts seconds and returns one frame scaled to
// width 240 (height auto, native aspect, autorotated). It pipes a PNG out of
// ffmpeg (lossless, so the only lossy step is the final sprite JPEG encode).
func extractFrameAt(videoPath string, ts float64) (image.Image, error) {
	cmd := exec.Command("ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-ss", strconv.FormatFloat(ts, 'f', 3, 64), // input seek = fast (decode ~1 GOP)
		"-i", videoPath,
		"-frames:v", "1",
		"-an",
		"-vf", "scale=240:-2", // native aspect, even height, no pad; autorotate on by default
		"-f", "image2pipe", "-vcodec", "png", "-",
	)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	if err := cmd.Run(); err != nil {
		return nil, err // wraps exec.ErrNotFound when ffmpeg is missing
	}
	img, err := png.Decode(&buf)
	if err != nil {
		return nil, fmt.Errorf("decode frame: %w", err)
	}
	return img, nil
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

// MediaInfo carries all metadata that one ffprobe call extracts from a video file.
// Every field is best-effort: zero value means "not present" or "couldn't parse".
type MediaInfo struct {
	DurationMs  int64
	Width       int
	Height      int
	VideoCodec  string
	AudioCodec  string
	FrameRate   float64
	BitRate     int64 // bps
	Rotation    int   // 0, 90, 180, 270
	TakenAt     time.Time
	Latitude    float64
	Longitude   float64
	HasLocation bool // true when Latitude/Longitude were actually parsed (distinguishes "no GPS" from Null Island)
}

type ffprobeFull struct {
	Format struct {
		Duration string            `json:"duration"`
		BitRate  string            `json:"bit_rate"`
		Tags     map[string]string `json:"tags"`
	} `json:"format"`
	Streams []struct {
		CodecType    string            `json:"codec_type"`
		CodecName    string            `json:"codec_name"`
		Width        int               `json:"width"`
		Height       int               `json:"height"`
		RFrameRate   string            `json:"r_frame_rate"`
		AvgFrameRate string            `json:"avg_frame_rate"`
		BitRate      string            `json:"bit_rate"`
		Tags         map[string]string `json:"tags"`
		SideData     []struct {
			SideDataType string  `json:"side_data_type"`
			Rotation     float64 `json:"rotation"`
		} `json:"side_data_list"`
	} `json:"streams"`
}

// Probe runs `ffprobe -show_format -show_streams` on videoPath and parses the
// JSON output into a MediaInfo. Returns an error only when ffprobe itself fails
// or its output cannot be parsed; partial fields are tolerated and left zero.
func Probe(videoPath string) (*MediaInfo, error) {
	cmd := exec.Command("ffprobe",
		"-v", "quiet",
		"-print_format", "json",
		"-show_format",
		"-show_streams",
		videoPath,
	)
	raw, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ffprobe Probe: %w", err)
	}
	var p ffprobeFull
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("ffprobe Probe: parse JSON: %w", err)
	}

	info := &MediaInfo{}
	if p.Format.Duration != "" {
		if secs, err := strconv.ParseFloat(p.Format.Duration, 64); err == nil {
			info.DurationMs = int64(math.Round(secs * 1000))
		}
	}
	if p.Format.BitRate != "" {
		if br, err := strconv.ParseInt(p.Format.BitRate, 10, 64); err == nil {
			info.BitRate = br
		}
	}

	for _, s := range p.Streams {
		switch s.CodecType {
		case "video":
			if info.VideoCodec != "" {
				continue
			}
			info.VideoCodec = s.CodecName
			info.Width = s.Width
			info.Height = s.Height
			info.FrameRate = parseFraction(s.RFrameRate)
			if info.FrameRate == 0 {
				info.FrameRate = parseFraction(s.AvgFrameRate)
			}
			// Rotation source 1: legacy "rotate" tag
			if v, ok := s.Tags["rotate"]; ok {
				if r, err := strconv.Atoi(v); err == nil {
					info.Rotation = ((r % 360) + 360) % 360
				}
			}
			// Rotation source 2 (newer ffprobe): Display Matrix side data
			// gives a negative angle; flip sign and normalize to 0..359.
			for _, sd := range s.SideData {
				if sd.SideDataType == "Display Matrix" {
					r := int(math.Round(-sd.Rotation))
					info.Rotation = ((r % 360) + 360) % 360
				}
			}
		case "audio":
			if info.AudioCodec == "" {
				info.AudioCodec = s.CodecName
			}
		}
	}

	// creation_time lives in format.tags (case varies across muxers)
	if tagsLower := lowerKeys(p.Format.Tags); tagsLower["creation_time"] != "" {
		if t, err := time.Parse(time.RFC3339, tagsLower["creation_time"]); err == nil {
			info.TakenAt = t
		}
	}

	// GPS: iPhone/Android videos put it in format.tags."com.apple.quicktime.location.ISO6709"
	// or in a stream's tags as "location".
	if loc := findLocationTag(p.Format.Tags); loc != "" {
		if lat, lon, ok := ParseISO6709(loc); ok {
			info.Latitude = lat
			info.Longitude = lon
			info.HasLocation = true
		}
	}
	if !info.HasLocation {
		for _, s := range p.Streams {
			if loc := findLocationTag(s.Tags); loc != "" {
				if lat, lon, ok := ParseISO6709(loc); ok {
					info.Latitude = lat
					info.Longitude = lon
					info.HasLocation = true
					break
				}
			}
		}
	}

	return info, nil
}

func lowerKeys(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[strings.ToLower(k)] = v
	}
	return out
}

func findLocationTag(tags map[string]string) string {
	for k, v := range tags {
		kl := strings.ToLower(k)
		if kl == "location" ||
			kl == "com.apple.quicktime.location.iso6709" ||
			strings.HasSuffix(kl, "iso6709") {
			return v
		}
	}
	return ""
}

func parseFraction(s string) float64 {
	if s == "" || s == "0/0" {
		return 0
	}
	parts := strings.Split(s, "/")
	if len(parts) != 2 {
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return f
		}
		return 0
	}
	num, err1 := strconv.ParseFloat(parts[0], 64)
	den, err2 := strconv.ParseFloat(parts[1], 64)
	if err1 != nil || err2 != nil || den == 0 {
		return 0
	}
	return num / den
}

// iso6709Re matches a signed lat/lon pair at the start of an ISO 6709 short
// string. The optional altitude segment is intentionally ignored.
var iso6709Re = regexp.MustCompile(`^([+-]\d+(?:\.\d+)?)([+-]\d+(?:\.\d+)?)`)

// ParseISO6709 parses an ISO 6709 short-form coordinate string (e.g.
// "+39.9042+116.4074/" or "-33.8568+151.2153+010.500/") and returns latitude,
// longitude, true on success. Altitude is ignored. The function is exported so
// it can be unit-tested.
func ParseISO6709(s string) (float64, float64, bool) {
	m := iso6709Re.FindStringSubmatch(strings.TrimSpace(s))
	if len(m) != 3 {
		return 0, 0, false
	}
	lat, err1 := strconv.ParseFloat(m[1], 64)
	lon, err2 := strconv.ParseFloat(m[2], 64)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return lat, lon, true
}
