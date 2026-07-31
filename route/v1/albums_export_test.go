package v1_test

import (
	"archive/zip"
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"

	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	v1 "github.com/NimoTech/NimoOS-Photos/route/v1"
	"github.com/NimoTech/NimoOS-Photos/service"
)

// 手动相册 ZIP 下载端点测试。与 favorites 的 Export 同构：
// GET + token query 参数（浏览器 window.location.href 导航发不出 Authorization
// 头，photos.go router 的 JWT Skipper 靠"/albums/:id/export"后缀放行，handler
// 内部自行校验 token），runtimePath=="" 时为测试/单机直连场景，跳过真实 JWT
// 校验（同 favorites_test.go 的 newFavHarness 约定）。

// TestAlbumExportZipHandler 验证成功路径：相册下全部成员流式打包进 zip，
// Content-Type/Content-Disposition 与文件内容均正确。
func TestAlbumExportZipHandler(t *testing.T) {
	dir := t.TempDir()
	fileA := filepath.Join(dir, "alpha.jpg")
	fileB := filepath.Join(dir, "beta.jpg")
	require.NoError(t, os.WriteFile(fileA, []byte("AAAA"), 0o644))
	require.NoError(t, os.WriteFile(fileB, []byte("BBBB"), 0o644))

	db, err := sqlite.Open(filepath.Join(dir, "test.db"))
	require.NoError(t, err)
	defer db.Close()
	_, err = db.Exec(`INSERT INTO albums(id, name) VALUES('al1', 'Trip')`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO assets(id, file_path, original_name, status) VALUES('a1', ?, 'alpha.jpg', 'indexed')`, fileA)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO assets(id, file_path, original_name, status) VALUES('a2', ?, 'beta.jpg', 'indexed')`, fileB)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO album_assets(album_id, asset_id, position) VALUES('al1','a1',0)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO album_assets(album_id, asset_id, position) VALUES('al1','a2',1)`)
	require.NoError(t, err)

	h := v1.NewAlbumsHandler(service.NewTestServices(db), "")

	req := httptest.NewRequest(http.MethodGet, "/v1/photos/albums/al1/export?token=t", nil)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues("al1")

	require.NoError(t, h.Export(c))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "application/zip", rec.Header().Get("Content-Type"))
	require.Contains(t, rec.Header().Get("Content-Disposition"), "album-")

	zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	require.NoError(t, err)
	require.Len(t, zr.File, 2)
	names := []string{zr.File[0].Name, zr.File[1].Name}
	require.ElementsMatch(t, []string{"alpha.jpg", "beta.jpg"}, names)

	for _, zf := range zr.File {
		rc, err := zf.Open()
		require.NoError(t, err)
		data, _ := io.ReadAll(rc)
		rc.Close()
		if zf.Name == "alpha.jpg" {
			require.Equal(t, "AAAA", string(data))
		} else {
			require.Equal(t, "BBBB", string(data))
		}
	}
}

// TestAlbumExportZipNotFound 相册不存在应 404。
func TestAlbumExportZipNotFound(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	defer db.Close()

	h := v1.NewAlbumsHandler(service.NewTestServices(db), "")
	req := httptest.NewRequest(http.MethodGet, "/v1/photos/albums/nope/export?token=t", nil)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues("nope")

	err = h.Export(c)
	httpErr, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusNotFound, httpErr.Code)
}

// TestAlbumExportZipEmpty 相册存在但无(可见)成员应 400。
func TestAlbumExportZipEmpty(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	defer db.Close()
	_, err = db.Exec(`INSERT INTO albums(id, name) VALUES('al1', 'Empty')`)
	require.NoError(t, err)

	h := v1.NewAlbumsHandler(service.NewTestServices(db), "")
	req := httptest.NewRequest(http.MethodGet, "/v1/photos/albums/al1/export?token=t", nil)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues("al1")

	err = h.Export(c)
	httpErr, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusBadRequest, httpErr.Code)
}

// TestAlbumExportZipMissingToken 缺 token query 参数应 401(与 favorites 同款
// 校验:浏览器导航无法带 Authorization 头,只能靠 query token 兜底)。
func TestAlbumExportZipMissingToken(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	defer db.Close()
	_, err = db.Exec(`INSERT INTO albums(id, name) VALUES('al1', 'Trip')`)
	require.NoError(t, err)

	h := v1.NewAlbumsHandler(service.NewTestServices(db), "")
	req := httptest.NewRequest(http.MethodGet, "/v1/photos/albums/al1/export", nil)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues("al1")

	err = h.Export(c)
	httpErr, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusUnauthorized, httpErr.Code)
}

// TestAlbumExportZipDeduplicatesNames 两个成员同 original_name 时,zip 内条目
// 名照 favorites 既有 dedupZipEntryName 规则加 "-2" 后缀去重。
func TestAlbumExportZipDeduplicatesNames(t *testing.T) {
	dir := t.TempDir()
	fileA := filepath.Join(dir, "sub1", "photo.jpg")
	fileB := filepath.Join(dir, "sub2", "photo.jpg")
	require.NoError(t, os.MkdirAll(filepath.Dir(fileA), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Dir(fileB), 0o755))
	require.NoError(t, os.WriteFile(fileA, []byte("X"), 0o644))
	require.NoError(t, os.WriteFile(fileB, []byte("Y"), 0o644))

	db, err := sqlite.Open(filepath.Join(dir, "test.db"))
	require.NoError(t, err)
	defer db.Close()
	_, err = db.Exec(`INSERT INTO albums(id, name) VALUES('al1', 'Trip')`)
	require.NoError(t, err)
	_, _ = db.Exec(`INSERT INTO assets(id, file_path, original_name, status) VALUES('a1', ?, 'photo.jpg', 'indexed')`, fileA)
	_, _ = db.Exec(`INSERT INTO assets(id, file_path, original_name, status) VALUES('a2', ?, 'photo.jpg', 'indexed')`, fileB)
	_, _ = db.Exec(`INSERT INTO album_assets(album_id, asset_id, position) VALUES('al1','a1',0)`)
	_, _ = db.Exec(`INSERT INTO album_assets(album_id, asset_id, position) VALUES('al1','a2',1)`)

	h := v1.NewAlbumsHandler(service.NewTestServices(db), "")
	req := httptest.NewRequest(http.MethodGet, "/v1/photos/albums/al1/export?token=t", nil)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues("al1")

	require.NoError(t, h.Export(c))

	zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	require.NoError(t, err)
	require.Len(t, zr.File, 2)
	names := []string{zr.File[0].Name, zr.File[1].Name}
	require.Contains(t, names, "photo.jpg")
	require.Contains(t, names, "photo-2.jpg")
}

// TestAlbumExportZipExcludesSoftDeleted 软删/离线成员不应出现在导出的 zip 里
// (复用 AlbumService.ListAssets 既有的可见性过滤,与网格读路径一致)。
func TestAlbumExportZipExcludesSoftDeleted(t *testing.T) {
	dir := t.TempDir()
	fileA := filepath.Join(dir, "alpha.jpg")
	fileDeleted := filepath.Join(dir, "gone.jpg")
	require.NoError(t, os.WriteFile(fileA, []byte("AAAA"), 0o644))
	require.NoError(t, os.WriteFile(fileDeleted, []byte("ZZZZ"), 0o644))

	db, err := sqlite.Open(filepath.Join(dir, "test.db"))
	require.NoError(t, err)
	defer db.Close()
	_, err = db.Exec(`INSERT INTO albums(id, name) VALUES('al1', 'Trip')`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO assets(id, file_path, original_name, status) VALUES('a1', ?, 'alpha.jpg', 'indexed')`, fileA)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO assets(id, file_path, original_name, status, deleted_at) VALUES('a2', ?, 'gone.jpg', 'indexed', CURRENT_TIMESTAMP)`, fileDeleted)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO album_assets(album_id, asset_id, position) VALUES('al1','a1',0)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO album_assets(album_id, asset_id, position) VALUES('al1','a2',1)`)
	require.NoError(t, err)

	h := v1.NewAlbumsHandler(service.NewTestServices(db), "")
	req := httptest.NewRequest(http.MethodGet, "/v1/photos/albums/al1/export?token=t", nil)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues("al1")

	require.NoError(t, h.Export(c))

	zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	require.NoError(t, err)
	require.Len(t, zr.File, 1)
	require.Equal(t, "alpha.jpg", zr.File[0].Name)
}
