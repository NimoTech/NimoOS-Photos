package service_test

import (
	"path/filepath"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/stretchr/testify/require"
)

func openTestViewsSvc(t *testing.T) (*service.ViewsService, func()) {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	for _, id := range []string{"a1", "a2"} {
		_, err := db.Exec(`INSERT INTO assets(id, file_path, status) VALUES(?, ?, 'indexed')`, id, "/x/"+id+".jpg")
		require.NoError(t, err)
	}
	return service.NewViewsService(db), func() { db.Close() }
}

func countView(t *testing.T, svc *service.ViewsService, userID, assetID string) int {
	t.Helper()
	n, err := svc.Count(userID, assetID)
	require.NoError(t, err)
	return n
}

func TestRecordFirstViewIsOne(t *testing.T) {
	svc, cleanup := openTestViewsSvc(t)
	defer cleanup()

	require.NoError(t, svc.Record("default", "a1"))
	require.Equal(t, 1, countView(t, svc, "default", "a1"))
}

func TestRecordIncrements(t *testing.T) {
	svc, cleanup := openTestViewsSvc(t)
	defer cleanup()

	require.NoError(t, svc.Record("default", "a1"))
	require.NoError(t, svc.Record("default", "a1"))
	require.NoError(t, svc.Record("default", "a1"))
	require.Equal(t, 3, countView(t, svc, "default", "a1"))
}

func TestRecordUnknownAsset(t *testing.T) {
	svc, cleanup := openTestViewsSvc(t)
	defer cleanup()

	require.ErrorIs(t, svc.Record("default", "nonexistent"), service.ErrNotFound)
}

func TestRecordPerUserIsolated(t *testing.T) {
	svc, cleanup := openTestViewsSvc(t)
	defer cleanup()

	require.NoError(t, svc.Record("u1", "a1"))
	require.NoError(t, svc.Record("u1", "a1"))
	require.NoError(t, svc.Record("u2", "a1"))
	require.Equal(t, 2, countView(t, svc, "u1", "a1"))
	require.Equal(t, 1, countView(t, svc, "u2", "a1"))
}
