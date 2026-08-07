package service

import (
	"image"
	"image/color"
	"testing"

	"github.com/disintegration/imaging"
)

// A 2x1 image (red left, blue right) rotated per EXIF orientation must land
// pixels where a correct viewer would show them.
func TestApplyOrientation(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 2, 1))
	red := color.NRGBA{255, 0, 0, 255}
	blue := color.NRGBA{0, 0, 255, 255}
	src.SetNRGBA(0, 0, red)
	src.SetNRGBA(1, 0, blue)

	// Orientation 1: unchanged.
	out := applyOrientation(imaging.Clone(src), 1)
	if out.NRGBAAt(0, 0) != red {
		t.Fatalf("orientation 1 must be a no-op")
	}

	// Orientation 3: 180° — red moves to the right.
	out = applyOrientation(imaging.Clone(src), 3)
	if out.NRGBAAt(1, 0) != red {
		t.Fatalf("orientation 3: want red at (1,0), got %v", out.NRGBAAt(1, 0))
	}

	// Orientation 6 (stored image rotated 90° CCW relative to display):
	// viewer rotates 90° CW → 2x1 becomes 1x2, red (was left) ends up on top.
	out = applyOrientation(imaging.Clone(src), 6)
	b := out.Bounds()
	if b.Dx() != 1 || b.Dy() != 2 {
		t.Fatalf("orientation 6: want 1x2, got %dx%d", b.Dx(), b.Dy())
	}
	if out.NRGBAAt(0, 0) != red {
		t.Fatalf("orientation 6: want red at (0,0), got %v", out.NRGBAAt(0, 0))
	}
}
