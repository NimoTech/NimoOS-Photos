package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// regularBoxes builds a regular document layout: n lines, horizontal, equal
// height, left-aligned, equal line spacing.
func regularBoxes(n int) [][]float64 {
	out := make([][]float64, 0, n)
	for i := 0; i < n; i++ {
		y := 0.1 + float64(i)*0.06
		out = append(out, []float64{0.1, y, 0.8, y, 0.8, y + 0.04, 0.1, y + 0.04})
	}
	return out
}

// scatteredBoxes builds scattered street-scene text: angle, line height, and
// horizontal position are all inconsistent.
func scatteredBoxes() [][]float64 {
	return [][]float64{
		{0.05, 0.10, 0.40, 0.18, 0.39, 0.28, 0.04, 0.20}, // large slanted text
		{0.55, 0.50, 0.70, 0.50, 0.70, 0.53, 0.55, 0.53}, // small horizontal text
		{0.20, 0.70, 0.60, 0.62, 0.61, 0.74, 0.21, 0.82}, // reverse-slanted text
		{0.80, 0.30, 0.95, 0.30, 0.95, 0.42, 0.80, 0.42}, // tall narrow block
	}
}

func TestDocGeoScore(t *testing.T) {
	reg := docGeoScore(regularBoxes(10))
	scat := docGeoScore(scatteredBoxes())
	require.Greater(t, reg, 0.8, "a regular layout should score high, got %v", reg)
	require.Less(t, scat, 0.5, "scattered text should score low, got %v", scat)
	require.Greater(t, reg, scat+0.3, "the two must be sufficiently distinguishable")

	require.InDelta(t, 0.5, docGeoScore(regularBoxes(2)), 1e-9, "<3 lines returns neutral 0.5")
	require.InDelta(t, 0.5, docGeoScore(nil), 1e-9)
	// Invalid boxes (length != 8) are skipped; all-invalid is equivalent to 0 lines → neutral
	require.InDelta(t, 0.5, docGeoScore([][]float64{{0.1, 0.2}}), 1e-9)
}

func TestDocSemMargin(t *testing.T) {
	e1 := make([]float32, 4)
	e1[0] = 1
	e2 := make([]float32, 4)
	e2[1] = 1
	mix := []float32{0.9, 0.1, 0, 0} // close to e1

	// Image close to the doc vector → positive margin; close to the photo vector → negative margin
	require.Greater(t, docSemMargin(mix, [][]float32{e1}, [][]float32{e2}), 0.0)
	require.Less(t, docSemMargin(mix, [][]float32{e2}, [][]float32{e1}), 0.0)
	// Multiple prompts: take each group's max
	require.Greater(t,
		docSemMargin(mix, [][]float32{e2, e1}, [][]float32{e2}), 0.0)
	// Safe on empty groups: returns 0 (neutral)
	require.InDelta(t, 0.0, docSemMargin(mix, nil, nil), 1e-9)
}

func TestDocVerdict(t *testing.T) {
	// Default config (used when config.Cfg is nil): wSem=0.65 wGeo=0.35 floor=0.5
	// semFloor=-0.01 semCeil=0.05 → margin 0.05 normalizes to 1.0
	require.True(t, docVerdict(0.05, 1.0), "strongly document-like semantics + regular geometry → document")
	require.False(t, docVerdict(-0.05, 0.2), "strongly photo-like semantics + scattered geometry → vetoed")
	// Neutral semantics (0.02 normalizes to 0.5) + neutral geometry 0.5 → weighted 0.5, passes >=floor
	require.True(t, docVerdict(0.02, 0.5))
	// Clearly photo-like semantics (normalizes to 0) can't be pulled back: 0.65*0 + 0.35*0.5 = 0.175 < 0.5
	require.False(t, docVerdict(-0.01, 0.5))
}

// TestHasOcrExprTriState verifies the tri-state semantics of the shared
// criterion fragment (via a real ListAssets query): is_doc=1 → hasOcr;
// is_doc=0 (density passes but vetoed) → not hasOcr; is_doc NULL → falls back
// to the old density criterion.
func TestHasOcrExprTriState(t *testing.T) {
	db := makeTestDB(t)
	s := NewSearchService(db, nil)

	mk := func(id string, coverage float64, lines int, isDoc any) {
		_, err := db.Exec(`INSERT INTO assets(id,file_path,mime_type,status) VALUES(?,?, 'image/jpeg','indexed')`, id, "/p/"+id+".jpg")
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO asset_ocr(asset_id,text,coverage,line_count,is_doc) VALUES(?,'x',?,?,?)`, id, coverage, lines, isDoc)
		require.NoError(t, err)
	}
	mk("verdict1", 0.1, 20, 1)      // classified as a document
	mk("vetoed", 0.1, 20, 0)        // density passes but semantically vetoed
	mk("legacyDoc", 0.1, 20, nil)   // not computed → old criterion: passes
	mk("legacyPhoto", 0.01, 2, nil) // not computed → old criterion: fails
	mk("rescued", 0.01, 2, 1)       // is_doc=1 but density gate fails → must not enter the OCR class (gate can't be bypassed)

	assets, err := s.ListAssets("", 100, 0)
	require.NoError(t, err)
	got := map[string]bool{}
	for _, a := range assets {
		got[a.ID] = a.HasOCR
	}
	require.True(t, got["verdict1"])
	require.False(t, got["vetoed"], "a vetoed asset must not enter the OCR class — the core goal of this feature")
	require.True(t, got["legacyDoc"], "not-yet-computed falls back to the old density criterion")
	require.False(t, got["legacyPhoto"])
	require.False(t, got["rescued"], "is_doc=1 cannot bypass the density gate (no rescue path)")
}
