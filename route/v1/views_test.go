package v1_test

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	v1 "github.com/NimoTech/NimoOS-Photos/route/v1"
	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
)

type fakeViewsServices struct {
	service.Services
	views *service.ViewsService
}

func (f *fakeViewsServices) Views() *service.ViewsService { return f.views }

func newViewsHarness(t *testing.T) (*v1.ViewsHandler, func()) {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO assets(id, file_path, status) VALUES('a1','/x/a1.jpg','indexed')`)
	require.NoError(t, err)

	svc := &fakeViewsServices{views: service.NewViewsService(db)}
	return v1.NewViewsHandler(svc), func() { db.Close() }
}

func TestRecordViewHandlerSuccess(t *testing.T) {
	h, cleanup := newViewsHarness(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodPost, "/v1/photos/views/a1", nil)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	c.SetParamNames("asset_id")
	c.SetParamValues("a1")

	require.NoError(t, h.Record(c))
	require.Equal(t, http.StatusNoContent, rec.Code)
}

func TestRecordViewHandlerUnknownAsset(t *testing.T) {
	h, cleanup := newViewsHarness(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodPost, "/v1/photos/views/nope", nil)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	c.SetParamNames("asset_id")
	c.SetParamValues("nope")

	err := h.Record(c)
	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusNotFound, he.Code)
}
