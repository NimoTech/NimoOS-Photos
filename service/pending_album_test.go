package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestSetPendingAlbum_EmptyIsNoop verifies that SetPendingAlbum with an empty
// albumID leaves the map untouched (takePendingAlbum returns "").
func TestSetPendingAlbum_EmptyIsNoop(t *testing.T) {
	db := makeTestDB(t)
	ix := NewIndexer(db, &mockML{}, t.TempDir(), 1)

	ix.SetPendingAlbum("/gallery/a.jpg", "")
	got := ix.takePendingAlbum("/gallery/a.jpg")
	require.Equal(t, "", got, "empty albumID must not be stored")
}

// TestSetPendingAlbum_StoreAndTakeOnce verifies that a stored albumID is
// returned on the first takePendingAlbum call and "" on the second (consumed).
func TestSetPendingAlbum_StoreAndTake(t *testing.T) {
	db := makeTestDB(t)
	ix := NewIndexer(db, &mockML{}, t.TempDir(), 1)

	ix.SetPendingAlbum("/gallery/b.jpg", "al1")

	got := ix.takePendingAlbum("/gallery/b.jpg")
	require.Equal(t, "al1", got, "first take must return the stored albumID")

	got2 := ix.takePendingAlbum("/gallery/b.jpg")
	require.Equal(t, "", got2, "second take must return empty (entry consumed)")
}

// TestIndexer_AlbumAssigner_CalledAfterProcessing verifies the end-to-end
// pending-album flow: SetPendingAlbum + SetAlbumAssigner → after the worker
// processes the file the assigner callback is called with the correct assetID
// and albumID.
func TestIndexer_AlbumAssigner_CalledAfterProcessing(t *testing.T) {
	db := makeTestDB(t)
	thumbDir := t.TempDir()
	imgDir := t.TempDir()
	imgPath := makeTestJPEG(t, imgDir)

	const wantAlbumID = "album-xyz-999"

	type call struct{ assetID, albumID string }
	calls := make(chan call, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	idx := NewIndexer(db, &mockML{}, thumbDir, 1)
	idx.SetAlbumAssigner(func(assetID, albumID string) {
		calls <- call{assetID, albumID}
	})

	// Register pending album BEFORE submit (MarkAndReserve equivalent in tests).
	idx.SetPendingAlbum(imgPath, wantAlbumID)
	go idx.Start(ctx)
	idx.Enqueue(imgPath)

	// Wait for assigner to be called.
	var got call
	select {
	case got = <-calls:
	case <-time.After(5 * time.Second):
		t.Fatal("albumAssigner was not called within timeout")
	}

	require.Equal(t, wantAlbumID, got.albumID)
	require.NotEmpty(t, got.assetID, "assetID must be non-empty")

	// Confirm the assetID matches what is in the DB.
	var dbID string
	require.NoError(t, db.QueryRow(`SELECT id FROM assets WHERE file_path=?`, imgPath).Scan(&dbID))
	require.Equal(t, dbID, got.assetID, "assigner must receive the actual DB asset ID")
}

// TestIndexer_AlbumAssigner_NilSafe verifies that a nil albumAssigner does not
// panic even when a pending album is registered.
func TestIndexer_AlbumAssigner_NilSafe(t *testing.T) {
	db := makeTestDB(t)
	thumbDir := t.TempDir()
	imgDir := t.TempDir()
	imgPath := makeTestJPEG(t, imgDir)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	idx := NewIndexer(db, &mockML{}, thumbDir, 1)
	// No SetAlbumAssigner call — assigner stays nil.
	idx.SetPendingAlbum(imgPath, "some-album")
	go idx.Start(ctx)
	idx.Enqueue(imgPath)

	// Just wait for indexed; should not panic.
	require.Eventually(t, func() bool {
		var s string
		_ = db.QueryRow(`SELECT status FROM assets WHERE file_path=?`, imgPath).Scan(&s)
		return s == "indexed"
	}, 5*time.Second, 50*time.Millisecond)
}
