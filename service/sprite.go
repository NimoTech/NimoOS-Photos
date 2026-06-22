package service

import (
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // register JPEG decoder for image.DecodeConfig
	"os"
	"sync"

	"github.com/NimoTech/NimoOS-Photos/pkg/ffmpeg"
)

const (
	SpriteFrameW = 120
	SpriteFrameH = 68

	spriteMinFrames     = 5
	spriteMaxFrames     = 20
	spriteSecsPerFrm    = 30
	spriteMaxConcurrent = 2
)

// SpriteFrameCount returns clamp(durationSeconds/30, 5, 20).
func SpriteFrameCount(durationMs int64) int {
	n := int(durationMs / 1000 / spriteSecsPerFrm)
	if n < spriteMinFrames {
		return spriteMinFrames
	}
	if n > spriteMaxFrames {
		return spriteMaxFrames
	}
	return n
}

// SpriteFrameCountFromFile derives the actual frame count from a sprite image's
// width (width / 120). tile=Nx1 makes width deterministically N*120.
func SpriteFrameCountFromFile(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return 0, err
	}
	if cfg.Width < SpriteFrameW {
		return 0, fmt.Errorf("sprite width %d too small", cfg.Width)
	}
	return cfg.Width / SpriteFrameW, nil
}

// SpriteGenerator lazily generates hover-preview sprites with a global
// concurrency cap and per-output deduplication.
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

// Ensure makes sure outPath holds a sprite for srcPath, generating it on demand.
// durationMs must be > 0 (caller resolves it). Returns the frame count used.
func (g *SpriteGenerator) Ensure(srcPath, outPath string, durationMs int64) (int, error) {
	fc := SpriteFrameCount(durationMs)
	if _, err := os.Stat(outPath); err == nil {
		return fc, nil
	}

	// Per-output dedup: the first caller leads generation; others wait, then
	// read the just-written file.
	g.mu.Lock()
	if ch, ok := g.inflight[outPath]; ok {
		g.mu.Unlock()
		<-ch
		if _, err := os.Stat(outPath); err == nil {
			return fc, nil
		}
		return 0, errors.New("sprite generation failed (joined leader)")
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

	if err := ffmpeg.GenerateSprite(srcPath, outPath, fc, float64(durationMs)/1000.0); err != nil {
		return 0, err
	}
	return fc, nil
}
