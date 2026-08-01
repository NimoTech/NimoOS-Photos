package service

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestProcessFileInternal_InlineSpritePregen verifies, via the real
// indexing pipeline (the full Enqueue→Start async flow), the hover-preview
// pregeneration goroutine triggered after processFileInternal's step 8:
// once a video is ingested, <thumbDir>/<assetID>/sprite.jpg and
// <thumbDir>/<assetID>/preview.mp4 should land asynchronously, without
// waiting for the /sprite, /preview routes' first-hover on-demand
// generation, and without depending on the BackfillSprites backfill.
//
// This path previously had zero test coverage — sprite_backfill_test.go
// only tests BackfillSprites' backfill logic, sprite_test.go only tests the
// generator itself; the fire-and-forget inline goroutine in indexer.go was
// never triggered by the real indexing pipeline (Enqueue/Start).
//
// Uses require.Eventually to poll for the artifact, ensuring the test
// doesn't return until the goroutine has genuinely finished (or fails at
// the deadline timeout) — this sidesteps a known race in the fire-and-forget
// design under a TempDir test environment: returning without waiting could
// let t.TempDir() cleanup happen before the goroutine writes its file.
func TestProcessFileInternal_InlineSpritePregen(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not found")
	}
	db := makeTestDB(t)
	thumbDir := t.TempDir()
	ix := NewIndexer(db, &mockML{}, thumbDir, 1)
	ix.SetPreviewPregen(true) // this test specifically covers the inline preview.mp4 pregeneration, needs explicit enabling (default is pure lazy generation)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ix.Start(ctx)

	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "v1.mp4")
	require.NoError(t, exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=duration=2:size=320x240:rate=25", "-y", src).Run())

	ix.Enqueue(src)

	var assetID string
	require.Eventually(t, func() bool {
		return db.QueryRow(`SELECT id FROM assets WHERE file_path=? AND status='indexed'`, src).Scan(&assetID) == nil
	}, 10*time.Second, 100*time.Millisecond, "video should be fully processed by the indexing pipeline")
	require.NotEmpty(t, assetID)

	spritePath := filepath.Join(thumbDir, assetID, "sprite.jpg")
	require.Eventually(t, func() bool {
		fi, err := os.Stat(spritePath)
		return err == nil && fi.Size() > 0
	}, 10*time.Second, 100*time.Millisecond, "ingesting a video should asynchronously pregenerate the hover sprite sprite.jpg")

	previewPath := filepath.Join(thumbDir, assetID, "preview.mp4")
	require.Eventually(t, func() bool {
		fi, err := os.Stat(previewPath)
		return err == nil && fi.Size() > 0
	}, 10*time.Second, 100*time.Millisecond, "ingesting a video should asynchronously pregenerate the hover preview video preview.mp4")
}

// TestProcessFileInternal_InlineSpriteLazyByDefault verifies the default
// pure-lazy-generation behavior: without calling SetPreviewPregen (i.e.
// photos.PreviewPregen defaults to false), ingesting a video still
// asynchronously pregenerates sprite.jpg, but preview.mp4 should not be
// pregenerated — it's left to the /preview route to generate on demand when
// the user actually hovers. sprite/preview execute sequentially within the
// same goroutine (Ensure the sprite first, then check the pregen flag), so
// sprite.jpg landing means that goroutine has already run past the preview
// flag check — asserting preview.mp4 doesn't exist at that point is
// deterministic and doesn't need extra waiting.
func TestProcessFileInternal_InlineSpriteLazyByDefault(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not found")
	}
	db := makeTestDB(t)
	thumbDir := t.TempDir()
	ix := NewIndexer(db, &mockML{}, thumbDir, 1) // SetPreviewPregen not called, defaults to false

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ix.Start(ctx)

	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "v1.mp4")
	require.NoError(t, exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=duration=2:size=320x240:rate=25", "-y", src).Run())

	ix.Enqueue(src)

	var assetID string
	require.Eventually(t, func() bool {
		return db.QueryRow(`SELECT id FROM assets WHERE file_path=? AND status='indexed'`, src).Scan(&assetID) == nil
	}, 10*time.Second, 100*time.Millisecond, "video should be fully processed by the indexing pipeline")
	require.NotEmpty(t, assetID)

	spritePath := filepath.Join(thumbDir, assetID, "sprite.jpg")
	require.Eventually(t, func() bool {
		fi, err := os.Stat(spritePath)
		return err == nil && fi.Size() > 0
	}, 10*time.Second, 100*time.Millisecond, "the sprite should still be pregenerated when PreviewPregen is off")

	previewPath := filepath.Join(thumbDir, assetID, "preview.mp4")
	_, err := os.Stat(previewPath)
	require.True(t, os.IsNotExist(err), "preview.mp4 should not be pregenerated when PreviewPregen defaults to false")
}
