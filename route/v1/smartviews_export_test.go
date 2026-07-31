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

// 智能相册(SmartView)ZIP 下载端点测试。补 GET+token 端点是为了修一个真实
// 断链:UI 的 runExport('zip') 用 window.location.href(浏览器导航,发不出
// Authorization 头)打旧的 POST /export?format=zip,而该路径既未注册 GET、
// 也不在 router.go 的 mediaGetSkip 白名单里,导致 JWT 中间件直接 401。
// 新端点镜像 albums_export_test.go 的 albums GET /:id/export 实现:同 token
// 校验、同 service.ExportZip 流式落地、同 404/400 语义;既有 POST /export
// 保持不动,向后兼容。

// newSVExportTestEcho 起一个只挂 smart-views 路由组的 echo 实例，返回底层 db
// 供用例直接插入 smart_views / assets / smart_view_matches 造数据。
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

// TestSVExportZipGetHandler 验证成功路径：GET + token 直接流式下载 zip，
// Content-Type/Content-Disposition/文件内容均正确。
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

// TestSVExportZipGetNotFound 智能相册不存在应 404。
func TestSVExportZipGetNotFound(t *testing.T) {
	e, _ := newSVExportTestEcho(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/photos/smart-views/sv-missing/export?token=t", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

// TestSVExportZipGetNoMatches 智能相册存在但无匹配资产应 400。
func TestSVExportZipGetNoMatches(t *testing.T) {
	e, db := newSVExportTestEcho(t)
	_, err := db.Exec(`INSERT INTO smart_views(id,name,conds_raw,conds_parsed,threshold,live) VALUES('sv-empty','Empty','[]','[]',70,0)`)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/v1/photos/smart-views/sv-empty/export?token=t", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

// TestSVExportZipGetMissingToken 缺 token query 参数应 401（浏览器 location.href
// 导航发不出 Authorization 头，只能靠 query token 兜底，与 albums 的 Export 同款）。
func TestSVExportZipGetMissingToken(t *testing.T) {
	e, db := newSVExportTestEcho(t)
	_, err := db.Exec(`INSERT INTO smart_views(id,name,conds_raw,conds_parsed,threshold,live) VALUES('sv-1','V','[]','[]',70,0)`)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/v1/photos/smart-views/sv-1/export", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

// TestSVExportPostStillWorks 既有 POST /export?format=zip 保持不动，向后兼容
// （旧调用方/未来 UI 修复前的兜底路径都不受影响）。
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
