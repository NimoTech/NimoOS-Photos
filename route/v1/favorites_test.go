package v1_test

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	v1 "github.com/NimoTech/NimoOS-Photos/route/v1"
	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
)

type fakeServices struct {
	service.Services
	favs *service.FavoritesService
}

func (f *fakeServices) Favorites() *service.FavoritesService { return f.favs }

func newFavHarness(t *testing.T) (*v1.FavoritesHandler, *fakeServices, func()) {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO assets(id, file_path, status) VALUES('a1','/x/a1.jpg','indexed')`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO assets(id, file_path, status) VALUES('a2','/x/a2.jpg','indexed')`)
	require.NoError(t, err)

	svc := &fakeServices{favs: service.NewFavoritesService(db)}
	h := v1.NewFavoritesHandler(svc, "/tmp/gallery", "")
	return h, svc, func() { db.Close() }
}

func TestFavoriteHandlerSuccess(t *testing.T) {
	h, _, cleanup := newFavHarness(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodPost, "/v1/photos/favorites/a1", nil)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	c.SetParamNames("asset_id")
	c.SetParamValues("a1")

	require.NoError(t, h.Favorite(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var resp struct {
		FavoritedAt string `json:"favoritedAt"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotEmpty(t, resp.FavoritedAt)
}

func TestFavoriteHandlerAssetNotFound(t *testing.T) {
	h, _, cleanup := newFavHarness(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodPost, "/v1/photos/favorites/nope", nil)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	c.SetParamNames("asset_id")
	c.SetParamValues("nope")

	err := h.Favorite(c)
	httpErr, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusNotFound, httpErr.Code)
}

func TestUnfavoriteHandlerIdempotent(t *testing.T) {
	h, _, cleanup := newFavHarness(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodDelete, "/v1/photos/favorites/nope", nil)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	c.SetParamNames("asset_id")
	c.SetParamValues("nope")

	require.NoError(t, h.Unfavorite(c))
	require.Equal(t, http.StatusNoContent, rec.Code)
}

func TestListIDsHandler(t *testing.T) {
	h, svcs, cleanup := newFavHarness(t)
	defer cleanup()

	_, _ = svcs.Favorites().Favorite("default", "a1")
	_, _ = svcs.Favorites().Favorite("default", "a2")

	req := httptest.NewRequest(http.MethodGet, "/v1/photos/favorites/ids", nil)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)

	require.NoError(t, h.ListIDs(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var ids []string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ids))
	require.ElementsMatch(t, []string{"a1", "a2"}, ids)
}

func TestListHandler(t *testing.T) {
	h, svcs, cleanup := newFavHarness(t)
	defer cleanup()

	_, _ = svcs.Favorites().Favorite("default", "a1")

	req := httptest.NewRequest(http.MethodGet, "/v1/photos/favorites?limit=10", nil)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)

	require.NoError(t, h.List(c))
	require.Equal(t, http.StatusOK, rec.Code)
	require.True(t, strings.Contains(rec.Body.String(), `"id":"a1"`))
	require.True(t, strings.Contains(rec.Body.String(), `"favoritedAt"`))
}

func TestExportZipHandler(t *testing.T) {
	dir := t.TempDir()
	fileA := filepath.Join(dir, "alpha.jpg")
	fileB := filepath.Join(dir, "beta.jpg")
	require.NoError(t, os.WriteFile(fileA, []byte("AAAA"), 0o644))
	require.NoError(t, os.WriteFile(fileB, []byte("BBBB"), 0o644))

	db, err := sqlite.Open(filepath.Join(dir, "test.db"))
	require.NoError(t, err)
	defer db.Close()
	_, err = db.Exec(`INSERT INTO assets(id, file_path, original_name, status) VALUES('a1', ?, 'alpha.jpg', 'indexed')`, fileA)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO assets(id, file_path, original_name, status) VALUES('a2', ?, 'beta.jpg', 'indexed')`, fileB)
	require.NoError(t, err)

	svc := &fakeServices{favs: service.NewFavoritesService(db)}
	_, _ = svc.favs.Favorite("default", "a1")
	_, _ = svc.favs.Favorite("default", "a2")

	h := v1.NewFavoritesHandler(svc, dir, "")

	req := httptest.NewRequest(http.MethodGet, "/v1/photos/favorites/export?token=t", nil)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)

	require.NoError(t, h.Export(c))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "application/zip", rec.Header().Get("Content-Type"))
	require.Contains(t, rec.Header().Get("Content-Disposition"), "favorites-")

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

func TestExportZipNoFavorites(t *testing.T) {
	h, _, cleanup := newFavHarness(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/v1/photos/favorites/export?token=t", nil)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)

	err := h.Export(c)
	httpErr, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusBadRequest, httpErr.Code)
}

func TestExportZipDeduplicatesNames(t *testing.T) {
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
	_, _ = db.Exec(`INSERT INTO assets(id, file_path, original_name, status) VALUES('a1', ?, 'photo.jpg', 'indexed')`, fileA)
	_, _ = db.Exec(`INSERT INTO assets(id, file_path, original_name, status) VALUES('a2', ?, 'photo.jpg', 'indexed')`, fileB)

	svc := &fakeServices{favs: service.NewFavoritesService(db)}
	_, _ = svc.favs.Favorite("default", "a1")
	_, _ = svc.favs.Favorite("default", "a2")

	h := v1.NewFavoritesHandler(svc, dir, "")
	req := httptest.NewRequest(http.MethodGet, "/v1/photos/favorites/export?token=t", nil)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)

	require.NoError(t, h.Export(c))

	zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	require.NoError(t, err)
	require.Len(t, zr.File, 2)
	names := []string{zr.File[0].Name, zr.File[1].Name}
	require.Contains(t, names, "photo.jpg")
	require.Contains(t, names, "photo-2.jpg")
}

func TestExportZipMissingToken(t *testing.T) {
	h, _, cleanup := newFavHarness(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/v1/photos/favorites/export", nil)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)

	err := h.Export(c)
	httpErr, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusUnauthorized, httpErr.Code)
}

func TestTopHandlerReturnsJSON(t *testing.T) {
	h, _, cleanup := newFavHarness(t)
	defer cleanup()

	// Favorite a1/a2 first, then request top.
	for _, id := range []string{"a1", "a2"} {
		req := httptest.NewRequest(http.MethodPost, "/v1/photos/favorites/"+id, nil)
		rec := httptest.NewRecorder()
		c := echo.New().NewContext(req, rec)
		c.SetParamNames("asset_id")
		c.SetParamValues(id)
		require.NoError(t, h.Favorite(c))
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/photos/favorites/top?limit=5", nil)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)

	require.NoError(t, h.Top(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var assets []map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &assets))
	require.Len(t, assets, 2)
}
