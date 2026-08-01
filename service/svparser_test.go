package service

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	"github.com/stretchr/testify/require"
)

func parserDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "p.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	_, _ = db.Exec(`INSERT INTO persons(id,name) VALUES('p-sara','Sara')`)
	_, _ = db.Exec(`INSERT INTO assets(id,file_path,status) VALUES('a1','/p/a.jpg','indexed')`)
	_, _ = db.Exec(`INSERT INTO asset_geo(asset_id,city_id,city,country,region) VALUES('a1',1,'Tokyo','Japan','Kanto')`)
	return db
}

func TestParseConditions(t *testing.T) {
	d := parserDB(t)
	got := ParseConditions(d, []string{
		"Sara", "Tokyo, Japan", "scene: sunset", "year: 2025",
		"OCR: receipt | invoice", "a red bicycle",
	})
	require.Len(t, got, 6)
	require.Equal(t, "person", got[0].Kind)
	require.Equal(t, "p-sara", got[0].Value)
	require.Equal(t, "place", got[1].Kind)
	require.Equal(t, "semantic", got[2].Kind)
	require.Equal(t, "sunset", got[2].Value)
	require.Equal(t, "date", got[3].Kind)
	require.NotNil(t, got[3].Start)
	require.Equal(t, 2025, got[3].Start.Year())
	require.Equal(t, 2025, got[3].End.Year())
	require.Equal(t, "ocr", got[4].Kind)
	require.Equal(t, "receipt | invoice", got[4].Value)
	require.Equal(t, "semantic", got[5].Kind)
	require.Equal(t, "a red bicycle", got[5].Value)
}

func TestParseOCRCondition(t *testing.T) {
	d := parserDB(t)
	got := ParseConditions(d, []string{"ocr: TOTAL", "OCR: receipt | invoice", "amount detected"})
	require.Equal(t, "ocr", got[0].Kind)
	require.Equal(t, "TOTAL", got[0].Value)
	require.Equal(t, "ocr", got[1].Kind)
	require.Equal(t, "receipt | invoice", got[1].Value)
	// The remaining legacy suggestion word without the ocr: prefix is still unsupported
	require.Equal(t, "unsupported", got[2].Kind)
}

// A bare year (e.g. "2024" typed in the search box) should be understood as
// a date condition, consistent with the search pipeline's "Nimo understood:
// date" behavior, rather than falling into a CLIP semantic query.
func TestParseBareYear(t *testing.T) {
	d := parserDB(t)
	got := ParseConditions(d, []string{"2024", "1985", "3024"})
	require.Equal(t, "date", got[0].Kind)
	require.Equal(t, 2024, got[0].Start.Year())
	require.Equal(t, time.January, got[0].Start.Month())
	require.Equal(t, 2024, got[0].End.Year())
	require.Equal(t, time.December, got[0].End.Month())
	require.Equal(t, "date", got[1].Kind)
	// A 4-digit number outside a reasonable year range still falls back to semantic search
	require.Equal(t, "semantic", got[2].Kind)
}

func TestParseDateRangeAbsolute(t *testing.T) {
	d := parserDB(t)
	got := ParseConditions(d, []string{"Mar 14 – 22, 2026"})
	require.Equal(t, "date", got[0].Kind)
	require.Equal(t, time.March, got[0].Start.Month())
	require.Equal(t, 14, got[0].Start.Day())
	require.Equal(t, 22, got[0].End.Day())
}

func TestParseScoreVariantsUnsupported(t *testing.T) {
	d := parserDB(t)
	for _, raw := range []string{
		"score>=80", "score >= 80", "score≥80", "score>80", "score = 80", "SCORE>=80",
		"score 80", // bare-number form with no operator, caught by the old "score " prefix enumeration
	} {
		got := ParseConditions(d, []string{raw})
		require.Len(t, got, 1)
		require.Equal(t, condUnsupported, got[0].Kind, "raw=%q should be blocked as unsupported rather than falling into semantic query", raw)
	}
	// Counter-examples: ordinary semantic words starting with "score" must not be falsely blocked
	for _, raw := range []string{
		"scoreboard at the stadium",
		"score of the game",
	} {
		got := ParseConditions(d, []string{raw})
		require.Len(t, got, 1)
		require.NotEqual(t, condUnsupported, got[0].Kind, "raw=%q should not be falsely blocked as unsupported", raw)
	}
}
