package service

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"

	"github.com/stretchr/testify/require"
)

// fakeJPEGHeader hand-builds a minimal JPEG byte sequence: SOI + APP0(JFIF) +
// SOF0 (baseline, single component). In configOnly mode the JPEG decoder
// returns as soon as it's read SOF0, without needing real entropy-coded scan
// data, so any (legal 16-bit range) width/height can be declared without
// allocating any pixel memory — used to test "read the header to determine
// whether it exceeds the limit" without actually constructing a 199.8MP image.
func fakeJPEGHeader(width, height int) []byte {
	buf := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x07, 'J', 'F', 'I', 'F', 0x00}
	buf = append(buf, 0xFF, 0xC0, 0x00, 0x0B,
		8, // 8-bit precision
		byte(height>>8), byte(height),
		byte(width>>8), byte(width),
		1,    // nComp
		1,    // component id
		0x11, // h=1, v=1
		0,    // quant table selector
	)
	return buf
}

func TestPixelsExceedMLLimit(t *testing.T) {
	cases := []struct {
		name          string
		width, height int
		want          bool
	}{
		{"below the threshold (169.9M)", 1, 169_900_000, false},
		{"exactly at the threshold (170M), not exceeding", 1, 170_000_000, false},
		{"just over the threshold", 1, 170_000_001, true},
		{"real case 16320x12240=199.8MP", 16320, 12240, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.want, pixelsExceedMLLimit(c.width, c.height))
		})
	}
}

func TestOversizedForML(t *testing.T) {
	t.Run("a hand-built oversized JPEG header is judged as exceeding the limit", func(t *testing.T) {
		data := fakeJPEGHeader(16320, 12240)
		require.True(t, oversizedForML(data), "16320x12240=199.8MP should exceed the 170M threshold")
	})

	t.Run("a normal small image does not exceed the limit", func(t *testing.T) {
		img := image.NewRGBA(image.Rect(0, 0, 20, 10))
		for y := 0; y < 10; y++ {
			for x := 0; x < 20; x++ {
				img.Set(x, y, color.RGBA{R: 1, G: 2, B: 3, A: 255})
			}
		}
		var buf bytes.Buffer
		require.NoError(t, png.Encode(&buf, img))
		require.False(t, oversizedForML(buf.Bytes()), "a 20x10 small image should not be judged as exceeding the limit")
	})

	t.Run("an unrecognizable format is treated as not exceeding the limit", func(t *testing.T) {
		require.False(t, oversizedForML([]byte("not an image")))
	})
}
