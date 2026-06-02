package v1_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"

	v1 "github.com/NimoTech/NimoOS-Photos/route/v1"
	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
)

func newTestEcho(t *testing.T) *echo.Echo {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "h.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	svc := service.NewTestServices(db)
	e := echo.New()
	g := e.Group("/v1/photos")
	h := v1.NewSmartViewsHandler(svc)
	v1.RegisterSmartViewRoutes(g, h)
	return e
}

func TestSmartViewHTTPCreateAndList(t *testing.T) {
	e := newTestEcho(t)
	body, _ := json.Marshal(map[string]any{
		"id": "sv-h", "name": "HTTP", "condsRaw": []string{}, "threshold": 70, "live": true,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/photos/smart-views", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	req = httptest.NewRequest(http.MethodGet, "/v1/photos/smart-views", nil)
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var list []service.SmartView
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Len(t, list, 1)
	require.Equal(t, "HTTP", list[0].Name)
}
