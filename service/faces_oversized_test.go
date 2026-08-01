package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/common"
	"github.com/NimoTech/NimoOS-Photos/pkg/mlclient"
	"github.com/stretchr/testify/require"
)

// recordingFaceML records the last bytes DetectAndRecognizeFaces received, so
// tests can assert whether detectFaceScanTarget has swapped the input for a
// thumbnail instead of the original when the original is oversized.
type recordingFaceML struct {
	lastData []byte
}

func (m *recordingFaceML) CLIPImageEmbed(_ []byte) ([]float32, error) {
	return make([]float32, common.CLIPDim), nil
}
func (m *recordingFaceML) CLIPTextEmbed(_ string) ([]float32, error) {
	return make([]float32, common.CLIPDim), nil
}
func (m *recordingFaceML) DetectAndRecognizeFaces(data []byte) ([]mlclient.FaceResult, error) {
	m.lastData = data
	return nil, nil
}
func (m *recordingFaceML) OCR(_ []byte) ([]mlclient.OCRLine, error) { return nil, nil }
func (m *recordingFaceML) IsReady() bool                            { return true }

// TestDetectFaceScanTarget_OversizedImageFallsBackToThumbnail covers a real
// bug that was tracked down: when the original image exceeds immich-ml/PIL's
// 178.9MP hard limit (a real case in the library was a
// 16320x12240=199.8MP Pexels photo), detectFaceScanTarget must automatically
// fall back to the already-generated large.jpg thumbnail in place of the
// original when feeding face detection, or the request will always 500,
// face_scanned will never get set, and RunPipeline will retry the same image
// forever.
func TestDetectFaceScanTarget_OversizedImageFallsBackToThumbnail(t *testing.T) {
	db := makeTestDB(t)
	srcDir := t.TempDir()
	thumbDir := t.TempDir()
	const assetID = "asset-oversized"

	// The source file is just a hand-crafted JPEG header declaring
	// 16320x12240 — the detection step only reads the header via
	// image.DecodeConfig, no actual pixel decoding is needed.
	oversizedPath := filepath.Join(srcDir, "big.jpg")
	require.NoError(t, os.WriteFile(oversizedPath, fakeJPEGHeader(16320, 12240), 0o644))

	// Put a real small JPEG at thumbs/<id>/large.jpg, simulating the index
	// pipeline having already generated a thumbnail.
	require.NoError(t, os.MkdirAll(filepath.Join(thumbDir, assetID), 0o755))
	generatedThumb := makeTestJPEG(t, filepath.Join(thumbDir, assetID))
	largePath := filepath.Join(thumbDir, assetID, "large.jpg")
	require.NoError(t, os.Rename(generatedThumb, largePath))
	thumbBytes, err := os.ReadFile(largePath)
	require.NoError(t, err)

	ml := &recordingFaceML{}
	s := NewFaceService(db)
	s.SetML(ml)
	s.SetThumbDir(thumbDir)

	err = s.detectFaceScanTarget(context.Background(), faceScanTarget{id: assetID, path: oversizedPath, isVideo: false})
	require.NoError(t, err)
	require.Equal(t, thumbBytes, ml.lastData, "oversized original should be swapped for large.jpg thumbnail bytes when fed to ML, not the original bytes")
}

// TestDetectFaceScanTarget_OversizedImageWithoutThumbnailFails covers the
// case where a fallback thumbnail isn't available either: when neither
// large.jpg nor small.jpg exists, this must follow the existing failure path
// (return error, face_scanned stays 0, leaving it for RunPipeline's next
// retry) rather than forcing the oversized original onto ML.
func TestDetectFaceScanTarget_OversizedImageWithoutThumbnailFails(t *testing.T) {
	db := makeTestDB(t)
	srcDir := t.TempDir()
	thumbDir := t.TempDir() // empty dir: no thumbnails at all

	oversizedPath := filepath.Join(srcDir, "big.jpg")
	require.NoError(t, os.WriteFile(oversizedPath, fakeJPEGHeader(16320, 12240), 0o644))

	ml := &recordingFaceML{}
	s := NewFaceService(db)
	s.SetML(ml)
	s.SetThumbDir(thumbDir)

	err := s.detectFaceScanTarget(context.Background(), faceScanTarget{id: "asset-no-thumb", path: oversizedPath, isVideo: false})
	require.Error(t, err)
	require.Nil(t, ml.lastData, "should not pass the oversized original to ML when no thumbnail is available")
}
