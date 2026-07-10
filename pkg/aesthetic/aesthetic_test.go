package aesthetic

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

// buildHead 按格式手工拼一个头的字节流。
func buildHead(t *testing.T, ver string, layers [][2][]float32, dims [][2]uint32) []byte {
	var buf bytes.Buffer
	buf.WriteString("NAES")
	require.NoError(t, binary.Write(&buf, binary.LittleEndian, uint32(len(ver))))
	buf.WriteString(ver)
	require.NoError(t, binary.Write(&buf, binary.LittleEndian, uint32(len(layers))))
	for i, l := range layers {
		require.NoError(t, binary.Write(&buf, binary.LittleEndian, dims[i][0]))
		require.NoError(t, binary.Write(&buf, binary.LittleEndian, dims[i][1]))
		require.NoError(t, binary.Write(&buf, binary.LittleEndian, l[0])) // weights
		require.NoError(t, binary.Write(&buf, binary.LittleEndian, l[1])) // bias
	}
	return buf.Bytes()
}

// 2 维输入 → 单层 1 维输出的 golden:输入 (3,4) L2 归一化后为 (0.6,0.8),
// W=[1,2] b=[0.5] ⇒ 0.6*1+0.8*2+0.5 = 2.7。
func TestScoreGolden(t *testing.T) {
	data := buildHead(t, "v-test",
		[][2][]float32{{{1, 2}, {0.5}}},
		[][2]uint32{{2, 1}})
	h, err := LoadFrom(bytes.NewReader(data))
	require.NoError(t, err)
	require.Equal(t, "v-test", h.Version())
	require.Equal(t, 2, h.InDim())
	got := h.Score([]float32{3, 4})
	require.InDelta(t, 2.7, got, 1e-6)
}

// 两层链:2→2(W=单位阵,b=0)再 2→1(W=[1,1],b=0);输入 (0,5) 归一化 (0,1) ⇒ 1。
func TestScoreTwoLayers(t *testing.T) {
	data := buildHead(t, "v",
		[][2][]float32{
			{{1, 0, 0, 1}, {0, 0}},
			{{1, 1}, {0}},
		},
		[][2]uint32{{2, 2}, {2, 1}})
	h, err := LoadFrom(bytes.NewReader(data))
	require.NoError(t, err)
	require.InDelta(t, 1.0, h.Score([]float32{0, 5}), 1e-6)
}

func TestScoreDimMismatchReturnsNaN(t *testing.T) {
	data := buildHead(t, "v", [][2][]float32{{{1, 2}, {0}}}, [][2]uint32{{2, 1}})
	h, err := LoadFrom(bytes.NewReader(data))
	require.NoError(t, err)
	require.True(t, math.IsNaN(h.Score([]float32{1, 2, 3})))
}

func TestLoadFromBadMagic(t *testing.T) {
	_, err := LoadFrom(bytes.NewReader([]byte("XXXX....")))
	require.Error(t, err)
}
