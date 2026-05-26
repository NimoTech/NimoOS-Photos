package service

import (
	"context"
	"database/sql"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func insertAsset(t *testing.T, db *sql.DB, path string, status string) string {
	t.Helper()
	id := uuid.NewString()
	_, err := db.Exec(`
        INSERT INTO assets(id, file_path, file_size, mime_type, original_name,
                           is_live_photo_video, status, checksum)
        VALUES(?,?,?, 'image/jpeg', ?, 0, ?, ?)`,
		id, path, 1, path, status, uuid.NewString())
	require.NoError(t, err)
	return id
}

func insertClipIdx(t *testing.T, db *sql.DB, assetID string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO asset_clip_idx(asset_id) VALUES(?)`, assetID)
	require.NoError(t, err)
}

// TestEmbedder_QueryMissing 只返回 status='indexed' 且 asset_clip_idx 没有行的 asset。
func TestEmbedder_QueryMissing(t *testing.T) {
	db := makeTestDB(t)
	missing := insertAsset(t, db, "/a.jpg", "indexed")
	_ = insertAsset(t, db, "/b.jpg", "pending") // 不该返回
	hasIdx := insertAsset(t, db, "/c.jpg", "indexed")
	insertClipIdx(t, db, hasIdx) // 已有 idx，不该返回

	e := NewEmbedder(db, &mockML{}, nil, nil)
	paths, err := e.queryMissing(context.Background())
	require.NoError(t, err)
	require.Equal(t, []string{"/a.jpg"}, paths)
	_ = missing
}

// TestEmbedder_HasEmbeddingForPath
func TestEmbedder_HasEmbeddingForPath(t *testing.T) {
	db := makeTestDB(t)
	a := insertAsset(t, db, "/x.jpg", "indexed")
	insertClipIdx(t, db, a)
	_ = insertAsset(t, db, "/y.jpg", "indexed")

	e := NewEmbedder(db, &mockML{}, nil, nil)
	require.True(t, e.hasEmbeddingForPath("/x.jpg"))
	require.False(t, e.hasEmbeddingForPath("/y.jpg"))
	require.False(t, e.hasEmbeddingForPath("/nope.jpg"))
}
