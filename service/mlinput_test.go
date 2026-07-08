package service

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"

	"github.com/stretchr/testify/require"
)

// fakeJPEGHeader 手工构造一段最小的 JPEG 字节序列：SOI + APP0(JFIF) +
// SOF0(baseline，单分量)。JPEG 解码器在 configOnly 模式下读完 SOF0 就返回，
// 不需要真正的熵编码扫描数据，所以可以在不分配任何像素内存的情况下声明任意
// (合法 16 位范围内的)宽高——用来测试"读头部判断是否超限"而不必真的构造一张
// 199.8MP 的图片。
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
		{"低于阈值(169.9M)", 1, 169_900_000, false},
		{"恰好等于阈值(170M)不算超限", 1, 170_000_000, false},
		{"刚超过阈值", 1, 170_000_001, true},
		{"真实案例 16320x12240=199.8MP", 16320, 12240, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.want, pixelsExceedMLLimit(c.width, c.height))
		})
	}
}

func TestOversizedForML(t *testing.T) {
	t.Run("手工构造的超大 JPEG 头判定为超限", func(t *testing.T) {
		data := fakeJPEGHeader(16320, 12240)
		require.True(t, oversizedForML(data), "16320x12240=199.8MP 应超过 170M 阈值")
	})

	t.Run("正常小图不超限", func(t *testing.T) {
		img := image.NewRGBA(image.Rect(0, 0, 20, 10))
		for y := 0; y < 10; y++ {
			for x := 0; x < 20; x++ {
				img.Set(x, y, color.RGBA{R: 1, G: 2, B: 3, A: 255})
			}
		}
		var buf bytes.Buffer
		require.NoError(t, png.Encode(&buf, img))
		require.False(t, oversizedForML(buf.Bytes()), "20x10 的小图不应判定为超限")
	})

	t.Run("无法识别的格式按不超限处理", func(t *testing.T) {
		require.False(t, oversizedForML([]byte("not an image")))
	})
}
