package service_test

import (
	"path/filepath"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/stretchr/testify/require"
)

// Deleting an asset must also drop its CLIP vector from the sqlite-vec table.
// The FK cascade only reaches asset_clip_idx, not the vec0 clip_embeddings rows,
// so without dropClipVector the deleted asset left an orphan vector that still
// occupied KNN top-k slots and degraded search. This guards that fix.
func TestRemoveByPathDropsClipVector(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "c.db"))
	require.NoError(t, err)
	defer db.Close()

	db.Exec(`INSERT INTO assets(id,file_path,status,is_live_photo_video) VALUES('a1','/p/x.jpg','indexed',0)`)
	db.Exec(`INSERT INTO asset_clip_idx(asset_id) VALUES('a1')`)
	var rowid int64
	require.NoError(t, db.QueryRow(`SELECT rowid FROM asset_clip_idx WHERE asset_id='a1'`).Scan(&rowid))
	vec := make([]float32, 512)
	vec[0] = 1.0
	_, err = db.Exec(`INSERT INTO clip_embeddings(rowid,embedding) VALUES(?,?)`, rowid, sqlite.SerializeFloat32(vec))
	require.NoError(t, err)

	idx := service.NewIndexer(db, nil, t.TempDir(), 1)
	idx.RemoveByPath("/p/x.jpg")

	var assetN, clipN int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM assets`).Scan(&assetN))
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM clip_embeddings`).Scan(&clipN))
	require.Equal(t, 0, assetN, "asset row removed")
	require.Equal(t, 0, clipN, "orphan CLIP vector removed")
}

// Deleting from the album UI goes through trash -> PurgeAsset; it must also drop
// the CLIP vector (this was the path the user asked about).
func TestPurgeAssetDropsClipVector(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "t.db"))
	require.NoError(t, err)
	defer db.Close()

	// Trashed asset (deleted_at set) with a CLIP vector.
	db.Exec(`INSERT INTO assets(id,file_path,status,is_live_photo_video,deleted_at) VALUES('a1','/p/x.jpg','indexed',0,CURRENT_TIMESTAMP)`)
	db.Exec(`INSERT INTO asset_clip_idx(asset_id) VALUES('a1')`)
	var rowid int64
	require.NoError(t, db.QueryRow(`SELECT rowid FROM asset_clip_idx WHERE asset_id='a1'`).Scan(&rowid))
	vec := make([]float32, 512)
	vec[0] = 1.0
	_, err = db.Exec(`INSERT INTO clip_embeddings(rowid,embedding) VALUES(?,?)`, rowid, sqlite.SerializeFloat32(vec))
	require.NoError(t, err)

	trash := service.NewTrashService(db, "/DATA/Gallery", t.TempDir())
	require.NoError(t, trash.PurgeAsset("a1"))

	var clipN int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM clip_embeddings`).Scan(&clipN))
	require.Equal(t, 0, clipN, "purged asset's CLIP vector removed")
}
