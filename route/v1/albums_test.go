package v1_test

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"

	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	v1 "github.com/NimoTech/NimoOS-Photos/route/v1"
	"github.com/NimoTech/NimoOS-Photos/service"
)

// newAlbumsTestEcho spins up an echo instance with only FromSmartView
// mounted, and returns the underlying db so tests can insert data directly
// into albums/smart_views/smart_view_matches.
func newAlbumsTestEcho(t *testing.T) (*echo.Echo, *sql.DB) {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "h.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	svc := service.NewTestServices(db)
	e := echo.New()
	g := e.Group("/v1/photos")
	h := v1.NewAlbumsHandler(svc, "")
	g.POST("/albums/from-smartview", h.FromSmartView)
	return e, db
}

// postJSON POSTs payload to the given path and returns the response.
func postJSON(t *testing.T, e *echo.Echo, path string, payload any) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

// TestFromSmartViewHTTPNotFound expects 404 when smartViewId doesn't exist.
func TestFromSmartViewHTTPNotFound(t *testing.T) {
	e, _ := newAlbumsTestEcho(t)
	rec := postJSON(t, e, "/v1/photos/albums/from-smartview", map[string]string{"smartViewId": "missing"})
	require.Equal(t, http.StatusNotFound, rec.Code)
}

// TestFromSmartViewHTTPBadRequest expects 400 when smartViewId is missing.
func TestFromSmartViewHTTPBadRequest(t *testing.T) {
	e, _ := newAlbumsTestEcho(t)
	rec := postJSON(t, e, "/v1/photos/albums/from-smartview", map[string]string{})
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

// TestFromSmartViewHTTPSuccess is the happy path: a smart view solidifies
// into a manual album and the original smart view disappears.
func TestFromSmartViewHTTPSuccess(t *testing.T) {
	e, db := newAlbumsTestEcho(t)
	_, err := db.Exec(`INSERT INTO smart_views(id,name,conds_raw,conds_parsed,threshold,live) VALUES('sv-h','H','[]','[]',70,1)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO assets(id,file_path,status,is_live_photo_video) VALUES('a1','/p/a1.jpg','indexed',0)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO smart_view_matches(smart_view_id,asset_id,match_score,origin) VALUES('sv-h','a1',1.0,1)`)
	require.NoError(t, err)

	rec := postJSON(t, e, "/v1/photos/albums/from-smartview", map[string]string{"smartViewId": "sv-h"})
	require.Equal(t, http.StatusOK, rec.Code)
	var album map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &album))
	require.Equal(t, "H", album["name"])

	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM smart_views WHERE id='sv-h'`).Scan(&n))
	require.Equal(t, 0, n, "original smart view should be deleted")
}

// TestFromSmartViewHTTPNameConflict expects 409 on a name conflict.
func TestFromSmartViewHTTPNameConflict(t *testing.T) {
	e, db := newAlbumsTestEcho(t)
	_, err := db.Exec(`INSERT INTO albums(id,name) VALUES('al-dup','Dup')`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO smart_views(id,name,conds_raw,conds_parsed,threshold,live) VALUES('sv-dup','Dup','[]','[]',70,1)`)
	require.NoError(t, err)

	rec := postJSON(t, e, "/v1/photos/albums/from-smartview", map[string]string{"smartViewId": "sv-dup"})
	require.Equal(t, http.StatusConflict, rec.Code)
}
