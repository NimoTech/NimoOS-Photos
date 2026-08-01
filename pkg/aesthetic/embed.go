package aesthetic

import (
	"bytes"
	_ "embed"
)

// Weights are produced by the conversion/training scripts under
// scripts/aesthetic/ (NAES format). When swapping heads, replace this file
// and make sure the version string changes (aesthetic_head_ver relies on it
// to trigger a full-library rescore).
//
//go:embed weights/head_v1.bin
var embeddedWeights []byte

// Load parses the aesthetic scoring head embedded in the binary.
func Load() (*Head, error) {
	return LoadFrom(bytes.NewReader(embeddedWeights))
}
