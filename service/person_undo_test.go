package service_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/stretchr/testify/require"
)

func seedOnePerson(t *testing.T, db *sql.DB) string {
	t.Helper()
	vec := make([]float32, 512)
	vec[0] = 1.0
	insertAssetFace(t, db, "u-a1", normalize(vec))
	require.NoError(t, service.NewFaceService(db).RunClustering(context.Background()))
	var pid string
	require.NoError(t, db.QueryRow(`SELECT id FROM persons`).Scan(&pid))
	return pid
}

// Undoable delete: hidden immediately with a future purge_at; restore
// cancels the scheduled purge entirely.
func TestHidePersonForPurgeAndRestore(t *testing.T) {
	db := makeTestFaceDB(t)
	pid := seedOnePerson(t, db)
	ps := service.NewPersonService(db)

	require.NoError(t, ps.HidePersonForPurge(pid))
	var hidden int
	var purgeAt sql.NullString
	require.NoError(t, db.QueryRow(
		`SELECT hidden, purge_at FROM persons WHERE id=?`, pid).Scan(&hidden, &purgeAt))
	require.Equal(t, 1, hidden)
	require.True(t, purgeAt.Valid, "purge_at must be scheduled")

	require.NoError(t, ps.RestorePerson(pid))
	require.NoError(t, db.QueryRow(
		`SELECT hidden, purge_at FROM persons WHERE id=?`, pid).Scan(&hidden, &purgeAt))
	require.Equal(t, 0, hidden)
	require.False(t, purgeAt.Valid, "restore must cancel the scheduled purge")

	require.ErrorIs(t, ps.HidePersonForPurge("no-such-id"), service.ErrNotFound)
}

// The sweep purges only overdue persons; future-scheduled ones survive.
func TestPurgeDuePersons(t *testing.T) {
	db := makeTestFaceDB(t)
	pid := seedOnePerson(t, db)
	ps := service.NewPersonService(db)

	// Overdue: set purge_at in the past directly.
	_, err := db.Exec(
		`UPDATE persons SET hidden=1, purge_at=datetime('now', '-1 seconds') WHERE id=?`, pid)
	require.NoError(t, err)
	require.NoError(t, ps.PurgeDuePersons(context.Background()))

	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM persons WHERE id=?`, pid).Scan(&n))
	require.Equal(t, 0, n, "overdue person must be hard-purged")
	// Faces excluded per PurgePerson semantics.
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM face_detections WHERE excluded=1`).Scan(&n))
	require.Equal(t, 1, n)
}

// Not-yet-due persons are untouched by the sweep.
func TestPurgeDuePersonsSkipsFuture(t *testing.T) {
	db := makeTestFaceDB(t)
	pid := seedOnePerson(t, db)
	ps := service.NewPersonService(db)
	require.NoError(t, ps.HidePersonForPurge(pid)) // now+30s, in the future
	require.NoError(t, ps.PurgeDuePersons(context.Background()))
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM persons WHERE id=?`, pid).Scan(&n))
	require.Equal(t, 1, n, "future-scheduled person must survive the sweep")
}
