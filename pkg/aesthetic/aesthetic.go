// Package aesthetic 在现成 CLIP(SigLIP2)图向量上跑一个小线性头计算美学分。
// 纯本地矩阵乘、微秒级/张,不依赖 ML 服务。权重格式见本包 doc.go/计划文档。
package aesthetic

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
)

type layer struct {
	in, out int
	w       []float32 // 行主序 [out][in]
	b       []float32
}

// Head 是加载完成的美学评分头。并发只读安全。
type Head struct {
	ver    string
	layers []layer
}

func (h *Head) Version() string { return h.ver }
func (h *Head) InDim() int      { return h.layers[0].in }

// LoadFrom 解析权重字节流。任何结构不符都返回错误。
func LoadFrom(r io.Reader) (*Head, error) {
	var magic [4]byte
	if _, err := io.ReadFull(r, magic[:]); err != nil {
		return nil, fmt.Errorf("aesthetic: read magic: %w", err)
	}
	if string(magic[:]) != "NAES" {
		return nil, fmt.Errorf("aesthetic: bad magic %q", magic)
	}
	var verLen uint32
	if err := binary.Read(r, binary.LittleEndian, &verLen); err != nil {
		return nil, fmt.Errorf("aesthetic: read verLen: %w", err)
	}
	if verLen > 128 {
		return nil, fmt.Errorf("aesthetic: verLen %d too large", verLen)
	}
	verBuf := make([]byte, verLen)
	if _, err := io.ReadFull(r, verBuf); err != nil {
		return nil, fmt.Errorf("aesthetic: read ver: %w", err)
	}
	var nLayers uint32
	if err := binary.Read(r, binary.LittleEndian, &nLayers); err != nil {
		return nil, fmt.Errorf("aesthetic: read nLayers: %w", err)
	}
	if nLayers == 0 || nLayers > 16 {
		return nil, fmt.Errorf("aesthetic: bad layer count %d", nLayers)
	}
	h := &Head{ver: string(verBuf)}
	for i := 0; i < int(nLayers); i++ {
		var in, out uint32
		if err := binary.Read(r, binary.LittleEndian, &in); err != nil {
			return nil, fmt.Errorf("aesthetic: layer %d in: %w", i, err)
		}
		if err := binary.Read(r, binary.LittleEndian, &out); err != nil {
			return nil, fmt.Errorf("aesthetic: layer %d out: %w", i, err)
		}
		if in == 0 || out == 0 || in > 1<<14 || out > 1<<14 {
			return nil, fmt.Errorf("aesthetic: layer %d bad dims %dx%d", i, in, out)
		}
		l := layer{in: int(in), out: int(out),
			w: make([]float32, int(in)*int(out)), b: make([]float32, out)}
		if err := binary.Read(r, binary.LittleEndian, l.w); err != nil {
			return nil, fmt.Errorf("aesthetic: layer %d weights: %w", i, err)
		}
		if err := binary.Read(r, binary.LittleEndian, l.b); err != nil {
			return nil, fmt.Errorf("aesthetic: layer %d bias: %w", i, err)
		}
		if i > 0 && h.layers[i-1].out != l.in {
			return nil, fmt.Errorf("aesthetic: layer %d in %d != prev out %d", i, l.in, h.layers[i-1].out)
		}
		h.layers = append(h.layers, l)
	}
	if h.layers[len(h.layers)-1].out != 1 {
		return nil, fmt.Errorf("aesthetic: final layer out %d != 1", h.layers[len(h.layers)-1].out)
	}
	return h, nil
}

// Score 对图向量打分:先 L2 归一化(v2.5 推理惯例),再逐层 y=Wx+b。
// 维度不符返回 NaN(调用侧跳过该资产)。
func (h *Head) Score(vec []float32) float64 {
	if len(vec) != h.layers[0].in {
		return math.NaN()
	}
	x := make([]float64, len(vec))
	var norm float64
	for i, v := range vec {
		x[i] = float64(v)
		norm += x[i] * x[i]
	}
	if norm > 0 {
		norm = math.Sqrt(norm)
		for i := range x {
			x[i] /= norm
		}
	}
	for _, l := range h.layers {
		y := make([]float64, l.out)
		for o := 0; o < l.out; o++ {
			s := float64(l.b[o])
			row := l.w[o*l.in : (o+1)*l.in]
			for i, wv := range row {
				s += float64(wv) * x[i]
			}
			y[o] = s
		}
		x = y
	}
	return x[0]
}
