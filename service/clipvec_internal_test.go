package service

import (
	"path/filepath"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	"github.com/stretchr/testify/require"
)

// pruneOrphanClipVectors must delete vec0 rows with no asset_clip_idx mapping
// while leaving mapped (valid) vectors untouched — a cheap no-ML safety net.
func TestPruneOrphanClipVectors(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "o.db"))
	require.NoError(t, err)
	defer db.Close()

	vec := make([]float32, 512)
	vec[0] = 1.0
	blob := sqlite.SerializeFloat32(vec)

	// Valid: asset + mapping + vector.
	db.Exec(`INSERT INTO assets(id,file_path,status,is_live_photo_video) VALUES('a1','/p/a1.jpg','indexed',0)`)
	db.Exec(`INSERT INTO asset_clip_idx(asset_id) VALUES('a1')`)
	var validRow int64
	require.NoError(t, db.QueryRow(`SELECT rowid FROM asset_clip_idx WHERE asset_id='a1'`).Scan(&validRow))
	_, err = db.Exec(`INSERT INTO clip_embeddings(rowid,embedding) VALUES(?,?)`, validRow, blob)
	require.NoError(t, err)

	// Orphan: vector with a rowid that has no asset_clip_idx entry.
	_, err = db.Exec(`INSERT INTO clip_embeddings(rowid,embedding) VALUES(999999,?)`, blob)
	require.NoError(t, err)

	pruneOrphanClipVectors(db)

	var total, orphan int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM clip_embeddings`).Scan(&total))
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM clip_embeddings WHERE rowid=999999`).Scan(&orphan))
	require.Equal(t, 1, total, "only the valid vector remains")
	require.Equal(t, 0, orphan, "orphan vector swept")
}
