package aesthetic

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

// buildHead hand-assembles a head's byte stream according to the format.
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

// Golden case for a 2-dim input -> single-layer 1-dim output: input (3,4)
// L2-normalizes to (0.6,0.8); W=[1,2] b=[0.5] => 0.6*1+0.8*2+0.5 = 2.7.
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

// Two-layer chain: 2->2 (W=identity, b=0) then 2->1 (W=[1,1], b=0); input
// (0,5) normalizes to (0,1) => 1.
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

// writeHeaderPrefix writes magic + version + layer count, reused by the
// failure-path test cases below that only test header field validation and
// don't care about the actual weight data.
func writeHeaderPrefix(t *testing.T, buf *bytes.Buffer, ver string, nLayers uint32) {
	buf.WriteString("NAES")
	require.NoError(t, binary.Write(buf, binary.LittleEndian, uint32(len(ver))))
	buf.WriteString(ver)
	require.NoError(t, binary.Write(buf, binary.LittleEndian, nLayers))
}

// TestLoadFromHugeDimNoPanic covers a case found during review: a declared
// dimension of 5000 (exceeding maxDim=4096) with missing weight/bias bytes
// (simulating a malformed/truncated file). Should return an error due to the
// dimension cap before reading any weight bytes, and must not attempt
// make([]float32, in*out), which would cause a huge memory allocation or panic.
func TestLoadFromHugeDimNoPanic(t *testing.T) {
	var buf bytes.Buffer
	writeHeaderPrefix(t, &buf, "v", 1)
	require.NoError(t, binary.Write(&buf, binary.LittleEndian, uint32(5000))) // in, exceeds maxDim
	require.NoError(t, binary.Write(&buf, binary.LittleEndian, uint32(1)))    // out
	// Deliberately write no weight/bias bytes, simulating a truncated file.
	require.NotPanics(t, func() {
		_, err := LoadFrom(bytes.NewReader(buf.Bytes()))
		require.Error(t, err)
	})
}

func TestLoadFromVerLenTooLarge(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteString("NAES")
	require.NoError(t, binary.Write(&buf, binary.LittleEndian, uint32(200))) // exceeds maxVerLen (128)
	_, err := LoadFrom(bytes.NewReader(buf.Bytes()))
	require.Error(t, err)
}

func TestLoadFromZeroLayers(t *testing.T) {
	var buf bytes.Buffer
	writeHeaderPrefix(t, &buf, "v", 0)
	_, err := LoadFrom(bytes.NewReader(buf.Bytes()))
	require.Error(t, err)
}

// TestLoadFromDimChainMismatch covers a dimension mismatch between layers:
// layer0 out=2, layer1 declares in=3.
func TestLoadFromDimChainMismatch(t *testing.T) {
	data := buildHead(t, "v",
		[][2][]float32{
			{{1, 0, 0, 1}, {0, 0}}, // layer0: in=2, out=2
			{{1, 1, 1}, {0}},       // layer1: declares in=3, out=1, doesn't chain with previous out=2
		},
		[][2]uint32{{2, 2}, {3, 1}})
	_, err := LoadFrom(bytes.NewReader(data))
	require.Error(t, err)
}

// TestLoadFromFinalOutNotOne covers the constraint that the final layer's output dimension must be 1.
func TestLoadFromFinalOutNotOne(t *testing.T) {
	data := buildHead(t, "v",
		[][2][]float32{{{1, 2, 3, 4}, {0, 0}}},
		[][2]uint32{{2, 2}})
	_, err := LoadFrom(bytes.NewReader(data))
	require.Error(t, err)
}
