package sqlite

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenCreatesTimelineIndexesAndCapsPool(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "t.db"))
	require.NoError(t, err)
	defer db.Close()

	for _, idx := range []string{"idx_assets_sortkey", "idx_assets_monthkey"} {
		var n int
		require.NoError(t, db.QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?`, idx).Scan(&n))
		require.Equal(t, 1, n, "index %s must exist", idx)
	}
	require.Equal(t, 8, db.Stats().MaxOpenConnections)
}
