package service_test

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	"github.com/stretchr/testify/require"
)

// openPerfDB opens a fresh temp SQLite with the full production schema.
func openPerfDB(t testing.TB) *sql.DB {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "perf.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	return db
}

// seedPerfAssets bulk-inserts n indexed assets spaced 3h apart (~4 month
// buckets per 1000 assets, ~34 years at 100k) so bucket/pagination queries
// exercise realistic group counts. Odd rows are videos, every 50th row is
// trashed (must be excluded by timeline queries).
func seedPerfAssets(t testing.TB, db *sql.DB, n int) {
	t.Helper()
	tx, err := db.Begin()
	require.NoError(t, err)
	stmtA, err := tx.Prepare(`INSERT INTO assets
		(id, file_path, file_size, mime_type, taken_at, indexed_at, status, is_live_photo_video, offline)
		VALUES (?,?,?,?,?,?, 'indexed', 0, 0)`)
	require.NoError(t, err)
	stmtE, err := tx.Prepare(`INSERT INTO asset_exif (asset_id, width, height) VALUES (?, 4000, 3000)`)
	require.NoError(t, err)
	base := time.Date(2018, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("perf-%07d", i)
		mime := "image/jpeg"
		if i%2 == 1 {
			mime = "video/mp4"
		}
		taken := base.Add(time.Duration(i) * 3 * time.Hour)
		_, err = stmtA.Exec(id, "/g/"+id+".jpg", int64(3_000_000), mime, taken, taken)
		require.NoError(t, err)
		_, err = stmtE.Exec(id)
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())
	// Trash every 50th asset in one statement (deleted_at is an ALTER column).
	_, err = db.Exec(`UPDATE assets SET deleted_at = CURRENT_TIMESTAMP
		WHERE CAST(substr(id, 6) AS INTEGER) % 50 = 0 AND id LIKE 'perf-%'`)
	require.NoError(t, err)
}

func TestSeedPerfAssetsSmoke(t *testing.T) {
	db := openPerfDB(t)
	seedPerfAssets(t, db, 500)
	var total, trashed int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM assets`).Scan(&total))
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM assets WHERE deleted_at IS NOT NULL`).Scan(&trashed))
	require.Equal(t, 500, total)
	require.Equal(t, 10, trashed)
}
