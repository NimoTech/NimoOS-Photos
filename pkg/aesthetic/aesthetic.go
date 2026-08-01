// Package aesthetic computes an aesthetic score by running a small linear
// head on top of an existing CLIP (SigLIP2) image vector.
// Pure local matrix multiply, microsecond-scale per image, no dependency on
// the ML service. See the LoadFrom function comment for the weight format.
package aesthetic

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
)

const (
	// maxVerLen is a defensive cap on the version string length in bytes,
	// far larger than any real version string (e.g. "v-test", "v2.5").
	maxVerLen = 128
	// maxLayers is a defensive cap on the number of layers in the linear
	// head, far larger than the real structure (1152->1024->128->64->16->1, 5 layers).
	maxLayers = 16
	// maxDim is a defensive cap on the input/output dimension of a single
	// layer. The real head's dimension chain is 1152->1024->128->64->16->1,
	// well below this value; the cap exists to reject an oversized dimension
	// declared by a malformed/truncated file before reading the weight bytes,
	// preventing make([]float32, in*out) from allocating GiB-scale memory
	// for a single layer.
	maxDim = 4096
)

type layer struct {
	in, out int
	w       []float32 // row-major [out][in]
	b       []float32
}

// Head is a fully loaded aesthetic scoring head. Safe for concurrent read-only use.
type Head struct {
	ver    string
	layers []layer
}

func (h *Head) Version() string { return h.ver }
func (h *Head) InDim() int      { return h.layers[0].in }

// LoadFrom parses the weight byte stream. Returns an error on any structural mismatch.
//
// Weight file format (little-endian):
//
//	magic   [4]byte   fixed "NAES"
//	verLen  uint32    version string length in bytes, capped at maxVerLen
//	ver     [verLen]byte  version string, e.g. "v2.5"
//	nLayers uint32    layer count, range [1, maxLayers]
//	layers  repeated nLayers times:
//	    in      uint32        this layer's input dimension, range (0, maxDim]
//	    out     uint32        this layer's output dimension, range (0, maxDim]
//	    weights [in*out]float32  row-major [out][in]
//	    bias    [out]float32
//
// Constraint: adjacent layers' in/out must chain (previous layer's out ==
// next layer's in), and the final layer's out must be 1.
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
	if verLen > maxVerLen {
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
	if nLayers == 0 || nLayers > maxLayers {
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
		if in == 0 || out == 0 || in > maxDim || out > maxDim {
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

// Score scores an image vector: L2-normalize first (v2.5 inference
// convention), then apply y=Wx+b layer by layer.
// Returns NaN on a dimension mismatch (the caller skips that asset).
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
