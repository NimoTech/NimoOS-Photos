package service

import (
	"fmt"
	"image"
	_ "image/jpeg" // register JPEG decoder for image.DecodeConfig
	"os"
	"sync"

	"github.com/NimoTech/NimoOS-Photos/pkg/ffmpeg"
)

const (
	SpriteFrameW = 240 // 固定帧宽（2026-06-23 由 120 提升至 240 提画质）
	SpriteFrameH = 135 // 16:9 名义帧高，仅作默认/回退；实际帧高按原始比例自适应、从文件读

	spriteMinFrames     = 5
	spriteMaxFrames     = 120
	spriteMaxConcurrent = 2
)

// SpriteFrameCount returns a frame count whose sampling density decreases with
// duration, then clamped to [5, 120]:
//
//	≤30s     → 1 frame / 1s
//	30–120s  → 1 frame / 2s
//	>120s    → 1 frame / 4s
//
// Short clips get ~1fps for smooth scrubbing; longer videos sample sparser so
// the single-row sprite (width = frames × 240px) stays within JPEG/decoder
// limits. The clamp ceiling 120 keeps the widest sprite at 28800px.
func SpriteFrameCount(durationMs int64) int {
	durS := durationMs / 1000
	var n int64
	switch {
	case durS <= 30:
		n = durS // 1 帧/秒
	case durS <= 120:
		n = durS / 2 // 1 帧/2 秒
	default:
		n = durS / 4 // 1 帧/4 秒
	}
	if n < spriteMinFrames {
		return spriteMinFrames
	}
	if n > spriteMaxFrames {
		return spriteMaxFrames
	}
	return int(n)
}

// SpriteInfoFromFile derives the actual frame count (width / SpriteFrameW) and
// the frame height from a sprite image. The sprite is a single row (tile=Nx1),
// so the image height equals one frame's height, and the width is deterministically
// N*SpriteFrameW. Frame height varies per video (native aspect, no padding).
func SpriteInfoFromFile(path string) (frameCount, frameH int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return 0, 0, err
	}
	if cfg.Width < SpriteFrameW {
		return 0, 0, fmt.Errorf("sprite width %d too small", cfg.Width)
	}
	return cfg.Width / SpriteFrameW, cfg.Height, nil
}

// SpriteFrameCountFromFile derives the actual frame count from a sprite image's
// width (width / SpriteFrameW). tile=Nx1 makes width deterministically N*SpriteFrameW.
func SpriteFrameCountFromFile(path string) (int, error) {
	fc, _, err := SpriteInfoFromFile(path)
	return fc, err
}

// SpriteGenerator lazily generates hover-preview artifacts — the frame-tile
// sprite JPEG and the low-bitrate real-playback preview MP4 — with a global
// concurrency cap and per-output deduplication. The semaphore is shared
// across both artifact kinds (global concurrent ffmpeg processes stay ≤2
// regardless of which one is being built); letting both run unbounded would
// let ffmpeg starve the rest of the box.
type SpriteGenerator struct {
	sem      chan struct{}
	mu       sync.Mutex
	inflight map[string]chan struct{}
}

func NewSpriteGenerator() *SpriteGenerator {
	return &SpriteGenerator{
		sem:      make(chan struct{}, spriteMaxConcurrent),
		inflight: make(map[string]chan struct{}),
	}
}

// ensure is the common core shared by Ensure and EnsurePreview: outPath
// already existing is a fast success; otherwise the first caller for a given
// outPath runs build() while later callers for the same outPath wait on it
// (per-output in-flight dedup), all gated by the shared semaphore so global
// concurrent ffmpeg invocations stay bounded.
func (g *SpriteGenerator) ensure(outPath string, build func() error) error {
	if _, err := os.Stat(outPath); err == nil {
		return nil
	}

	// Per-output dedup: the first caller leads generation; others wait, then
	// read the just-written file.
	g.mu.Lock()
	if ch, ok := g.inflight[outPath]; ok {
		g.mu.Unlock()
		<-ch
		if _, err := os.Stat(outPath); err == nil {
			return nil
		}
		return fmt.Errorf("generation failed (joined leader): %s", outPath)
	}
	ch := make(chan struct{})
	g.inflight[outPath] = ch
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		delete(g.inflight, outPath)
		close(ch)
		g.mu.Unlock()
	}()

	g.sem <- struct{}{}
	defer func() { <-g.sem }()

	return build()
}

// Ensure makes sure outPath holds a sprite for srcPath, generating it on demand.
// durationMs must be > 0 (caller resolves it). Returns the frame count used.
func (g *SpriteGenerator) Ensure(srcPath, outPath string, durationMs int64) (int, error) {
	fc := SpriteFrameCount(durationMs)
	err := g.ensure(outPath, func() error {
		return ffmpeg.GenerateSprite(srcPath, outPath, fc, float64(durationMs)/1000.0)
	})
	if err != nil {
		return 0, err
	}
	return fc, nil
}

// EnsurePreview makes sure outPath holds a low-bitrate real-playback preview
// (240p/15fps/H.264, no audio) for srcPath, generating it on demand. Unlike
// Ensure it needs no duration — GeneratePreview has no fps expression that
// depends on it.
func (g *SpriteGenerator) EnsurePreview(srcPath, outPath string) error {
	return g.ensure(outPath, func() error {
		return ffmpeg.GeneratePreview(srcPath, outPath)
	})
}
