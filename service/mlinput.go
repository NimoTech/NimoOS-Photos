package service

import (
	"bytes"
	"image"
	"os"
	"path/filepath"
)

// maxMLInputPixels 是喂给 immich-ml(/predict)的图片输入的安全像素上限。
// immich-ml 容器内 PIL 自带防解压炸弹保护，硬上限为 178,956,970 像素
// (Pillow 默认 Image.MAX_IMAGE_PIXELS，约等于 0.25 * 2^31)；超过这个数字
// PIL 会直接抛异常，/predict 请求必然返回 500。这里取一个略低于硬上限的
// 安全阈值：一旦原图超过它，就自动把人脸检测/OCR 的输入降级成缩略图，
// 避免真实存在的超大图（例如库里出现过的 16320x12240=199.8MP 高分辨率
// 照片）把这两条 ML 流水线永久卡死——OCR 每次索引都 500 而被吞掉、
// face_scanned 永远置不上导致 RunPipeline 无限重试同一张图。
// 不做成配置项：这是绕过第三方硬限制的兜底,不需要用户可调(YAGNI)。
const maxMLInputPixels = 170_000_000

// pixelsExceedMLLimit 是判定逻辑的纯函数部分，只做乘法比较，便于直接单测
// 边界值而不需要构造图片字节。
func pixelsExceedMLLimit(width, height int) bool {
	return int64(width)*int64(height) > maxMLInputPixels
}

// oversizedForML 只读取图片字节的头部（image.DecodeConfig，不做全量解码）
// 拿到宽高，判断是否超过 maxMLInputPixels。
// data 不是已注册解码器能识别的格式时（DecodeConfig 出错），按不超限处理，
// 保持原有行为——把数据原样传给 ML，让 ML 自己决定是否报错。
func oversizedForML(data []byte) bool {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return false
	}
	return pixelsExceedMLLimit(cfg.Width, cfg.Height)
}

// readLargeOrSmallThumb 读取 <thumbDir>/<id>/large.jpg，取不到时回退 small.jpg；
// 两者都取不到返回 nil。用于给人脸检测/OCR 在原图超过 maxMLInputPixels 时
// 提供降级输入（沿用视频人脸检测/CLIP 补跑既有的 large→small 取图策略）。
func readLargeOrSmallThumb(thumbDir, id string) []byte {
	if b, err := os.ReadFile(filepath.Join(thumbDir, id, "large.jpg")); err == nil && len(b) > 0 {
		return b
	}
	if b, err := os.ReadFile(filepath.Join(thumbDir, id, "small.jpg")); err == nil && len(b) > 0 {
		return b
	}
	return nil
}
