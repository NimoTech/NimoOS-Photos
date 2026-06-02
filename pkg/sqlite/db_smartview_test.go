package sqlite_test

import (
	"path/filepath"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	"github.com/stretchr/testify/require"
)

func TestSmartViewTablesExist(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "sv.db"))
	require.NoError(t, err)
	defer db.Close()

	_, err = db.Exec(`INSERT INTO smart_views(id,name,conds_raw,conds_parsed,threshold)
		VALUES('sv-1','t','[]','[]',70)`)
	require.NoError(t, err)

	_, err = db.Exec(`INSERT INTO assets(id,file_path,status) VALUES('a1','/p/a.jpg','indexed')`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO smart_view_matches(smart_view_id,asset_id,match_score)
		VALUES('sv-1','a1',0.9)`)
	require.NoError(t, err)

	_, err = db.Exec(`INSERT INTO smart_view_activity(id,smart_view_id,event_type)
		VALUES('ev-1','sv-1','created')`)
	require.NoError(t, err)

	_, err = db.Exec(`DELETE FROM smart_views WHERE id='sv-1'`)
	require.NoError(t, err)
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM smart_view_matches`).Scan(&n))
	require.Equal(t, 0, n)
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM smart_view_activity`).Scan(&n))
	require.Equal(t, 0, n)
}
