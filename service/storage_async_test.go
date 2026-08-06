package service_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/stretchr/testify/require"
)

func seedStorageAssets(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, row := range []struct {
		id, path, mime string
		size           int64
	}{
		{"s1", "/g/a.jpg", "image/jpeg", 100},
		{"s2", "/g/b.mp4", "video/mp4", 200},
		{"s3", "/g/c.dng", "image/x-adobe-dng", 400},
	} {
		_, err := db.Exec(`INSERT INTO assets (id, file_path, file_size, mime_type, status, is_live_photo_video, offline)
			VALUES (?,?,?,?, 'indexed', 0, 0)`, row.id, row.path, row.size, row.mime)
		require.NoError(t, err)
	}
}

// DB-derived buckets must be correct without any filesystem walk.
func TestStatsAggregatesFromSQL(t *testing.T) {
	db := openPerfDB(t)
	seedStorageAssets(t, db)
	dir := t.TempDir()
	s := service.NewStorageService(db, filepath.Join(dir, "photos.db"),
		filepath.Join(dir, "thumbs"), filepath.Join(dir, "faces"), dir)

	st, err := s.Stats()
	require.NoError(t, err)
	require.Equal(t, int64(100), st.PhotosBytes)
	require.Equal(t, int64(200), st.VideosBytes)
	require.Equal(t, int64(400), st.RawBytes)
}

// The thumbs walk must land asynchronously: first call may report 0 cache,
// but a subsequent call (after the background refresh finishes) reports it.
func TestStatsCacheBytesRefreshAsync(t *testing.T) {
	db := openPerfDB(t)
	seedStorageAssets(t, db)
	dir := t.TempDir()
	thumbs := filepath.Join(dir, "thumbs", "s1")
	require.NoError(t, os.MkdirAll(thumbs, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(thumbs, "small.jpg"), make([]byte, 1234), 0o644))
	s := service.NewStorageService(db, filepath.Join(dir, "photos.db"),
		filepath.Join(dir, "thumbs"), filepath.Join(dir, "faces"), dir)

	_, err := s.Stats() // kicks the background walk
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		st, err := s.Stats()
		return err == nil && st.CacheBytes == 1234
	}, 5*time.Second, 50*time.Millisecond)
}
