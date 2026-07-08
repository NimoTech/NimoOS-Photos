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

// recordingFaceML 记录 DetectAndRecognizeFaces 最近一次收到的字节，用于断言
// detectFaceScanTarget 在原图超限时是否已经把输入换成了缩略图而不是原图。
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

// TestDetectFaceScanTarget_OversizedImageFallsBackToThumbnail 覆盖真实定位到的
// bug：原图超过 immich-ml/PIL 的 178.9MP 硬上限（真实案例是库里
// 16320x12240=199.8MP 的 Pexels 照片）时，detectFaceScanTarget 必须自动降级
// 用已生成的 large.jpg 缩略图代替原图喂给人脸检测，否则请求必然 500、
// face_scanned 永远置不上、RunPipeline 无限重试同一张图。
func TestDetectFaceScanTarget_OversizedImageFallsBackToThumbnail(t *testing.T) {
	db := makeTestDB(t)
	srcDir := t.TempDir()
	thumbDir := t.TempDir()
	const assetID = "asset-oversized"

	// 原文件只是一段手工构造的 JPEG 头，声明 16320x12240——探测阶段只靠
	// image.DecodeConfig 读头部，不需要真的能解码出像素。
	oversizedPath := filepath.Join(srcDir, "big.jpg")
	require.NoError(t, os.WriteFile(oversizedPath, fakeJPEGHeader(16320, 12240), 0o644))

	// thumbs/<id>/large.jpg 放一张真实的小 JPEG，模拟索引流水线已经生成过缩略图。
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
	require.Equal(t, thumbBytes, ml.lastData, "超限原图应换成 large.jpg 缩略图字节喂给 ML，而不是原图字节")
}

// TestDetectFaceScanTarget_OversizedImageWithoutThumbnailFails 覆盖降级也拿不到
// 缩略图的情况：large.jpg/small.jpg 都不存在时，必须按现有失败路径处理（返回
// error，face_scanned 保持 0，交给 RunPipeline 下一轮重试），而不是把超限原图
// 硬塞给 ML。
func TestDetectFaceScanTarget_OversizedImageWithoutThumbnailFails(t *testing.T) {
	db := makeTestDB(t)
	srcDir := t.TempDir()
	thumbDir := t.TempDir() // 空目录：没有任何缩略图

	oversizedPath := filepath.Join(srcDir, "big.jpg")
	require.NoError(t, os.WriteFile(oversizedPath, fakeJPEGHeader(16320, 12240), 0o644))

	ml := &recordingFaceML{}
	s := NewFaceService(db)
	s.SetML(ml)
	s.SetThumbDir(thumbDir)

	err := s.detectFaceScanTarget(context.Background(), faceScanTarget{id: "asset-no-thumb", path: oversizedPath, isVideo: false})
	require.Error(t, err)
	require.Nil(t, ml.lastData, "缩略图不可用时不应把超限原图传给 ML")
}
