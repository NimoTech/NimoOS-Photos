package service

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// writeFileOfSize 在 dir/name 写出指定字节数的文件。
func writeFileOfSize(t *testing.T, dir, name string, size int) string {
	t.Helper()
	p := filepath.Join(dir, name)
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
	require.NoError(t, os.WriteFile(p, make([]byte, size), 0o644))
	return p
}

func TestStorageStats(t *testing.T) {
	db := makeTestDB(t)
	// 两个资产：一张图片 + 一个视频；另有一个孤儿缩略图目录。
	_, err := db.Exec(`INSERT INTO assets(id, file_path, file_size, mime_type, status)
		VALUES('a1','/x/a.jpg',1000,'image/jpeg','indexed'),
		      ('a2','/x/b.mp4',5000,'video/mp4','indexed')`)
	require.NoError(t, err)

	thumbDir := t.TempDir()
	writeFileOfSize(t, thumbDir, "a1/small.jpg", 100)    // 有效缓存
	writeFileOfSize(t, thumbDir, "ghost/small.jpg", 300) // 孤儿缓存
	faceDir := t.TempDir()
	writeFileOfSize(t, faceDir, "p1.jpg", 50)

	dbFile := writeFileOfSize(t, t.TempDir(), "photos.db", 700)

	s := NewStorageService(db, dbFile, thumbDir, faceDir, t.TempDir())
	st, err := s.Stats()
	require.NoError(t, err)

	require.Equal(t, int64(1000), st.PhotosBytes)
	require.Equal(t, int64(5000), st.VideosBytes)
	require.Equal(t, int64(0), st.RawBytes)
	require.Equal(t, int64(450), st.CacheBytes)   // 100 + 300 + 50
	require.Equal(t, int64(300), st.PrunableBytes) // 仅孤儿目录
	require.Equal(t, int64(700), st.AIBytes)
	require.Greater(t, st.DiskTotalBytes, int64(0))
	require.Greater(t, st.DiskFreeBytes, int64(0))
}

func TestStorageStatsCached(t *testing.T) {
	db := makeTestDB(t)
	s := NewStorageService(db, filepath.Join(t.TempDir(), "photos.db"), t.TempDir(), t.TempDir(), t.TempDir())
	st1, err := s.Stats()
	require.NoError(t, err)
	// 缓存窗口内新插入资产不影响返回值
	_, err = db.Exec(`INSERT INTO assets(id, file_path, file_size, mime_type, status)
		VALUES('a9','/x/c.jpg',1234,'image/jpeg','indexed')`)
	require.NoError(t, err)
	st2, err := s.Stats()
	require.NoError(t, err)
	require.Equal(t, st1.PhotosBytes, st2.PhotosBytes)
	// Invalidate 后重新计算
	s.Invalidate()
	st3, err := s.Stats()
	require.NoError(t, err)
	require.Equal(t, int64(1234), st3.PhotosBytes)
}
