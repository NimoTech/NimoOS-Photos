package aesthetic

import (
	"bytes"
	_ "embed"
)

// 权重由 scripts/aesthetic/ 下的转换/训练脚本产出(NAES 格式)。
// 换头时替换此文件并保证版本串变化(aesthetic_head_ver 靠它触发全库重打)。
//
//go:embed weights/head_v1.bin
var embeddedWeights []byte

// Load 解析随二进制内嵌的美学评分头。
func Load() (*Head, error) {
	return LoadFrom(bytes.NewReader(embeddedWeights))
}
