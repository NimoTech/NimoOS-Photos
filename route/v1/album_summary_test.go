package v1

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
)

func TestAlbumSummaryHandler(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	_, err = db.Exec(`INSERT INTO albums(id, name) VALUES ('al1', 'Untitled')`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO assets(id, file_path, mime_type, original_name, taken_at, status)
		VALUES ('p1','/g/p1.jpg','image/jpeg','IMG_1.jpg','2024-04-02 10:00:00','indexed')`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO album_assets(album_id, asset_id) VALUES ('al1','p1')`)
	require.NoError(t, err)

	h := NewAlbumsHandler(service.NewTestServices(db), "")
	e := echo.New()
	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
	c.SetParamNames("id")
	c.SetParamValues("al1")

	require.NoError(t, h.Summary(c))
	body := rec.Body.String()
	require.Contains(t, body, `"assetCount":1`)
	require.Contains(t, body, `"dateStart":"2024-04-02"`)
	require.Contains(t, body, `"topPlaces":[]`)
	require.Contains(t, body, `"coverCandidates":["p1"]`)
}

func TestAlbumSummaryHandlerNotFound(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	h := NewAlbumsHandler(service.NewTestServices(db), "")
	e := echo.New()
	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
	c.SetParamNames("id")
	c.SetParamValues("nope")

	err = h.Summary(c)
	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusNotFound, he.Code)
}
