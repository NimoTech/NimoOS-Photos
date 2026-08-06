package service_test

import (
	"fmt"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/stretchr/testify/require"
)

// TestListTrashPagination verifies ListTrash's new limit/offset parameters
// page through the trash, newest deletion first.
func TestListTrashPagination(t *testing.T) {
	db := openPerfDB(t)
	tx, err := db.Begin()
	require.NoError(t, err)
	for i := 0; i < 30; i++ {
		_, err = tx.Exec(`INSERT INTO assets (id, file_path, status, is_live_photo_video, offline, deleted_at)
			VALUES (?, ?, 'indexed', 0, 0, datetime('now', ?))`,
			fmt.Sprintf("tr-%03d", i), fmt.Sprintf("/g/tr-%03d.jpg", i), fmt.Sprintf("-%d minutes", i))
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())

	ts := service.NewTrashService(db, t.TempDir(), t.TempDir()) // align ctor args with existing trash tests
	page1, err := ts.ListTrash("u1", 10, 0)
	require.NoError(t, err)
	require.Len(t, page1, 10)
	require.Equal(t, "tr-000", page1[0].ID, "newest deletion first")

	page2, err := ts.ListTrash("u1", 10, 10)
	require.NoError(t, err)
	require.Len(t, page2, 10)
	require.Equal(t, "tr-010", page2[0].ID)
}
