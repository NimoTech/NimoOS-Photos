package v1_test

import (
	"archive/zip"
	"bytes"
	"database/sql"
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

// Tests for the smart view (SmartView) ZIP download endpoint. The added
// GET+token endpoint fixes a real broken link: the UI's runExport('zip')
// uses window.location.href (browser navigation, which can't send an
// Authorization header) to hit the old POST /export?format=zip, and that
// path neither registers GET nor appears in router.go's mediaGetSkip
// allowlist, so the JWT middleware 401s it outright. The new endpoint
// mirrors albums_export_test.go's albums GET /:id/export implementation:
// same token validation, same service.ExportZip streaming, same 404/400
// semantics; the existing POST /export is left unchanged for backward
// compatibility.

// newSVExportTestEcho spins up an echo instance with only the smart-views
// route group mounted, and returns the underlying db so tests can insert
// data directly into smart_views / assets / smart_view_matches.
func newSVExportTestEcho(t *testing.T) (*echo.Echo, *sql.DB) {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "h.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	svc := service.NewTestServices(db)
	e := echo.New()
	g := e.Group("/v1/photos")
	h := v1.NewSmartViewsHandler(svc, "")
	v1.RegisterSmartViewRoutes(g, h)
	return e, db
}

// TestSVExportZipGetHandler verifies the happy path: GET + token streams
// the zip download directly, and Content-Type/Content-Disposition/file
// contents are all correct.
func TestSVExportZipGetHandler(t *testing.T) {
	dir := t.TempDir()
	fileA := filepath.Join(dir, "alpha.jpg")
	fileB := filepath.Join(dir, "beta.jpg")
	require.NoError(t, os.WriteFile(fileA, []byte("AAAA"), 0o644))
	require.NoError(t, os.WriteFile(fileB, []byte("BBBB"), 0o644))

	e, db := newSVExportTestEcho(t)
	_, err := db.Exec(`INSERT INTO smart_views(id,name,conds_raw,conds_parsed,threshold,live) VALUES('sv-1','V','[]','[]',70,0)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO assets(id,file_path,original_name,status,is_live_photo_video) VALUES('a1',?, 'alpha.jpg','indexed',0)`, fileA)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO assets(id,file_path,original_name,status,is_live_photo_video) VALUES('a2',?, 'beta.jpg','indexed',0)`, fileB)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO smart_view_matches(smart_view_id,asset_id,match_score,origin) VALUES('sv-1','a1',1.0,1)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO smart_view_matches(smart_view_id,asset_id,match_score,origin) VALUES('sv-1','a2',1.0,1)`)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/v1/photos/smart-views/sv-1/export?token=t", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "application/zip", rec.Header().Get("Content-Type"))
	require.Contains(t, rec.Header().Get("Content-Disposition"), "smartview-")

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

// TestSVExportZipGetNotFound expects 404 when the smart view doesn't exist.
func TestSVExportZipGetNotFound(t *testing.T) {
	e, _ := newSVExportTestEcho(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/photos/smart-views/sv-missing/export?token=t", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

// TestSVExportZipGetNoMatches expects 400 when the smart view exists but has no matched assets.
func TestSVExportZipGetNoMatches(t *testing.T) {
	e, db := newSVExportTestEcho(t)
	_, err := db.Exec(`INSERT INTO smart_views(id,name,conds_raw,conds_parsed,threshold,live) VALUES('sv-empty','Empty','[]','[]',70,0)`)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/v1/photos/smart-views/sv-empty/export?token=t", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

// TestSVExportZipGetMissingToken expects 401 when the token query
// parameter is missing (browser location.href navigation can't send an
// Authorization header, so it falls back to a query token, same as
// albums' Export).
func TestSVExportZipGetMissingToken(t *testing.T) {
	e, db := newSVExportTestEcho(t)
	_, err := db.Exec(`INSERT INTO smart_views(id,name,conds_raw,conds_parsed,threshold,live) VALUES('sv-1','V','[]','[]',70,0)`)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/v1/photos/smart-views/sv-1/export", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

// TestSVExportPostStillWorks: the existing POST /export?format=zip is left
// unchanged for backward compatibility (old callers/the fallback path
// before a future UI fix are both unaffected).
func TestSVExportPostStillWorks(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "gamma.jpg")
	require.NoError(t, os.WriteFile(file, []byte("GGGG"), 0o644))

	e, db := newSVExportTestEcho(t)
	_, err := db.Exec(`INSERT INTO smart_views(id,name,conds_raw,conds_parsed,threshold,live) VALUES('sv-1','V','[]','[]',70,0)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO assets(id,file_path,original_name,status,is_live_photo_video) VALUES('a1',?, 'gamma.jpg','indexed',0)`, file)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO smart_view_matches(smart_view_id,asset_id,match_score,origin) VALUES('sv-1','a1',1.0,1)`)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/v1/photos/smart-views/sv-1/export?format=zip", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "application/zip", rec.Header().Get("Content-Type"))
}
