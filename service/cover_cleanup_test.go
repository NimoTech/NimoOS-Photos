package service_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/stretchr/testify/require"
)

// Detaching every asset from an anchored person must clear its cover so the
// face-thumbnail endpoint doesn't keep pointing at detached faces.
func TestDetachAllClearsCover(t *testing.T) {
	db := makeTestFaceDB(t)
	vec := make([]float32, 512)
	vec[0] = 1.0
	insertAssetFace(t, db, "a1", normalize(vec))
	require.NoError(t, service.NewFaceService(db).RunClustering(context.Background()))

	var pid string
	require.NoError(t, db.QueryRow(`SELECT id FROM persons`).Scan(&pid))
	_, err := db.Exec(`UPDATE persons SET name='Alice' WHERE id=?`, pid)
	require.NoError(t, err)

	ps := service.NewPersonService(db)
	removed, err := ps.DetachAssetsFromPerson(pid, []string{"a1"})
	require.NoError(t, err)
	require.Equal(t, 1, removed)

	var cover, centroid sql.NullString
	require.NoError(t, db.QueryRow(
		`SELECT cover_face_id, centroid FROM persons WHERE id=?`, pid).Scan(&cover, &centroid))
	require.False(t, cover.Valid && cover.String != "", "cover must be cleared")
}

// After an asset hard-delete cascades face rows away, the minute-level
// self-heal must null out the now-dangling persons.cover_face_id.
func TestClearDanglingCovers(t *testing.T) {
	db := makeTestFaceDB(t)
	vec := make([]float32, 512)
	vec[0] = 1.0
	insertAssetFace(t, db, "a1", normalize(vec))
	fs := service.NewFaceService(db)
	require.NoError(t, fs.RunClustering(context.Background()))

	var pid string
	require.NoError(t, db.QueryRow(`SELECT id FROM persons`).Scan(&pid))
	_, err := db.Exec(`UPDATE persons SET name='Alice' WHERE id=?`, pid)
	require.NoError(t, err)
	_, err = db.Exec(`DELETE FROM assets WHERE id='a1'`) // FK cascades face_detections/face_person
	require.NoError(t, err)

	require.NoError(t, fs.ClearDanglingCovers(context.Background()))

	var cover sql.NullString
	require.NoError(t, db.QueryRow(
		`SELECT cover_face_id FROM persons WHERE id=?`, pid).Scan(&cover))
	require.False(t, cover.Valid && cover.String != "", "dangling cover must be healed")
}
