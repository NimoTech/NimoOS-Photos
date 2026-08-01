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

// Tests for the manual album ZIP download endpoint. Mirrors favorites'
// Export: GET + token query parameter (browser window.location.href
// navigation can't send an Authorization header, so photos.go router's JWT
// Skipper lets the "/albums/:id/export" suffix through and the handler
// validates the token itself); when runtimePath=="" it's a test/standalone
// scenario, skipping real JWT validation (same convention as
// favorites_test.go's newFavHarness).

// TestAlbumExportZipHandler verifies the happy path: all members of the
// album are streamed into a zip, and Content-Type/Content-Disposition and
// file contents are all correct.
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

// TestAlbumExportZipNotFound expects 404 when the album doesn't exist.
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

// TestAlbumExportZipEmpty expects 400 when the album exists but has no
// (visible) members.
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

// TestAlbumExportZipMissingToken expects 401 when the token query
// parameter is missing (same validation as favorites: browser navigation
// can't carry an Authorization header, so it falls back to a query token).
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

// TestAlbumExportZipDeduplicatesNames: when two members share the same
// original_name, the zip entry names are deduplicated with a "-2" suffix
// per favorites' existing dedupZipEntryName rule.
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

// TestAlbumExportZipExcludesSoftDeleted: soft-deleted/offline members
// should not appear in the exported zip (reuses AlbumService.ListAssets'
// existing visibility filter, consistent with the grid read path).
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
