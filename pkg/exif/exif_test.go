package exif_test

import (
	"bytes"
	"image"
	"image/jpeg"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/pkg/exif"
	"github.com/stretchr/testify/require"
)

func TestParseNoEXIF(t *testing.T) {
	// generate a plain JPEG with no EXIF using image/jpeg
	img := image.NewRGBA(image.Rect(0, 0, 10, 10))
	var buf bytes.Buffer
	jpeg.Encode(&buf, img, nil)
	result := exif.Parse(bytes.NewReader(buf.Bytes()))
	require.NotNil(t, result)
	require.True(t, result.TakenAt.IsZero())
}

func TestParseNonJPEG(t *testing.T) {
	// passing garbage data should not panic, should return zero result
	result := exif.Parse(bytes.NewReader([]byte("not a jpeg")))
	require.NotNil(t, result)
	require.True(t, result.TakenAt.IsZero())
	require.Equal(t, "", result.Make)
}

func TestParseEmptyReader(t *testing.T) {
	result := exif.Parse(bytes.NewReader([]byte{}))
	require.NotNil(t, result)
	require.True(t, result.TakenAt.IsZero())
}

func TestResultHasExtendedFields(t *testing.T) {
	// Even when the input has no EXIF, the new fields must be addressable
	// and zero-valued (this also acts as a compile-time check).
	result := exif.Parse(bytes.NewReader([]byte{}))
	require.Equal(t, 0, result.ISO)
	require.Equal(t, "", result.ShutterSpeed)
	require.Equal(t, 0.0, result.Aperture)
	require.Equal(t, 0.0, result.FocalLength)
	require.Equal(t, 0, result.Orientation)
}
