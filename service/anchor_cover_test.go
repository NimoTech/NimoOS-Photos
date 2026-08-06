package service_test

import (
	"context"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/stretchr/testify/require"
)

// A user-pinned cover (cover_locked=1) must anchor an otherwise unnamed
// person across re-clustering: same person id, same cover face.
func TestReclusterKeepsCoverLockedPerson(t *testing.T) {
	db := makeTestFaceDB(t)
	vec := make([]float32, 512)
	vec[0] = 1.0
	insertAssetFace(t, db, "a1", normalize(vec))
	insertAssetFace(t, db, "a2", normalize(vec))
	fs := service.NewFaceService(db)
	require.NoError(t, fs.RunClustering(context.Background()))

	var pid, coverFace string
	require.NoError(t, db.QueryRow(
		`SELECT id, cover_face_id FROM persons`).Scan(&pid, &coverFace))
	_, err := db.Exec(`UPDATE persons SET cover_locked=1 WHERE id=?`, pid)
	require.NoError(t, err)

	require.NoError(t, fs.RunClustering(context.Background()))

	var pid2, coverFace2 string
	require.NoError(t, db.QueryRow(
		`SELECT id, cover_face_id FROM persons`).Scan(&pid2, &coverFace2))
	require.Equal(t, pid, pid2, "cover-locked person must survive re-clustering")
	require.Equal(t, coverFace, coverFace2, "pinned cover must be preserved")
}

// Same for a user-chosen hero background.
func TestReclusterKeepsHeroPerson(t *testing.T) {
	db := makeTestFaceDB(t)
	vec := make([]float32, 512)
	vec[0] = 1.0
	insertAssetFace(t, db, "a1", normalize(vec))
	fs := service.NewFaceService(db)
	require.NoError(t, fs.RunClustering(context.Background()))

	var pid string
	require.NoError(t, db.QueryRow(`SELECT id FROM persons`).Scan(&pid))
	_, err := db.Exec(`UPDATE persons SET hero_asset_id='a1' WHERE id=?`, pid)
	require.NoError(t, err)

	require.NoError(t, fs.RunClustering(context.Background()))
	var pid2 string
	require.NoError(t, db.QueryRow(`SELECT id FROM persons`).Scan(&pid2))
	require.Equal(t, pid, pid2, "hero person must survive re-clustering")
}
