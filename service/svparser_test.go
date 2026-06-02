package service

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
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
	require.Equal(t, "unsupported", got[4].Kind)
	require.Equal(t, "semantic", got[5].Kind)
	require.Equal(t, "a red bicycle", got[5].Value)
}

func TestParseDateRangeAbsolute(t *testing.T) {
	d := parserDB(t)
	got := ParseConditions(d, []string{"Mar 14 – 22, 2026"})
	require.Equal(t, "date", got[0].Kind)
	require.Equal(t, time.March, got[0].Start.Month())
	require.Equal(t, 14, got[0].Start.Day())
	require.Equal(t, 22, got[0].End.Day())
}
